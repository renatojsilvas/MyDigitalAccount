# Estado do projeto

Atualizado ao fim da **Fase 1 (Ledger)**, em 2026-11-12. Leia este arquivo no início de cada fase.
Referências: `docs/plano-conta-digital.md` (plano), `docs/arquitetura-conta-digital.md` (diagrama),
`docs/adr/` (decisões), `load/README.md` (gate de desempenho).

## 1. Fases

| Fase | Estado |
|---|---|
| 0. Fundação | **Concluída** |
| 1. Ledger (Go) | **Concluída** |
| 2. BFF "home" (TS) | Próxima |
| 3 a 8 | Pendentes |

**Critério de pronto da Fase 1**, verificado:

1. **Débitos concorrentes nunca deixam o saldo negativo**, com `-race`: `TestConcurrentDebitsNeverOverdraw`
   (`services/ledger/internal/account/service_test.go`), 4 casos (2 débitos de 80 em 100: passa 1; 50 de 300 em
   10.000: passam exatamente 33; 10 que zeram a conta; conta vazia), conferindo também saldo gravado = soma das
   pernas e extrato encadeado sem saldo negativo. **Checagem de mutação**: sem o `FOR UPDATE`, o teste falha por
   *lost update* (registrado no ADR-0004).
2. **O gate do k6 passa** (`load/ledger-baseline.js`, código 0) e reprova quando um limite é violado (código 99).

Num checkout limpo (sem `gen/`, `sqlcgen/` nem `node_modules`), `make lint test` passa em 62 s: Go com 12 pacotes
(`-race`), BFF com 49 testes Vitest.

## 2. O que existe

- **ledger** (Go), serviço completo da Fase 1:
  - Contas de cliente (uma por CPF) e contas internas por finalidade (`opening_balance`, `settlement`); lançamentos em
    partida dobrada, append-only; saldo materializado só nas contas de cliente, sob lock pessimista; extrato paginado
    por cursor opaco.
  - Idempotência em todas as escritas (tabela `idempotency_keys`, mesma transação), inclusive recusas de negócio, que
    são gravadas e repetidas idênticas (status gRPC + `ErrorInfo`).
  - Dois papéis Postgres (`ledger_owner` para migrations, `ledger_app` para o serviço, só DML e append-only por
    privilégio); migrations embutidas no binário e aplicadas pelo job `ledger-migrate`.
  - Seed de 4 contas de demonstração (`make seed`), pela API, idempotente.
  - Observabilidade: RED do gRPC, spans e métricas do `pgx`, USE do pool, métricas de negócio, dashboard próprio.
- **bff** (TypeScript, Fastify): ainda só `GET /ping` (Fase 0). Na Fase 1 mudaram apenas os nomes da telemetria do
  cliente gRPC (semconv v1.43, iguais aos do ledger).
- **Infra**: profiles `core` (Postgres, ledger-migrate, ledger, bff), `obs` (Collector, Prometheus, Tempo, Loki,
  Grafana), `load` (k6), `tools`/`test` (imagem de ferramentas e Postgres de teste).
- **Não existe ainda**: autenticação, `transfers`, `antifraude`, `spi-fake`, `notifications`, `reconciler`, Kafka, Redis,
  front-end, Alertmanager, runbooks.

## 3. Árvore de arquivos (versionados)

Só o que mudou ou nasceu na Fase 1; o resto segue como na Fase 0 (ver o histórico deste arquivo no Git).

