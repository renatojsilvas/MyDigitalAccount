# Estado do projeto

Atualizado ao fim da **Fase 3 (Transfers e antifraude)**, em 2026-11-12. Leia este arquivo no início de cada fase.
Referências: `docs/plano-conta-digital.md` (plano), `docs/arquitetura-conta-digital.md` (diagrama),
`docs/adr/` (decisões), `load/README.md` (gate de desempenho).

## 1. Fases

| Fase | Estado |
|---|---|
| 0. Fundação | **Concluída** |
| 1. Ledger (Go) | **Concluída** |
| 2. BFF "home" (TS) | **Concluída** |
| 3. Transfers e antifraude (Go) | **Concluída** |
| 4. Eventos e tempo real | Próxima |
| 5 a 8 | Pendentes |

**Critério de pronto da Fase 3**, verificado nos testes e na stack real pelo script `make pix-e2e`
(`deploy/obs/e2e-pix.sh`, que também roda no job de ponta a ponta do CI):

1. **Toda recusa explícita na liquidação termina em estorno**: `TestSettlementRejectedEndsInReversal` e o caso "recusa
   confirmada na consulta" (`services/transfers/internal/saga`); na stack, com o spi-fake em `REJECT`, o Pix termina
   `failed / reversed / settlement_rejected` e o saldo volta.
2. **Um timeout na liquidação nunca estorna nem conclui sem confirmação**: `TestSettlementOutcomes` (inclusive
   "timeout e depois circuito aberto: pendente, nunca estorno"), `TestConfirmation` (timeout sem liquidação),
   `TestDeadlineExceededStaysPending`, e o teste de rede real com o Toxiproxy (`TestSPIThroughNetworkFailures`). Na
   stack, com o spi-fake em `NO_SETTLE_NO_RESPONSE`, o Pix fica `processing / pending_confirmation` com o débito; um Pix
   assim passou do prazo de 10 min e ficou `confirmation_overdue = true`, sem estorno.
3. **Uma requisição repetida não gera débito duplicado**: `TestConcurrentAcceptSameKey`, `TestRepeatedRequestDebitsOnce`,
   `TestCrashAfterDebitNeverDebitsTwice`, `TestLedgerUnavailableOnDebitRetriesWithSameKey`; na stack, a mesma
   `Idempotency-Key` devolve o mesmo Pix e o saldo cai uma vez só.
4. **Um único trace mostra o Pix passando por BFF, transfers, antifraude, ledger e spi-fake**: na stack, o `x-trace-id`
   do `POST /pix` leva, no Tempo, a um trace com os cinco serviços (`POST /pix` → `StartPix` → `pix.saga.*` →
   `Evaluate`, `GetAccountByDocument`, `Debit`, `Settle`, `Credit`). Testes de unidade: o `traceparent` do BFF chega ao
   transfers (`bff/test/pix.test.ts`), o aceite grava o contexto (`grpcapi_test.go`) e cada etapa é filha dele
   (`sagaotel_test.go`).

`make lint test`: todos os módulos Go com `-race` (ledger, antifraude, spi-fake, transfers, platform, testkit), BFF com
141 testes Vitest, web com 15.

Os critérios das Fases 0 a 2 continuam valendo (`make obs-e2e`, testes do ledger e da home); ver o histórico deste
arquivo no Git para o detalhe de como foram verificados.

## 2. O que existe

- **ledger** (Go, Fase 1): partida dobrada, saldo sob lock pessimista, idempotência na mesma transação, extrato por
  cursor. Na Fase 3 ganhou:
  - **`Reverse`**: estorno como lançamento novo com as pernas invertidas, apontando o original; só débitos `EXTERNAL`,
    uma vez só (chave, checagem sob o lock da conta e `UNIQUE` em `reverses_entry_id`), ADR-0009;
  - **`external_reference`** em `Credit`, `Debit`, `Reverse` e `Posting` (o `transfer_id`, para a conciliação);
  - **motivo do lançamento no `Posting`** (`EntryReason`: `OPENING`, `EXTERNAL`, `REVERSAL`).
