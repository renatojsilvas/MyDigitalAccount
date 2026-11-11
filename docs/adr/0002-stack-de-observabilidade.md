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
- Como cada sinal chega ao seu backend (por exemplo, métricas por `remote_write` do Collector para o Prometheus ou por scrape de um exporter do Collector; logs pelo endpoint OTLP do Loki) será **conferido na documentação das versões fixadas** antes de configurar, no passo da stack.

### Instrumentação no BFF (Node), decidida no passo 5

- **Sem auto-instrumentação e sem globais.** Os providers do OpenTelemetry são criados no composition root e o contexto de trace trafega de forma explícita pela requisição (`request.telemetry.context`), passando ao cliente gRPC por `contextValues`. A auto-instrumentação do Node (`--import`, monkey-patch de módulos) exigiria um contexto assíncrono global e contraria "sem singletons importados diretamente". O preço é escrever o span de servidor e o de cliente à mão (um plugin do Fastify e um interceptor do Connect), o que também ensina o mecanismo.
- **Pacotes da geração 3.x do SDK** (`@opentelemetry/sdk-trace`, `sdk-metrics`, `sdk-logs` 3.1.1, exporters 0.301.1, `api` 1.10.0). O `sdk-trace-node` 2.x está deprecated e exige `api <1.10`; misturar gerações quebra os pares.
- **Logs**: o `pino` escreve JSON no stdout e um stream próprio reenvia cada linha ao OpenTelemetry, com `trace_id` e `span_id` como contexto do registro.
- **`/readyz` do BFF não consulta o ledger**: o BFF degrada de forma parcial (fase 2); tirá-lo do balanceador por causa de uma dependência seria pior que responder algo útil.
- **Deadline**: `timeoutMs` do Connect vira o header `Grpc-Timeout` (verificado na rede em teste), que o servidor Go enxerga como prazo do contexto.

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