```
proto/conta/ledger/v1/ledger.proto        contrato do ledger (seção 4)
services/ledger/
  sqlc.yaml                                sqlc (pgx/v5); saída em internal/postgres/sqlcgen/, fora do Git
  migrations/                              00001_ledger_schema.sql + migrations.go (embed.FS); só para frente
  cmd/ledger/                              main.go (composition root), migrate.go (subcomando), healthcheck.go
  cmd/seed/                                seed das contas de demonstração (cliente gRPC) + demo-accounts.json
  internal/account/                        domínio e casos de uso, sem banco nem gRPC:
                                             money, document (CPF mascarado ao formatar), text, ids (UUIDv7),
                                             reason (motivo -> conta interna), entry (partida dobrada), account,
                                             errors, ports (Store, Tx, ResultCodec), idempotency, service, observe
  internal/accountotel/                    adaptador OTel da porta account.Observer (métricas de negócio)
  internal/postgres/                       adaptador de persistência: Store, Tx, WithTx, mapping, errors; queries/*.sql
  internal/grpcapi/                        handlers do LedgerService, errors (statusFor + interceptor), codec, mapping
  internal/platform/postgres/              pool (otelpgx, lock_timeout, MaxConns), Ping, Migrate; pgtest/ (helper)
testkit/testpg/                            + EnsureRole e ConnectAs (papel restrito nos testes)
deploy/postgres/init/01-ledger.sh          papéis ledger_owner e ledger_app, banco ledger
deploy/compose/load.yaml                   k6 (profile load)
deploy/grafana/dashboards/ledger.json      dashboard "Conta Digital: ledger"
load/ledger-baseline.js, load/README.md    gate de desempenho e origem dos thresholds; load/results/ fora do Git
docs/adr/0004-ledger-partida-dobrada-concorrencia-idempotencia.md
```

## 4. Contratos `.proto`

`proto/conta/ping/v1/ping.proto`: inalterado desde a Fase 0 (`PingService.Ping`).

`proto/conta/ledger/v1/ledger.proto` (pacote `conta.ledger.v1`), resumo:

```proto
service LedgerService {
  rpc OpenAccount(OpenAccountRequest) returns (OpenAccountResponse);   // idempotency_key, holder_name, holder_document (CPF)
  rpc GetAccount(GetAccountRequest) returns (GetAccountResponse);      // account_id
  rpc Credit(CreditRequest) returns (CreditResponse);                  // idempotency_key, account_id, amount_cents, reason, description
  rpc Debit(DebitRequest) returns (DebitResponse);                     // idem, com DebitReason
  rpc GetBalance(GetBalanceRequest) returns (GetBalanceResponse);      // account_id -> balance_cents
  rpc ListStatement(ListStatementRequest) returns (ListStatementResponse); // page_size (1..100, 0 = 20), page_token
}
enum ErrorReason  { UNSPECIFIED; ACCOUNT_NOT_FOUND = 1; INSUFFICIENT_FUNDS = 2; IDEMPOTENCY_KEY_REUSED = 3;
                    INVALID_REQUEST = 4; ACCOUNT_ALREADY_EXISTS = 5; }   // prefixo ERROR_REASON_
enum CreditReason { UNSPECIFIED; OPENING = 1; EXTERNAL = 2; }          // contrapartida: opening_balance / settlement
enum DebitReason  { UNSPECIFIED; EXTERNAL = 1; }                       // contrapartida: settlement
enum AccountStatus { UNSPECIFIED; ACTIVE = 1; }
enum Direction    { UNSPECIFIED; DEBIT = 1; CREDIT = 2; }
message Account { account_id; holder_name; holder_document; status; created_at }
message Posting { entry_id; account_id; direction; amount_cents; balance_after_cents; description; posted_at }
```

Valores em centavos (`int64`), sempre positivos. Erros: status gRPC canônico + `google.rpc.ErrorInfo`
(`reason` = nome do `ErrorReason`, `domain` = `ledger.conta.local`, `metadata.field` nos erros de validação):

| Situação | Status | `ErrorReason` |
|---|---|---|
| Conta inexistente | `NOT_FOUND` | `ACCOUNT_NOT_FOUND` |
| Saldo insuficiente | `FAILED_PRECONDITION` | `INSUFFICIENT_FUNDS` |
| CPF já tem conta | `ALREADY_EXISTS` | `ACCOUNT_ALREADY_EXISTS` |
| Chave reutilizada com outro pedido | `ALREADY_EXISTS` | `IDEMPOTENCY_KEY_REUSED` |
| Pedido inválido | `INVALID_ARGUMENT` | `INVALID_REQUEST` |
| Conta travada além do `lock_timeout` | `UNAVAILABLE` (repetir com a mesma chave é seguro) | nenhum |
| Erro inesperado | `INTERNAL` ("erro interno"; a causa vai para o log) | nenhum |

## 5. Decisões dos ADRs (uma linha cada)