- **antifraude** (Go, Fase 3, ADR-0008): score por contadores de velocidade no Redis (janela deslizante aproximada,
  script Lua idempotente por `transfer_id`, só inteiros); com o Redis fora do ar ou lento (50 ms), decisão degradada
  (aprova até R$ 500); falhas injetáveis (latência, taxa de erro) por ambiente e por `AntifraudeAdminService`. O
  `/readyz` não depende do Redis.
- **spi-fake** (Go, Fase 3): liquidação idempotente pelo `settlement_id`, resultado imutável (Postgres, banco `spi`);
  modos `settle`, `reject`, `settle_no_response`, `no_settle_no_response`, falha da consulta de status e latência, por
  ambiente e por `SpiAdminService`. Os modos "sem responder" seguram a chamada até o prazo do cliente (teto de 30 s).
- **transfers** (Go, Fase 3, ADR-0007): orquestrador do Pix e dono do limite diário.
  - Aceite idempotente (`StartPix` responde já, em `ACCEPTED`); tabela de idempotência só com HMAC do pedido.
  - Máquina de estados com 10 estados e transições explícitas; histórico (`GetPix`).
  - Runner da saga: fila sobre o Postgres (`FOR UPDATE SKIP LOCKED`), lease com fencing token, aviso imediato e
    varredura; fan-out com `errgroup` (antifraude, reserva do limite, destino); débito, liquidação, crédito no destino
    e estorno com chave por etapa.
  - Liquidação: 3 tentativas com o mesmo `settlement_id`; marcador `settlement_maybe_sent` gravado antes de cada uma
    (trigger impede voltar a falso); só marcador falso prova "não enviado".
  - Worker de confirmação (pool próprio): consulta de status com intervalo crescente (2 s → 30 s, equal jitter);
    prazo máximo de 10 min, depois pendente e `overdue`.
  - Retry com backoff exponencial e full jitter; circuit breaker por dependência (`gobreaker/v2`, dois passos).
  - Limite diário (R$ 2.000) com reserva atômica e devolução ao dia da reserva; dia em `America/Sao_Paulo` (base de
    fusos embutida no binário).
  - Aquecimento das conexões no boot.
- **platform** (módulo Go `conta.local/platform`, ADR-0010): `telemetry`, `logging`, `admin`, `grpcserver` (com o
  `PingService`), `grpcclient` (com `WarmUp`), `healthcheck` e `pg` (pool com `otelpgx`, `Ping`, `Migrate`), usados
  pelos quatro serviços Go.
- **bff** (TS, Fase 2): na Fase 3 ganhou `POST /pix` (exige `Idempotency-Key`, repassa intacto, 202 com `Location`),
  `GET /pix/{pixId}` (com histórico) e a seção opcional `limits` na `/home` (degradação parcial, prazo de 200 ms);
  adaptador `clients/transfers.ts`; aquecimento de ledger e transfers no boot.
- **web** (React): a Home mostra o limite diário de Pix (ou um aviso de indisponibilidade).
- **Infra**:
  - profile `core`: Postgres, `postgres-bootstrap` (job), `ledger-migrate`, `spi-migrate`, `transfers-migrate`,
    ledger, antifraude, spi-fake, transfers, **redis**, bff, web;
  - profile `obs` (o Collector agora lê o Redis), `load` (k6);
  - profiles `tools`/`test`: Postgres de teste, **redis-test**, **toxiproxy**.
- **Não existe ainda**: Kafka, outbox, notifications, reconciler, telas Novo Pix e Acompanhar Pix (Fase 4), SSE,
  relatório de liquidações do spi-fake (Fase 5), Alertmanager e runbooks (Fase 6).

## 3. Árvore de arquivos (versionados)

Só o que mudou ou nasceu nas Fases 1 a 3; o resto segue como na Fase 0 (ver o histórico deste arquivo no Git).

**Fase 3:**

