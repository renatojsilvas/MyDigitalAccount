# ADR-0002: Stack de observabilidade

- **Status**: Aceito (Fase 0)
- **Data**: 2026-11-11

## Contexto

Observabilidade faz parte do pronto de todas as fases (regra 7). Precisamos de logs, métricas e traces **correlacionados** (do log ao trace, do trace ao log), exportados de forma que trocar um backend não exija mexer no código dos serviços.

## Decisão

**OpenTelemetry nos serviços, OpenTelemetry Collector no meio, e backends da família Grafana.**

```
serviços ──OTLP──> Collector ──> Prometheus (métricas)
                             ──> Tempo      (traces)
                             ──> Loki       (logs)
                 Grafana lê os três (datasources provisionados como código)
```

| Peça | Papel | Por que ela |
|---|---|---|
| **OpenTelemetry (SDK + OTLP)** | Único protocolo de saída dos serviços, em Go e em TS | Padrão neutro de mercado. O código não conhece Prometheus, Tempo nem Loki. |
| **Collector** | Recebe OTLP e distribui, com `batch` e `memory_limiter` | Ponto único para trocar backend, amostrar, filtrar dado sensível e aplicar retry, sem rebuild dos serviços. |
| **Prometheus** | Métricas, regras de alerta e de gravação | Padrão de fato; PromQL é a linguagem dos SLOs da fase 6. |
| **Tempo** | Traces | Armazena por ID, barato, integra com Grafana e com exemplars das métricas. |
| **Loki** | Logs | Indexa só labels, não o texto: leve para rodar local. Correlaciona com o trace por `trace_id`. |
| **Grafana** | Visualização | Lê os três backends; dashboards e datasources em `deploy/grafana/`, sem configurar na interface. |
| **Alertmanager** | Roteamento de alertas | Entra na fase 6, não na fase 0. |

### Convenções fixadas aqui (detalhe na seção 7 do plano)

- Contexto W3C `traceparent` em toda chamada. Todo log leva `trace_id` e `span_id`.
- `service.name` e `deployment.environment` como atributos de recurso. Esses viram labels; ID de conta, transação ou usuário nunca viram label de métrica.
- Logs em JSON no stdout e **também** exportados por OTLP; o stdout continua útil para `docker compose logs`.
- `/healthz` (processo vivo) e `/readyz` (dependências prontas) em todo serviço, fora do tracing de negócio. Nos serviços Go, ficam numa **porta admin HTTP separada (`:8081`)**, e o gRPC de negócio usa `:50051` (`:9090` é a porta padrão do Prometheus e é evitada). Os serviços Go também expõem `grpc.health.v1`. No BFF, as rotas ficam na porta do Fastify, fora dos hooks de negócio.
- **Cliente gRPC do BFF (Connect)**: a instrumentação HTTP do OpenTelemetry não cobre HTTP/2, então um interceptor próprio cria o span de cliente (atributos `rpc.*` e status gRPC), registra a métrica de latência e erro por dependência e injeta o `traceparent`.
- Caminho de cada sinal (decidido e verificado no passo 8, ver seção abaixo): traces por OTLP gRPC ao Tempo, métricas por **remote write** ao Prometheus e logs pelo endpoint OTLP nativo do Loki (`/otlp`).

### Instrumentação no BFF (Node), decidida no passo 5

- **Sem auto-instrumentação e sem globais.** Os providers do OpenTelemetry são criados no composition root e o contexto de trace trafega de forma explícita pela requisição (`request.telemetry.context`), passando ao cliente gRPC por `contextValues`. A auto-instrumentação do Node (`--import`, monkey-patch de módulos) exigiria um contexto assíncrono global e contraria "sem singletons importados diretamente". O preço é escrever o span de servidor e o de cliente à mão (um plugin do Fastify e um interceptor do Connect), o que também ensina o mecanismo.
- **Pacotes da geração 3.x do SDK** (`@opentelemetry/sdk-trace`, `sdk-metrics`, `sdk-logs` 3.1.1, exporters 0.301.1, `api` 1.10.0). O `sdk-trace-node` 2.x está deprecated e exige `api <1.10`; misturar gerações quebra os pares.
- **Logs**: o `pino` escreve JSON no stdout e um stream próprio reenvia cada linha ao OpenTelemetry, com `trace_id` e `span_id` como contexto do registro.
- **`/readyz` do BFF não consulta o ledger**: o BFF degrada de forma parcial (fase 2); tirá-lo do balanceador por causa de uma dependência seria pior que responder algo útil.
- **Deadline**: `timeoutMs` do Connect vira o header `Grpc-Timeout` (verificado na rede em teste), que o servidor Go enxerga como prazo do contexto.

### A stack no compose, decidida no passo 8