**ADR-0001, monorepo e contrato primeiro**
- Monorepo, um módulo Go por serviço ligados por `go.work`, workspace pnpm para TypeScript.
- O `.proto` é a fonte; Go e TS são gerados pelo `buf` com plugins locais; `buf breaking` no CI.
- `gen/` fora do Git e excluído dos linters; `make generate` sobrescreve no lugar (não apaga) para não derrubar `tsx` e `air`.
- Cliente gRPC em TypeScript: Connect (`protobuf-es`), pelo `AbortSignal`/`timeoutMs` nativos e pelo servidor falso em memória.

**ADR-0002, observabilidade**
- Serviços exportam só OTLP para o Collector; backends Prometheus (métricas), Tempo (traces), Loki (logs), Grafana.
- Métricas por remote write; traces por OTLP gRPC; logs pelo endpoint OTLP nativo do Loki (`/otlp`).
- BFF sem auto-instrumentação e sem globais do OTel: o contexto de trace trafega explícito pela requisição.
- SDK do OTel JS na geração 3.x (`sdk-trace` 3.1.1, `api` 1.10.0); misturar com a 2.x quebra os pares.
- Exportação opt-in via `OTEL_EXPORTER_OTLP_ENDPOINT`; `service.instance.id` (hostname) em todo recurso.
- Tempo 3.x monolítico, disco local; Loki em instância única; retenção de 48 h (Tempo), 72 h (Loki), 7 d (Prometheus).
- Healthcheck de imagens sem shell com um `busybox` estático copiado; Grafana sem login só em `127.0.0.1`.
- `/readyz` do BFF não consulta o ledger (degradação parcial na Fase 2); o do ledger consulta o Postgres.

**ADR-0003, ambiente em contêineres**
- Imagem `tools` com tudo (Go 1.27.4, Node 24.22.2, pnpm 12.18.0, buf 1.75.0, sqlc 1.31.1, golangci-lint 2.15.0, goose 3.30.0, grpcurl 1.9.4, air 1.67.4, govulncheck 1.9.0); `GOTOOLCHAIN=local`.
- Dois alvos por Dockerfile: `dev` (reaproveita a imagem `tools`) e `final` (distroless nonroot / Node slim, bases por tag e digest).
- Compose por grupo, incluído pela raiz e autocontido; portas publicadas só em `127.0.0.1` por `*_HOST_PORT`.
- Testes de integração: serviços de teste no compose (opção B), isolamento por banco (`testkit/testpg`).
- Testes de falha das fases 3 e 4 (Redis e Kafka fora do ar) usarão o Toxiproxy, que só entra na Fase 3.

**ADR-0004, ledger: partida dobrada, concorrência e idempotência**
- Partida dobrada com contas internas **por finalidade** (`opening_balance` para o seed, `settlement` para o SPI); quem chama informa o motivo (enum), o ledger escolhe a contrapartida.
- Lançamentos append-only; a partida dobrada é conferida no domínio e de novo pelo banco (constraint trigger adiada no `COMMIT`).
- Concorrência: **lock pessimista** (`SELECT ... FOR UPDATE`), saldo gravado só nas contas de cliente, `CHECK (balance >= 0)`, `lock_timeout` de sessão (`LEDGER_DB_LOCK_TIMEOUT`, padrão 1s) com erro repetível.
- Contas internas sem saldo gravado nem lock, para não virarem gargalo global.
- Idempotência: tabela no Postgres na mesma transação; a chave é reservada primeiro; mesma chave e mesmo pedido = mesma resposta, **inclusive a recusa de negócio**; pedido diferente = `IDEMPOTENCY_KEY_REUSED`; erro de infraestrutura não é gravado.
- Resposta gravada = Protobuf da resposta (OK) ou `google.rpc.Status` com `ErrorInfo` (recusa), produzidos pela mesma tradução da borda.
- Um CPF, uma conta (`UNIQUE` + `ON CONFLICT DO NOTHING`, para a recusa poder ser gravada na mesma transação).
- Erros: status gRPC canônico + `ErrorInfo` (AIP-193), motivos num `enum ErrorReason` gerado.
- Papéis `ledger_owner` (DDL) e `ledger_app` (DML por tabela; `UPDATE` só de `balance_cents`); migrations como job, não no boot.
- sqlc fora do Git; `WithTx` com rollback em erro e panic; erro transitório reconhecido por `Retryable()`.
- `otelpgx` v0.13.0 para spans e métricas do pool; sem parâmetros nos spans; métricas de negócio por uma porta `Observer`.
- Rótulo de status gRPC unificado entre BFF e ledger na semconv v1.43 (`rpc.response.status_code` por nome); a semconv de RPC ainda evolui.