```
proto/conta/transfers/v1/transfers.proto   TransfersService (StartPix, GetPix, GetLimits)
proto/conta/antifraude/v1/antifraude.proto AntifraudeService (Evaluate) e AntifraudeAdminService (SetFaults, GetFaults)
proto/conta/spi/v1/spi.proto               SpiService (Settle, GetSettlement) e SpiAdminService (SetBehavior, GetBehavior)
proto/conta/ledger/v1/ledger.proto         + Reverse, external_reference, EntryReason, ErrorReasons de estorno
platform/                                  módulo compartilhado (ADR-0010): admin, grpcserver, grpcclient, healthcheck,
                                             logging, telemetry, pg (saíram de services/ledger/internal/platform)
services/ledger/migrations/00002_*.sql     motivo, referência externa e reverses_entry_id nos lançamentos
services/ledger/internal/account/          + reference.go, Reverse (account.go, service.go), reverse_test.go
services/antifraude/
  cmd/antifraude/                          composition root e subcomando healthcheck
  internal/risk/                           domínio: regras, janela deslizante, caso de uso Evaluate (portas)
  internal/redisvelocity/                  script Lua (record.lua) e cliente go-redis com prazos curtos
  internal/faults/                         latência e taxa de erro injetadas
  internal/riskotel/, internal/grpcapi/, internal/platform/config/
services/spi-fake/
  cmd/spi-fake/                            composition root, subcomandos migrate e healthcheck
  migrations/00001_spi_schema.sql          settlements (resultado imutável)
  internal/settlement/                     domínio: modos, Switch, casos de uso Settle e Get (portas)
  internal/postgres/, internal/grpcapi/, internal/settlementotel/, internal/platform/config/
services/transfers/
  cmd/transfers/                           composition root (runner e worker), subcomandos migrate e healthcheck
  migrations/00001..00003_*.sql            transfers, histórico, idempotência, limite; motivo novo; confirmação
  internal/pix/                            domínio: estados e transições, chave Pix, dia do limite, aceite e limites
  internal/saga/                           runner, etapas, worker de confirmação (portas Store, Antifraud, Ledger, SPI)
  internal/resilience/                     retry com jitter, circuit breaker, classificação de erros gRPC
  internal/clients/                        adaptadores gRPC de antifraude, ledger e SPI
  internal/postgres/                       Store (aceite, lease, reserva, transição, consultas de pendentes)
  internal/grpcapi/, internal/pixotel/, internal/sagaotel/, internal/platform/config/
testkit/toxi/                              cliente da API do Toxiproxy (proxy por teste, queda, latência)
testkit/pgbootstrap/                       testes do script de bootstrap dos bancos
deploy/postgres/bootstrap.sh               job postgres-bootstrap (substitui deploy/postgres/init/01-ledger.sh)
deploy/obs/e2e-pix.sh                      critério de pronto da Fase 3 na stack (make pix-e2e)
deploy/grafana/dashboards/                 + antifraude.json, spi-fake.json, transfers.json; bff.json com o transfers
bff/src/clients/transfers.ts               adaptador do transfers
bff/src/modules/pix/                       schema, mapper, service, route (POST /pix, GET /pix/{pixId})
bff/src/money.ts                           centsToNumber (compartilhado)
bff/test/fake-transfers.ts, pix.test.ts    transfers gRPC falso e testes do Pix
docs/adr/0007..0010                        saga e timeout; antifraude e Redis; estorno; módulo platform
```

**Fase 2:**

```
proto/google/rpc/                          error_details.proto e status.proto do googleapis (commit fixado, README.md);
                                             geram SÓ TypeScript; fora do buf lint/format/breaking
buf.gen.yaml / buf.gen.ts.yaml             Go (exclui proto/google) e TypeScript (tudo), dois modelos
bff/src/
  config.ts                                + BFF_JWT_SECRET, BFF_JWT_TTL_SECONDS, BFF_DEMO_LOGIN, prazos da home
  clients/ledger.ts                        adaptador do ledger: único que conhece o gRPC dele
  errors.ts, problem-schema.ts             DependencyError, UnauthenticatedError, BffError; Problem (RFC 9457)
  modules/home/                            schema (Zod), mapper, service (allSettled), observe, route
  modules/auth/                            schema, service, route (POST /auth/demo-login)
  plugins/auth.ts, openapi.ts, telemetry.ts
bff/scripts/generate-openapi.ts            grava gen/openapi/bff.json
web/                                       React + Vite; src/api/client.ts, src/screens/{Login,Home}, src/money.ts;
                                             src/api/schema.d.ts é GERADO (fora do Git)
deploy/grafana/dashboards/bff.json         dashboard "Conta Digital: BFF"
docs/adr/0005, 0006
```

