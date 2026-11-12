# ADR-0008: Antifraude: score por contadores de velocidade no Redis, e o Redis fora do ar

- **Status**: Aceito (Fase 3). Redis fora do ar: degradar o score; antifraude fora do ar: o `transfers` recusa o Pix;
  biblioteca `redis/go-redis` v9. Ajuste do autor: registrar o risco do `volatile-lru` sobre os contadores e expor a
  métrica de chaves descartadas.
- **Data**: 2026-11-12

## Contexto

O antifraude devolve uma decisão (aprovar ou recusar) e um score para cada Pix, a partir de **contadores de
velocidade**: quantos Pix e quanto dinheiro uma conta enviou na última hora. Os contadores precisam ser compartilhados
entre as réplicas do antifraude, então ficam no Redis (seção 4.3 do plano). O Redis **não é fonte da verdade**
(regra 9): perder os contadores degrada a análise, mas não perde dinheiro, porque o limite diário mora no Postgres do
`transfers` (ADR-0007, seção 6).

O antifraude também é o **alvo de testes de resiliência**: latência e taxa de erro configuráveis.

## 1. Score e decisão

`Evaluate(transfer_id, account_id, amount_cents)` devolve `decision` (`APPROVE`, `DENY`), `score` (0 a 100), os motivos
e `degraded` (verdadeiro quando o Redis não respondeu). Regras iniciais, didáticas, com limites por variável de
ambiente:

| Sinal | Pontos |
|---|---|
| Valor do Pix acima de R$ 1.000,00 | +20 |
| Quantidade de Pix na última hora (contando este): ≥ 5 / ≥ 10 | +20 / +40 |
| Soma na última hora (contando este) acima de R$ 2.000,00 / R$ 5.000,00 | +20 / +40 |

Recusa com score ≥ 60. A regra de negócio fica no antifraude; o `transfers` só lê a decisão.

## 2. Contadores de velocidade

**Janela deslizante aproximada (*sliding window counter*).** Dois contadores de janela fixa de 1 h (a atual e a
anterior), e a estimativa da última hora pondera a anterior pelo tempo que ainda falta para ela sair:

```
estimativa = atual + anterior × (1 − segundos_passados_na_janela_atual / 3600)
```

É o algoritmo usado em rate limiters de mercado: memória O(1) por conta e erro pequeno, contra um *sorted set* com
um item por Pix (exato, mas caro) ou uma janela fixa pura (que zera de uma vez e deixa passar o dobro na virada).

**Chaves**, todas com prefixo de uso, versão e expiração:

| Chave | Conteúdo | TTL |
|---|---|---|
| `af:v1:cnt:{account_id}:{janela}` | quantidade de Pix | 2 h (a janela e a seguinte) |
| `af:v1:sum:{account_id}:{janela}` | soma em centavos (`int64`) | 2 h |
| `af:v1:seen:{transfer_id}` | marca de "já contado" | 2 h |

**Atomicidade e idempotência num script Lua** (executado por `EVALSHA`):

1. `SET af:v1:seen:{transfer_id} 1 NX EX 7200`; se a chave já existia, **não incrementa**;
2. senão, `INCR` e `INCRBY` nos contadores da janela atual, com `EXPIRE`;
3. devolve os quatro contadores (atual e anterior).

O script roda inteiro, sem outro comando no meio. Ele é idempotente porque o `transfers` repete o `Evaluate` em caso
de timeout (*at-least-once*, mesmo numa chamada síncrona): sem a marca, um retry contaria o mesmo Pix duas vezes e
poderia recusar um cliente honesto. Os contadores contam **tentativas avaliadas**, inclusive as recusadas: tentar
muitas vezes também é sinal de fraude.

**Timeout** de 50 ms por comando (`ANTIFRAUDE_REDIS_TIMEOUT`); o pool tem tamanho fixo e prazo de espera curto.

## 3. Redis fora do ar: degradar o score

| | Aprovar (*fail-open*) | Recusar (*fail-closed*) | **Degradar** (escolhida) |
|---|---|---|---|
| Disponibilidade do Pix | Total | Nenhuma: o Redis vira ponto único de falha | Total para valores baixos |
| Risco de fraude | Só limitado pelo limite diário | Nenhum | Limitado pelo teto degradado e pelo limite diário |
| Coerência com "Redis pode ser perdido" | Sim | Não | Sim |

**Decisão: degradar.** Sem resposta do Redis (erro ou timeout de 50 ms), o antifraude decide só com as regras que não
dependem de histórico: aprova Pix de até R$ 500,00 (`ANTIFRAUDE_DEGRADED_MAX_AMOUNT_CENTS`) e recusa acima disso com o
motivo `VELOCITY_UNAVAILABLE`. A resposta sai com `degraded = true`, o span ganha `antifraude.degraded = true`, e a
métrica de decisões tem o rótulo `degraded`. O Pix não espera o Redis voltar.