## 6. Como rodar e testar

```bash
cp .env.example .env     # HOST_UID/HOST_GID (id -u, id -g); LEDGER_DB_OWNER_PASSWORD e LEDGER_DB_APP_PASSWORD
docker compose run --rm tools make lint test                       # lint e testes (gera gen/ e sqlcgen/ antes)
docker compose --profile core up -d --build                        # Postgres, ledger-migrate (job), ledger, bff
docker compose run --rm tools make seed                            # 4 contas de demonstração (idempotente)
docker compose run --rm tools grpcurl -plaintext -import-path proto -proto conta/ledger/v1/ledger.proto \
  -d '{"account_id":"<id>"}' ledger:50051 conta.ledger.v1.LedgerService/GetBalance

# Critério de pronto da Fase 1
docker compose run --rm tools sh -c 'cd services/ledger && go test -race -count=1 -v -run Concurrent ./internal/account/'
docker compose --profile core up -d --wait ledger && docker compose --profile load run --rm k6   # saída 0

# Observabilidade (Grafana: dashboards "visão geral" e "ledger")
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317 docker compose --profile core --profile obs up -d --build
docker compose run --rm tools make obs-e2e

# Recriar só o banco (o script de init dos papéis só roda num volume novo); down -v apagaria o cache do Go
docker compose --profile core down && docker volume rm conta-digital_postgres-data
```

Contas de demonstração (CPFs fictícios): Ana 123.456.789-09 (R$ 5.000,00), Bruno 987.654.321-00 (R$ 1.500,00),
Carla 111.444.777-35 (R$ 250,00), Diego 000.000.001-91 (R$ 0,00). Os `account_id` são UUIDv7 gerados pelo ledger:
saem no log do `make seed` e **mudam se o banco for recriado**.

## 7. Métricas, logs, dashboards e alertas

**Alertas: nenhum.** Alertmanager e runbooks entram na Fase 6. **Gatilho existente**: gate de desempenho do k6 no
CI (job `load-baseline`; thresholds e origem em `load/README.md`).

**Métricas no Prometheus** (nomes como chegam do Collector; `job` = serviço, `instance` = contêiner):

| Origem | Métrica | Rótulos |
|---|---|---|
| BFF | `http_server_request_duration_seconds_*` | `http_request_method`, `http_route`, `http_response_status_code` |
| BFF | `rpc_client_call_duration_seconds_*` | `dependency`, `rpc_system_name`, `rpc_method` (completo), `rpc_response_status_code` (nome) |
| BFF | `nodejs_eventloop_delay_p99_seconds`, `nodejs_heap_used_bytes` | |
| ledger | `rpc_server_call_duration_seconds_*` | `rpc_system_name`, `rpc_method` (completo), `rpc_response_status_code` (nome) |
| ledger | `db_client_operation_duration_seconds_*` | `db_system_name`, `pgx_operation_type` (query, prepare, connect) |
| ledger | `pgxpool_acquired_connections`, `pgxpool_idle_connections`, `pgxpool_max_connections`, `pgxpool_total_connections` | `db_client_connection_pool_name` |
| ledger | `pgxpool_empty_acquire_wait_time_nanoseconds_total`, `pgxpool_canceled_acquires_total`, `pgxpool_acquires_total` e outras | idem |
| ledger | `ledger_entries_total`, `ledger_posted_amount_total` (centavos) | `ledger_operation` (credit, debit), `ledger_reason` (opening, external) |
| ledger | `ledger_rejections_total` | `ledger_operation`, `ledger_rejection` (insufficient_funds, account_not_found, account_already_exists) |
| ledger | `ledger_idempotency_replays_total`, `ledger_idempotency_conflicts_total` | `ledger_operation` |
| ledger | `ledger_accounts_opened_total`, `go_*` do runtime | |
| stack | `up` dos 5 componentes | `job` |