**Fase 1:**

```
services/ledger/
  sqlc.yaml, migrations/ (00001), cmd/ledger/, cmd/seed/
  internal/account/                        domínio e casos de uso
  internal/accountotel/, internal/postgres/, internal/grpcapi/
  internal/platform/postgres/              hoje só os testes do schema e pgtest/ (o pool foi para platform/pg)
testkit/testpg/                            banco isolado por teste, EnsureRole, ConnectAs
deploy/compose/load.yaml, load/            k6 e o gate de desempenho
deploy/grafana/dashboards/ledger.json
docs/adr/0004
```

## 4. Contratos `.proto`

Todos com erros como status gRPC + `google.rpc.ErrorInfo` (`reason` = nome do `ErrorReason`, `domain` =
`<serviço>.conta.local`, `metadata.field` nos erros de validação). Valores em centavos, `int64`, sempre positivos.

`conta.ping.v1.PingService.Ping`: inalterado; registrado nos quatro serviços Go.

`conta.ledger.v1.LedgerService` (Fase 3: `Reverse` e campos novos, tudo aditivo):

```proto
rpc OpenAccount / GetAccount / GetAccountByDocument / Credit / Debit / GetBalance / ListStatement  (como na Fase 2)
rpc Reverse(ReverseRequest) returns (ReverseResponse);  // idempotency_key, entry_id, external_reference, description
CreditRequest / DebitRequest: + external_reference (6)
Posting: + reason (EntryReason, 8), external_reference (9), reverses_entry_id (10)
enum EntryReason { UNSPECIFIED; OPENING = 1; EXTERNAL = 2; REVERSAL = 3; }
enum ErrorReason { ...; ENTRY_NOT_FOUND = 6; ENTRY_ALREADY_REVERSED = 7; ENTRY_NOT_REVERSIBLE = 8; }
```

`conta.transfers.v1.TransfersService`:

```proto
rpc StartPix(idempotency_key, account_id, destination_key{type CPF, value}, amount_cents, description) -> Pix (ACCEPTED)
rpc GetPix(account_id, pix_id) -> Pix com history       // NOT_FOUND (PIX_NOT_FOUND) se for de outra conta
rpc GetLimits(account_id) -> daily_limit_cents, used_today_cents, available_today_cents, resets_at
Pix: pix_id, source/destination_account_id, destination_key_masked, amount_cents, description, state (PixState),
     status (PixStatus), reason (PixReason), confirmation_overdue, created_at, updated_at, history
enum PixState  { ACCEPTED, VALIDATED, DEBITED, SETTLING, PENDING_CONFIRMATION, SETTLED, COMPLETED, REVERSING,
                 REVERSED, REJECTED }
enum PixStatus { PROCESSING, COMPLETED, FAILED, REJECTED }     // visão do cliente, derivada no transfers
enum PixReason { ANTIFRAUD_DENIED, LIMIT_EXCEEDED, DESTINATION_INVALID, SAME_ACCOUNT, INSUFFICIENT_FUNDS,
                 DEPENDENCY_UNAVAILABLE, SETTLEMENT_REJECTED, SETTLEMENT_NOT_SENT, SOURCE_ACCOUNT_INVALID }
enum ErrorReason { INVALID_REQUEST = 1; IDEMPOTENCY_KEY_REUSED = 2; PIX_NOT_FOUND = 3; }
```

`conta.antifraude.v1`:

```proto
AntifraudeService.Evaluate(transfer_id, account_id, amount_cents) -> decision (APPROVE|DENY), score, signals, degraded
AntifraudeAdminService.SetFaults / GetFaults(Faults{latency, error_rate})
```