Teste: Redis real em contêiner por trás do **Toxiproxy** (ADR-0003). O teste derruba o proxy (Redis fora do ar) e
injeta latência acima do timeout (Redis lento); nos dois casos a decisão degradada sai em menos de 100 ms.

## 4. Antifraude fora do ar (visto pelo `transfers`)

Se o antifraude não responde depois das tentativas (ou o circuito está aberto), o `transfers` **recusa** o Pix com
`DEPENDENCY_UNAVAILABLE`. Nada foi movido ainda, e o cliente pode tentar de novo. Aprovar sem antifraude seria
*fail-open* fora do controle do próprio antifraude.

## 5. Falhas injetáveis

Latência (`ANTIFRAUDE_FAULT_LATENCY`) e taxa de erro (`ANTIFRAUDE_FAULT_ERROR_RATE`, de 0 a 1, devolvendo
`UNAVAILABLE`) vêm de variáveis de ambiente e podem ser trocadas em tempo de execução pelo serviço administrativo gRPC
`AntifraudeAdminService.SetFaults` (contrato no `.proto`, chamado por `grpcurl` ou pelos testes). Os valores ativos
aparecem como métricas (`antifraude.fault.latency`, `antifraude.fault.error_rate`), para que um gráfico de latência
alta no `transfers` possa ser explicado no mesmo dashboard. O serviço administrativo fica na rede interna do compose e
nunca é exposto pelo BFF.

## 6. Configuração do Redis

- Imagem `redis:8.8.4-alpine`, fixada por digest; uma instância, sem cluster.
- `maxmemory 64mb` e **`maxmemory-policy volatile-lru`**: toda chave tem TTL, e o cache-aside da Fase 7 (que divide a
  instância) também será descartável por LRU.
- **Sem persistência** (`save ""`, `appendonly no`): reiniciar o Redis zera os contadores, o que é uma degradação
  curta e aceitável. Persistir seria tratar o Redis como fonte da verdade.
- Licença: o Redis 8 é distribuído sob AGPLv3 (entre outras opções). Para um projeto didático, local e sem
  distribuição, não há impacto. O Valkey é o *fork* compatível, caso isso mude.

## 7. Biblioteca cliente em Go

Manutenção conferida em 2026-11-12, no proxy de módulos do Go e nos repositórios:

| | **`redis/go-redis` v9** (escolhida) | `redis/rueidis` | `gomodule/redigo` |
|---|---|---|---|
| Último release | v9.23.0, 2026-10-05 | v1.0.79, 2026-10-15 | v1.9.3, 2025-10-08 (um ano sem release) |
| Mantenedor | Redis (oficial) | Redis (oficial) | Comunidade |
| OpenTelemetry | `extra/redisotel` (traces e métricas), mesma versão | `rueidisotel` | Não tem (só auto-instrumentação de terceiros) |
| API | Idiomática, um método por comando; `redis.Script` com `EVALSHA` e *fallback* para `EVAL` | *Builder* (`b.Set().Key()...`), menos familiar | Baixo nível (`Do("SET", ...)`), respostas sem tipo |
| Diferencial | É a mais usada; a documentação da Redis usa ela | Auto-pipelining e cache no cliente | Nenhum relevante aqui |
| Go mínimo | 1.26 (temos 1.27.4) | — | — |

**Decisão: `go-redis` v9 com `redisotel`.** O ganho de desempenho do `rueidis` não importa no volume do projeto, e o
cache no cliente dele seria útil só para o cache da Fase 7, que mora no BFF (Node).

## Consequências

- **O `volatile-lru` pode descartar contadores do antifraude** quando a memória apertar, e isso piora quando o cache
  da Fase 7 passar a dividir a instância: um contador descartado faz o antifraude **subestimar** a velocidade de uma
  conta (risco de fraude, não de dinheiro, porque o limite diário continua valendo). Mitigações: `maxmemory` com folga
  (os contadores ocupam poucos bytes por conta) e a **métrica de chaves descartadas** (`redis_keys_evicted_total`, lida
  pelo receiver `redis` do OpenTelemetry Collector, que já está na distribuição *contrib* que usamos), com a memória
  usada, no dashboard do antifraude. Se o descarte aparecer, a Fase 7 decide entre separar as instâncias ou mudar a
  política (registrado aqui para essa decisão).
- **Métricas**: decisões por `decision` e `degraded`, score (histograma), latência dos comandos do Redis (via
  `redisotel`), falhas injetadas ativas, chaves descartadas e memória usada (via Collector).
- A janela deslizante é uma aproximação: num limite exato, a estimativa pode errar alguns por cento. É aceitável para
  um score, não seria para um limite financeiro (que está no Postgres).
- Um Redis reiniciado zera a velocidade de todas as contas por até 1 h.