**Traces**: no ledger, cada escrita mostra as queries como filhas (`BEGIN`, reserva da chave, `FOR UPDATE`,
lançamento, pernas, saldo, gravação do resultado, `COMMIT`), sem valores de parâmetros, e os atributos
`ledger.operation`, `ledger.outcome`, `ledger.reason`, `ledger.rejection` e `ledger.account_id` (só no span, nunca em
rótulo). O `/readyz` não gera spans.

**Logs**: JSON no stdout e OTLP para o Loki, com `trace_id` e `span_id`; um por chamada gRPC; erros internos trazem a
causa. CPF mascarado na origem (o tipo `account.Document` só sai formatado como `***.456.789-**`).

**Dashboards**: "Conta Digital: visão geral" (Fase 0, painéis do ledger visto pelo BFF atualizados para os nomes
novos; falha = só falha do servidor) e **"Conta Digital: ledger"** (RED, negócio, USE do pool, logs, traces de escrita).

## 8. Pendências e dívidas conhecidas

**Para você fechar**
- O CI e o `.devcontainer` **nunca rodaram** no GitHub (instabilidade do Actions); o workflow passa no `actionlint`.
  Quando ficar verde, registrar aqui. Ao rodar, **recalibrar os thresholds do k6** com o artefato `k6-ledger-baseline`
  do runner (os atuais vêm do Mac).

**Para a Fase 2 (decidir no início dela)**
- **Login de demonstração precisa achar a conta do usuário**: os `account_id` mudam se o banco for recriado. Opções:
  uma RPC de consulta por CPF no ledger (mudança de contrato, não quebra) ou o BFF manter o mapeamento.
- O BFF vai precisar do tipo `google.rpc.ErrorInfo` gerado em TypeScript para ler os detalhes dos erros do ledger.

**Para fases seguintes**
- Estorno (novo lançamento invertido, motivo novo no enum) e referência externa no lançamento (ID da transferência),
  na Fase 3; listagem de lançamentos por janela, em streaming, na Fase 5.
- Idempotência: **sem expurgo** da tabela `idempotency_keys`, e a resposta gravada do `OpenAccount` contém o **CPF
  completo** (dado pessoal duplicado, sem retenção). Resolver juntos (ADR-0004).
- O gate do k6 roda contra o ledger de desenvolvimento (`air`), não contra a imagem final.
- A stack de observabilidade distorce a cauda de latência no Mac (CPU disputada na VM): medir carga só com `core`.

**Técnicas (herdadas da Fase 0, ainda abertas)**
- Retenção de 48 h do Tempo não observada na prática; CI sem cache de camadas da imagem `tools`.
- O interceptor Connect do BFF só instrumenta chamadas unárias; `traceparent` de entrada aceito de qualquer cliente
  (rever nas Fases 2 e 6).
- `psql` da imagem `tools` é o 15 contra servidor 18 (use `docker compose exec postgres psql`).
- `gen/` e `sqlcgen/` são pré-requisito do build das imagens finais; exportação de telemetria opt-in; `service.version` = `dev`.
- O título do dashboard "visão geral" ainda diz "(Fase 0)".

**Resolvidas na Fase 1**: papel Postgres próprio do ledger; spans do `pgx` e métricas do pool; rótulo de status gRPC
unificado entre BFF e ledger; teste do ping que oscilava (espera com prazo).

**Desvios do plano (registrados nos ADRs)**
- Pasta `testkit/` (módulo Go) fora do layout da seção 4; pastas `deploy/tempo`, `deploy/loki`, `deploy/obs`,
  `deploy/postgres` além das listadas.
- `internal/accountotel` (adaptador de métricas de negócio) e `cmd/seed` além do layout da seção 4.
- Banco de desenvolvimento: há uma conta de teste (CPF 529.982.247-25) e contas `Carga N` criadas pelo k6; somem ao
  recriar o volume.

## 9. Próxima fase

**Fase 2: BFF "home" (TS)**, modelo recomendado na seção 8 do plano. Começa pelo prompt da Fase 2 e pela decisão de
como o login de demonstração encontra a conta (seção 8 acima). Reaproveita: o contrato `conta.ledger.v1`
(`GetBalance`, `ListStatement`), o seed (`make seed`), o cliente Connect e o interceptor de telemetria do BFF.