`conta.spi.v1`:

```proto
SpiService.Settle(settlement_id, transfer_id, payer, payee, amount_cents) -> Settlement (SETTLED|REJECTED)
SpiService.GetSettlement(settlement_id)       // NOT_FOUND (SETTLEMENT_NOT_FOUND) não prova que não liquidou
SpiAdminService.SetBehavior / GetBehavior(Behavior{settle_mode, fail_status_queries, latency})
enum ErrorReason { INVALID_REQUEST = 1; SETTLEMENT_ID_REUSED = 2; SETTLEMENT_NOT_FOUND = 3; }
```

`proto/google/rpc/*`: de terceiros, só para o BFF decodificar o `ErrorInfo`.

**OpenAPI do BFF** (`gen/openapi/bff.json`): `POST /auth/demo-login`, `GET /home` (com `limits`), `GET /ping`,
`POST /pix` (202, `Idempotency-Key` obrigatório, 409), `GET /pix/{pixId}`.

## 5. Decisões dos ADRs (uma linha cada)

**ADR-0001 a ADR-0006**: ver o histórico deste arquivo; resumo:
- 0001: monorepo, um módulo Go por serviço (`go.work`), contrato primeiro com `buf`, `gen/` fora do Git, Connect no TS.
- 0002: só OTLP para o Collector; Prometheus, Tempo, Loki e Grafana; providers explícitos, sem globais.
- 0003: tudo em contêiner (imagem `tools`), dois alvos por Dockerfile, testes de integração contra serviços de teste do
  compose; **Toxiproxy entrou na Fase 3** (`testkit/toxi`).
- 0004: ledger em partida dobrada, lock pessimista, idempotência na mesma transação, papéis `_owner`/`_app`; **o
  script de init deu lugar ao job `postgres-bootstrap`** (Fase 3).
- 0005: BFF agrega, adapta e protege; degradação parcial por seção; **Fase 3: seção `limits` e `modules/pix`**.
- 0006: login de demonstração, JWT HS256, `traceparent` de entrada só como link.

**ADR-0007, transfers: saga, máquina de estados, retomada e timeout**
- Saga orquestrada no transfers (não coreografada); estado no Postgres do próprio transfers.
- 10 estados com transições explícitas; compare-and-set no estado **e no fencing token** (`lease_token`).
- Débito compensável (estorno) e retentável até resposta definitiva; liquidação é o pivô; crédito no destino e
  estorno só andam para frente.
- `POST /pix` responde 202 e a saga roda depois: fila sobre o Postgres com lease, aviso imediato e varredura;
  retomada pelo estado gravado, com chaves por etapa.
- "Não enviado" só com prova sobre **todas** as tentativas: marcador `settlement_maybe_sent`, irreversível, gravado
  antes de cada tentativa (circuit breaker de dois passos).
- Parâmetros: 3 tentativas de 1 s (backoff 200 ms → 1 s, full jitter); consultas de status de 2 s → 30 s (equal
  jitter); prazo máximo de 10 min, depois pendente e `overdue`.
- Limite diário reservado atomicamente; devolução ao dia da reserva (`limit_day`).
- Retry com full jitter escrito à mão; circuit breaker `sony/gobreaker/v2` por dependência.
- Crédito no destino forward-only; a conciliação da Fase 5 casa por `settlement_id` e `external_reference`, nunca pelo
  saldo da `settlement`, e só em janelas fechadas.
- Motivo `SOURCE_ACCOUNT_INVALID`; `ENTRY_ALREADY_REVERSED` conta como sucesso; aquecimento das conexões no boot.

**ADR-0008, antifraude e Redis**
- Score por janela deslizante aproximada; script Lua idempotente por `transfer_id`; valores devolvidos como texto
  (sem `double`).
- Redis fora do ar: degradar (aprova até R$ 500, recusa acima); antifraude fora do ar: o transfers recusa o Pix.
- Redis 8.8.4, `maxmemory 64mb`, `volatile-lru`, sem persistência e **sem volume** (desvio registrado); o descarte
  pode apagar contadores, e a métrica de chaves descartadas fica no dashboard.