- **Versões** (tag + digest, conferidas nas fontes oficiais): Collector contrib 0.164.0, Prometheus 3.15.1, Tempo 3.1.1, Loki 3.7.9, Grafana 13.2.5. Preferi lançamentos com algumas semanas; o mais novo de cada um tinha dias.
- **Métricas por remote write, não por scrape nem pelo receptor OTLP do Prometheus**: o Collector continua o único hub, e o Prometheus mantém o contrato clássico (`--web.enable-remote-write-receiver`). O `job` da série vem de `service.name`; os rótulos de recurso não são promovidos a rótulo de métrica, para o conjunto de rótulos ficar pequeno. Os alvos raspados são só a própria stack (Collector, Tempo, Loki, Grafana, Prometheus).
- **Nomes de componente do Collector 0.164**: `otlp_grpc`, `otlp_http` e `prometheus_remote_write`, e não `otlp`, `otlphttp` e `prometheusremotewrite` como em versões anteriores. A configuração é validada com `otelcol-contrib validate`.
- **Tempo 3.x em modo monolítico (`-target=all`) com disco local, sem Kafka**: confirmado no exemplo oficial `single-binary` da versão 3.1.1. O Tempo 3.0 passou a exigir Kafka só na arquitetura distribuída. O compactor foi removido, e a retenção de blocos fica em `overrides.defaults.compaction.block_retention` (48 h). A chave é aceita pelo `-config.verify`, mas o **efeito** da retenção não foi observado, porque exigiria esperar 48 h.
- **Loki em instância única**, com `allow_structured_metadata: true` (o OTLP leva `trace_id` e `span_id` como metadados estruturados) e retenção de 72 h. O Prometheus guarda 7 dias.
- **Healthcheck em imagens sem shell**: Collector, Tempo e Loki não têm sh, wget nem curl. `deploy/obs/Dockerfile` copia um busybox estático (musl, 1,3 MB) para cada uma, e o healthcheck chama `/busybox wget`. Prometheus e Grafana já têm `wget`.
- **Grafana sem login, com papel de administrador, só em `127.0.0.1`** (`GF_AUTH_ANONYMOUS_*`): é uma decisão de desenvolvimento local, para ninguém configurar nada à mão. Datasources e dashboards são provisionados como código e ficam somente leitura. Não serve para nenhum ambiente compartilhado.
- **Limites de memória** (`mem_limit`): Collector 256 MB (com `memory_limiter` em 192 MB), Prometheus 512 MB, Tempo 512 MB, Loki 512 MB e Grafana 512 MB. Uso medido com a stack ociosa: cerca de 565 MB no total; o Grafana chegou a 293 de 384 MB, por isso subiu para 512 MB.
- **Ligações entre sinais**: o Tempo leva do span aos logs (consulta por `trace_id` em metadado estruturado) e às métricas; o Loki leva do `trace_id` ao Tempo. As consultas estão provisionadas; o salto de ponta a ponta com dados reais é validado no passo 9.
- **Verificação**: `deploy/obs/smoke.sh` (`docker compose run --rm tools make obs-smoke`) envia um trace, uma métrica e um log por OTLP e confere a chegada em Tempo, Prometheus e Loki e a saúde dos três datasources.

### Os serviços ligados à stack, decidido no passo 9

- **A exportação é opt-in**: os serviços só exportam se `OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317` estiver definido (no `.env` ou na linha de comando). Com ele definido e sem o Collector no ar, o export falha com uma linha de erro por lote; por isso o padrão é desligado, e o stdout em JSON continua sendo a reserva.
- **`service.instance.id`** (hostname do contêiner) em todo recurso. No Prometheus, o Collector o transforma no rótulo `instance`; sem ele, duas réplicas escreveriam séries idênticas (a Fase 7 escala réplicas). Há teste nos dois serviços.
- **Os nomes das métricas chegam diferentes por serviço**, porque cada SDK segue uma versão da convenção semântica: o BFF produz `rpc_grpc_status_code` numérico (`"3"`), enquanto o `otelgrpc` do ledger produz `rpc_response_status_code` por nome (`"INVALID_ARGUMENT"`). O dashboard trata cada um no seu lado. Unificar fica como pendência, de preferência quando o `otelgrpc` e o BFF convergirem na mesma versão da convenção.
- **Dashboard inicial** (`deploy/grafana/dashboards/visao-geral.json`): stack de observabilidade, RED do BFF, dependência ledger vista pelo BFF, RED do ledger, recursos (USE), logs e traces. Todas as 15 consultas PromQL foram executadas contra o Prometheus com dados reais antes de fechar o passo. A conferência **visual** no navegador não foi feita na sessão (a extensão do Chrome não estava conectada).
- **Critério de pronto verificável por comando**: `deploy/obs/e2e-trace.sh` (`make obs-e2e`) manda um `traceparent` conhecido ao BFF e confere no Tempo que há um único trace com `bff (servidor) -> bff (cliente gRPC) -> ledger (servidor)` encadeados, e no Loki que os dois serviços logaram com o mesmo `trace_id`. Foi verificado nos dois sentidos: falha, com instrução, sem exportação; passa com ela.

## Alternativas consideradas

| Alternativa | Por que não |
|---|---|
| **Serviços exportando direto para cada backend**, sem Collector | Acopla o código a backends e espalha configuração de retry e amostragem por todos os serviços. |
| **Prometheus fazendo scrape de `/metrics` em cada serviço** | Funciona e é o modelo clássico, mas dois protocolos de saída (scrape para métricas, OTLP para o resto) contrariam "tudo por OTLP". |
| **Jaeger** (traces) | Bom, mas Tempo integra melhor ao Grafana e ao Loki e o armazenamento é mais simples para rodar localmente. |
| **ELK/OpenSearch** (logs) | Indexação de texto completo é mais pesada, e a RAM é recurso escasso nesta stack (Kafka vem depois). |
| **Grafana Alloy no lugar do Collector** | Válido e cada vez mais comum, mas o Collector é o artefato oficial do OpenTelemetry e o que se encontra na maioria das empresas; o que se aprende é transferível. |
| **SaaS (Datadog, New Relic)** | Fora do escopo: tudo roda local, em contêiner, sem conta externa. |

## Consequências

- (+) Trocar um backend é mudança de configuração do Collector.
- (+) Um único caminho de instrumentação nas duas linguagens.
- (−) Mais peças no compose (cinco serviços de observabilidade). Por isso ficam no profile `obs`, separado do `core`.
- (−) O SDK de logs do OpenTelemetry é o sinal menos maduro dos três em algumas linguagens. Mitigação: o stdout em JSON continua como caminho de reserva, e conferimos o estado atual do SDK de cada linguagem antes de implementar.
- (−) A pilha consome memória. Mitigação: limites no compose e `memory_limiter` no Collector.