- Biblioteca: `redis/go-redis` v9 com `redisotel` (sem os argumentos dos comandos nos spans).

**ADR-0009, ledger: estorno e referência externa**
- RPC `Reverse` (não `Credit` com motivo); estorno integral, só de débito `EXTERNAL`, uma vez só em três camadas.
- `EntryReason` exposto no `Posting`; motivos antigos preenchidos pela migration `00002`.
- `external_reference` no hash do pedido só quando informada (hash de pedidos antigos inalterado).

**ADR-0010, módulo `platform` compartilhado**
- `platform/` (chassis): telemetria, log, servidor e cliente gRPC, admin, healthcheck e pool do Postgres; domínio,
  configuração, schema e tradução de erros continuam em cada serviço. Desvio do layout da seção 4.

## 6. Como rodar e testar

```bash
cp .env.example .env     # ou acrescente ao seu .env as variáveis novas da Fase 3 (abaixo)
docker compose run --rm tools make lint test                       # lint e testes de tudo
docker compose --profile core up -d --build                        # bootstrap, migrations e todos os serviços
docker compose run --rm tools make seed                            # 4 contas de demonstração (idempotente)

# Fase 3: Pix pelo BFF (Ana -> Bruno)
TOKEN=$(curl -s -X POST localhost:8088/auth/demo-login -H 'content-type: application/json' -d '{"document":"12345678909"}' | jq -r .accessToken)
curl -si -X POST localhost:8088/pix -H "authorization: Bearer $TOKEN" -H "idempotency-key: $(uuidgen)" \
  -H 'content-type: application/json' -d '{"destinationKey":{"type":"cpf","value":"98765432100"},"amountCents":1000}'
curl -s localhost:8088/pix/<pixId> -H "authorization: Bearer $TOKEN"
curl -s localhost:8088/home -H "authorization: Bearer $TOKEN"        # com a seção limits

# Simular falhas (rede interna, pelo tools)
docker compose run --rm tools grpcurl -plaintext -import-path proto -proto conta/spi/v1/spi.proto \
  -d '{"behavior":{"settle_mode":"SETTLE_MODE_SETTLE_NO_RESPONSE"}}' spi-fake:50071 conta.spi.v1.SpiAdminService/SetBehavior
docker compose run --rm tools grpcurl -plaintext -import-path proto -proto conta/antifraude/v1/antifraude.proto \
  -d '{"faults":{"latency":"2s"}}' antifraude:50061 conta.antifraude.v1.AntifraudeAdminService/SetFaults
docker compose stop redis                                           # antifraude degradado; start redis para voltar

# Critérios de pronto na stack (com obs no ar e exportando)
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317 docker compose --profile core --profile obs up -d --build
docker compose run --rm tools make obs-e2e                         # Fase 0: um ping, um trace
docker compose run --rm tools make pix-e2e                         # Fase 3: os quatro itens do critério

# Testes focados da Fase 3
docker compose run --rm tools sh -c 'cd services/transfers && go test -race -count=1 -v ./internal/saga/'
docker compose run --rm tools sh -c 'cd services/antifraude && go test -race -count=1 -v ./internal/redisvelocity/'
docker compose run --rm tools sh -c 'cd services/ledger && go test -race -count=1 -v -run "Reverse|Reversal" ./internal/...'
docker compose --profile core run --rm postgres-bootstrap          # idempotente: pode rodar quantas vezes quiser
```

**Variáveis novas obrigatórias no `.env`** (valores de desenvolvimento no `.env.example`): `SPI_DB_OWNER_PASSWORD`,
`SPI_DB_APP_PASSWORD`, `TRANSFERS_DB_OWNER_PASSWORD`, `TRANSFERS_DB_APP_PASSWORD`, `TRANSFERS_REQUEST_HASH_KEY` (mínimo
32 caracteres). Sem elas, nenhum comando `docker compose` funciona.

Contas de demonstração (CPFs fictícios): Ana 123.456.789-09 (R$ 5.000,00), Bruno 987.654.321-00 (R$ 1.500,00),
Carla 111.444.777-35 (R$ 250,00), Diego 000.000.001-91 (R$ 0,00). Saldos mudam com os Pix; os `account_id` mudam se o
banco for recriado.

## 7. Métricas, logs, dashboards e alertas

**Alertas: nenhum.** Alertmanager e runbooks entram na Fase 6. **Gatilhos existentes**: gate do k6 no CI; circuit
breaker por dependência no transfers (abre com 5 falhas seguidas ou 50% em 20 chamadas em 10 s, fica aberto 5 s;
testado em `resilience_test.go` e `clients_test.go`; disparando por engano, o Pix é recusado com
`DEPENDENCY_UNAVAILABLE` na validação ou fica pendente na liquidação, nunca estornado sem prova).

**Métricas no Prometheus** (as das Fases 1 e 2 continuam; `job` = serviço):

| Origem | Métrica | Rótulos |
|---|---|---|
| todos os Go | `rpc_server_call_duration_seconds_*`, `go_*` | `rpc_method`, `rpc_response_status_code` |
| transfers, bff | `rpc_client_call_duration_seconds_*` | `rpc_method`, `rpc_response_status_code` (bff: `dependency`) |
| ledger, spi-fake, transfers | `db_client_operation_duration_seconds_*`, `pgxpool_*` | |
| antifraude | `antifraude_decisions_total` | `antifraude_decision`, `antifraude_degraded` |
| antifraude | `antifraude_degraded_total` | `antifraude_degraded_reason` (timeout, error) |
| antifraude | `antifraude_score_*` (histograma 0-100) | `antifraude_degraded` |
| antifraude | `antifraude_fault_latency_seconds`, `antifraude_fault_error_rate_ratio` | |
| antifraude | `db_client_connections_use_time_milliseconds_*` (redisotel) | `type`, `status`, `error_type` |
| Collector | `redis_memory_used_bytes`, `redis_maxmemory_bytes`, `redis_keys_evicted_total`, `redis_clients_connected` e outras | |
| spi-fake | `spi_settlements_total` | `spi_status`, `spi_replay` |
| spi-fake | `spi_settle_held_total` | `spi_settle_mode` |
| spi-fake | `spi_status_queries_total` | `spi_query_result` (found, not_found, failed) |
| spi-fake | `spi_behavior_settle_mode`, `spi_behavior_fail_status_queries`, `spi_behavior_latency_seconds` | `spi_settle_mode` |
| transfers | `transfers_pix_started_total`, `transfers_pix_started_amount_total`, `transfers_idempotency_conflicts_total` | `transfers_replay` |
| transfers | `transfers_saga_transitions_total` | `transfers_from`, `transfers_to` |
| transfers | `transfers_pix_outcomes_total` | `transfers_state`, `transfers_reason` |
| transfers | `transfers_compensations_total` | `transfers_compensation` (limit_release, reversal) |
| transfers | `transfers_saga_step_duration_seconds_*` | `transfers_step`, `transfers_result` |
| transfers | `transfers_settlement_attempts_total` | `transfers_settle_result` (settled, rejected, not_sent, ambiguous = timeouts) |
| transfers | `transfers_confirmation_pending`, `_pending_overdue`, `_oldest_pending_age_seconds` (do banco) | |
| transfers | `transfers_confirmation_status_queries_total`, `transfers_confirmation_overdue_total` | `transfers_status_query_result` |
| transfers | `transfers_dependency_retries_total`, `transfers_circuit_breaker_state` (0/1/2) | `dependency` |
| transfers | `transfers_leases_lost_total` | |
| bff | `bff_home_degraded_total` | `home_section` (statement, **limits**), `home_degraded_reason` |

IDs de conta, Pix e liquidação só em spans e logs, nunca em rótulos (testado em cada adaptador de telemetria). A chave
Pix sai sempre mascarada (`***.456.789-**`), e o CPF não aparece em log nem span do BFF (testado).

**Traces**: um Pix é um trace só: `POST /pix` (bff) → `StartPix` → `pix.saga.<estado>` (uma por etapa, filhas do
contexto gravado no aceite) → chamadas a antifraude (`evalsha` no Redis), ledger (queries) e spi-fake. Etapas sem
resposta definitiva ficam com erro e o motivo no span.

**Dashboards**: "visão geral", "ledger", "BFF" (dependências agora ledger **e transfers**), **"antifraude"**,
**"spi-fake"** e **"transfers"** (RED do aceite, desfechos, transições, compensações, etapas, liquidação, confirmação
com pendentes e idade do mais antigo, dependências e circuito, idempotência, logs, sagas com erro).

## 8. Pendências e dívidas conhecidas

**Para você fechar**
- **Acrescentar ao seu `.env`** as cinco variáveis novas da seção 6.
- O `.devcontainer` **nunca rodou** (herdado).

**Dívidas novas da Fase 3**
- **Pix pendentes além do prazo não se fecham sozinhos** até a conciliação (Fase 5), por decisão (regra 10). No seu
  banco há alguns, criados pelos testes na stack (um por execução de `make pix-e2e`), com o débito mantido.
- Depois de entrar em `PENDING_CONFIRMATION`, o transfers **não reenvia** a liquidação; só consulta. Se o SPI nunca
  recebeu o pedido, só a conciliação resolve (seção 4.5 do plano).
- Crédito no destino ou estorno que falham para sempre deixam o Pix parado em `SETTLED` ou `REVERSING` (repetindo a
  cada 2 s); visível no dashboard, alerta na Fase 6.
- **Sem expurgo** da tabela de idempotência do transfers (como a do ledger, herdada); a do transfers não guarda dado
  pessoal.
- O registro da transferência guarda a chave Pix de destino em claro (dado de negócio, como num comprovante); logs,
  traces e métricas só a veem mascarada.
- `GetLimits` não confere se a conta existe (devolve o limite cheio para qualquer UUID).
- Regras do antifraude e limite diário são didáticos (sem limite noturno, sem limite por conta, só chave CPF).
- O spi-fake ainda não gera o relatório de liquidações (Fase 5).
- O Toxiproxy só existe nos testes; para simular falha de rede no ambiente, use os modos do spi-fake, as falhas do
  antifraude ou `docker compose stop`.
- Os testes com Redis e Toxiproxy dependem dos serviços `redis-test` e `toxiproxy` (sobem junto com o `tools`).

**Desvios do plano (registrados nos ADRs)**
- **Pasta `platform/` na raiz** (ADR-0010), fora do layout `internal/platform` por serviço.
- **Redis sem volume nomeado** (ADR-0008): sem persistência, não há dado em disco.
- `web/` sem imagem final (ADR-0005, Fase 2); `testkit/`, `cmd/seed`, `internal/accountotel` (Fases 0 e 1).

**Herdadas e ainda abertas**
- Idempotência do ledger sem expurgo e com o CPF completo na resposta gravada do `OpenAccount` (não piorou).
- `GET /ping` do BFF continua público; rate limit só na Fase 6.
- Token do front em `sessionStorage`, sem refresh; o BFF não cancela chamadas quando o cliente desconecta.
- O gate do k6 roda contra o ledger de desenvolvimento; a stack de observabilidade distorce a cauda de latência no Mac.
- Retenção do Tempo não observada; CI sem cache de camadas da imagem `tools`; `service.version` = `dev`.

**Resolvidas na Fase 3**: estorno e referência externa no ledger (pendência da Fase 2); Toxiproxy no compose
(prometido no ADR-0003); provisionamento dos bancos sem recriar o volume (job idempotente).

## 9. Próxima fase

**Fase 4: Eventos e tempo real**, pelo prompt da seção 8 do plano. Reaproveita: a máquina de estados e o histórico do
transfers (os eventos de status nascem nas transições, que já são transacionais: o outbox entra na mesma transação),
o `GetPix` (estado atual enviado a cada conexão SSE), o BFF (`modules/pix`) e o Redis (pub/sub, seção 4.3). O gatilho
de backpressure pelo acúmulo do outbox (seção 7.4) é dessa fase.
