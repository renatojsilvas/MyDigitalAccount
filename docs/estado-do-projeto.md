# Estado do projeto

Atualizado ao fim da **Fase 2 (BFF "home")**, em 2026-11-12. Leia este arquivo no início de cada fase.
Referências: `docs/plano-conta-digital.md` (plano), `docs/arquitetura-conta-digital.md` (diagrama),
`docs/adr/` (decisões), `load/README.md` (gate de desempenho).

## 1. Fases

| Fase | Estado |
|---|---|
| 0. Fundação | **Concluída** |
| 1. Ledger (Go) | **Concluída** |
| 2. BFF "home" (TS) | **Concluída** |
| 3. Transfers e antifraude (Go) | Próxima |
| 4 a 8 | Pendentes |

**Critério de pronto da Fase 2**, verificado:

1. **Com o extrato lento ou fora do ar, a `/home` responde 200 com o saldo e o extrato `unavailable`**:
   `bff/test/home.test.ts` (extrato lento: estoura os 80 ms dos testes; fora do ar) e `home-service.test.ts`. Contra
   a stack real, com `LEDGER_STATEMENT_TIMEOUT_MS=1`: resposta 200 com `statement: {status:"unavailable",
   reason:"timeout"}`.
2. **Aparece no contador e no trace**: `bff_home_degraded_total{home_section="statement",
   home_degraded_reason="timeout"}` no Prometheus; no Tempo, `GET /home` com `home.degraded=true`, `ListStatement`
   com `DEADLINE_EXCEEDED` no BFF **e no ledger** (o prazo é propagado) e `GetBalance` OK. O teste de unidade confere
   o mesmo (contador, atributo, evento e status do span do cliente).
3. **No navegador**, a Home mostra o saldo e um aviso no lugar do extrato: coberto pelo teste `Home.test.tsx`
   ("com o extrato indisponível..."). **A conferência visual no navegador fica com você** (a extensão do Chrome não
   conectou nesta sessão): `http://127.0.0.1:5173`, CPF `12345678909`; para forçar o aviso, suba o BFF com
   `LEDGER_STATEMENT_TIMEOUT_MS=1`.

`make lint test`: Go com 12 pacotes (`-race`), BFF com 116 testes Vitest, web com 13. A suíte do BFF foi estressada
(8 processos consumindo CPU em 4 CPUs): 0 falhas em 20 execuções (ver "Resolvidas na Fase 2", seção 8).

**Critério de pronto da Fase 1**, verificado:

1. **Débitos concorrentes nunca deixam o saldo negativo**, com `-race`: `TestConcurrentDebitsNeverOverdraw`
   (`services/ledger/internal/account/service_test.go`), 4 casos (2 débitos de 80 em 100: passa 1; 50 de 300 em
   10.000: passam exatamente 33; 10 que zeram a conta; conta vazia), conferindo também saldo gravado = soma das
   pernas e extrato encadeado sem saldo negativo. **Checagem de mutação**: sem o `FOR UPDATE`, o teste falha por
   *lost update* (registrado no ADR-0004).
2. **O gate do k6 passa** (`load/ledger-baseline.js`, código 0) e reprova quando um limite é violado (código 99).

Num checkout limpo da Fase 1, `make lint test` passava em 62 s.

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
- O ledger ganhou, na Fase 2, a RPC `GetAccountByDocument` (busca a conta pelo CPF; leitura, aditiva).
- **bff** (TypeScript, Fastify), serviço da Fase 2:
  - `GET /home` (autenticada): saldo + últimas 10 transações, em duas chamadas paralelas ao ledger. Saldo é essencial
    (falha = 504/503/502, ou 401 se a conta do token não existe mais); extrato é opcional (falha = 200 com
    `statement.status = "unavailable"` e o motivo `timeout | unavailable | error`).
  - `POST /auth/demo-login` (só `development` e `test`; em production a rota não existe e o boot falha se for
    ligada): CPF → `GetAccountByDocument` → JWT HS256 (`sub` = `account_id`, 15 min, sem CPF no token).
  - `GET /ping` continua (diagnóstico da Fase 0, **público**).
  - OpenAPI gerado dos schemas Zod: `gen/openapi/bff.json` (`make generate`); sondas de saúde ficam fora.
  - Erros em `application/problem+json` (RFC 9457) com `trace_id`; toda resposta traz `x-trace-id`. O `traceparent` de
    entrada **não é continuado**: vira um span link num trace novo (ADR-0006).
- **web** (React 19 + Vite 8, TypeScript estrito), front de demonstração: login por CPF digitado e tela Home. Tipos e
  cliente (`openapi-fetch`) gerados do OpenAPI do BFF. Só imagem de desenvolvimento (sem imagem final).
- **Infra**: profiles `core` (Postgres, ledger-migrate, ledger, bff, **web**), `obs` (Collector, Prometheus, Tempo, Loki,
  Grafana), `load` (k6), `tools`/`test` (imagem de ferramentas e Postgres de teste).
- **Não existe ainda**: `transfers`, `antifraude`, `spi-fake`, `notifications`, `reconciler`, Kafka, Redis, telas Novo
  Pix e Acompanhar Pix, Alertmanager, runbooks.

## 3. Árvore de arquivos (versionados)

Só o que mudou ou nasceu nas Fases 1 e 2; o resto segue como na Fase 0 (ver o histórico deste arquivo no Git).

**Fase 2:**

```
proto/google/rpc/                          error_details.proto e status.proto do googleapis (commit fixado, README.md);
                                             geram SÓ TypeScript; fora do buf lint/format/breaking
buf.gen.yaml / buf.gen.ts.yaml             Go (exclui proto/google) e TypeScript (tudo), dois modelos
bff/src/
  config.ts                                + BFF_JWT_SECRET (>= 32, sem padrão), BFF_JWT_TTL_SECONDS, BFF_DEMO_LOGIN,
                                             LEDGER_BALANCE_TIMEOUT_MS (250), LEDGER_STATEMENT_TIMEOUT_MS (200)
  clients/ledger.ts                        adaptador do ledger: único que conhece gRPC; lê o ErrorInfo gerado
  errors.ts, problem-schema.ts             DependencyError, UnauthenticatedError, BffError; Problem (RFC 9457)
  modules/home/                            schema (Zod), mapper (bigint -> int), service (allSettled), observe, route
  modules/auth/                            schema, service, route (POST /auth/demo-login)
  plugins/auth.ts, openapi.ts              JWT (@fastify/jwt) e OpenAPI (@fastify/swagger)
  plugins/telemetry.ts                     span raiz + link do traceparent de entrada + x-trace-id
bff/scripts/generate-openapi.ts            grava gen/openapi/bff.json
bff/test/                                  fake-ledger.ts (ledger gRPC falso), auth, home, home-service, ledger-client,
                                             openapi, error-info (gRPC real) e os anteriores
web/                                       React + Vite; src/api/client.ts (cliente tipado), src/screens/{Login,Home},
                                             src/money.ts; src/api/schema.d.ts é GERADO (fora do Git)
deploy/grafana/dashboards/bff.json         dashboard "Conta Digital: BFF"
docs/adr/0005-bff-papel-e-degradacao-parcial.md
docs/adr/0006-login-de-demonstracao-jwt-e-traceparent.md
```

**Fase 1:**

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

`proto/google/rpc/{error_details,status}.proto`: de terceiros (googleapis), só para o BFF decodificar o `ErrorInfo`.

`proto/conta/ledger/v1/ledger.proto` (pacote `conta.ledger.v1`), resumo (na Fase 2 ganhou `GetAccountByDocument`):

```proto
service LedgerService {
  rpc OpenAccount(OpenAccountRequest) returns (OpenAccountResponse);   // idempotency_key, holder_name, holder_document (CPF)
  rpc GetAccount(GetAccountRequest) returns (GetAccountResponse);      // account_id
  rpc GetAccountByDocument(GetAccountByDocumentRequest) returns (GetAccountByDocumentResponse); // holder_document (CPF)
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
- O contexto `tools` dos Dockerfiles é `docker-image://conta-digital/tools:dev`, não `service:tools` (profile inativo = serviço inexistente no `up` do CI); a imagem precisa existir antes do `up` (`make generate` a constrói).

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

**ADR-0005, BFF: papel, limites e degradação parcial**
- O BFF agrega, adapta e protege; não calcula valor, não decide permissão, não guarda dado de negócio nem cacheia saldo.
- Duas fontes escritas à mão: o `.proto` e o schema Zod. O resto é gerado (`protoc-gen-es`, OpenAPI, `openapi-typescript`).
- `clients/ledger.ts` é a *anti-corruption layer*: o resto do BFF não vê gRPC nem `bigint` além do mapper.
- Saldo essencial (falha = erro; conta do token inexistente = 401), extrato opcional (200 + `unavailable` + motivo, união discriminada).
- Chamadas em paralelo, cada uma com o prazo próprio (250/200 ms, menores que o SLO de 300 ms), propagado ao ledger.
- `bigint` -> inteiro com guarda de inteiro seguro; estourar é falha, nunca arredondamento.

**ADR-0006, login de demonstração, JWT e `traceparent`**
- Login acha a conta pela RPC `GetAccountByDocument` do ledger (o BFF não guarda mapeamento); o front pede o CPF digitado.
- JWT HS256 com algoritmo fixado dos dois lados, `iss`/`aud`, 15 min, `sub` = `account_id`, sem CPF; segredo em variável de ambiente validada no boot.
- Login de demonstração só em `development`/`test`; em production a rota não existe e o boot falha se for ligada. CPF sem conta = 401.
- `traceparent` de entrada não é confiável: o BFF abre trace novo (raiz) e guarda o contexto recebido como span link; `x-trace-id` na resposta.
- `google.rpc` do googleapis copiado e fixado por commit, gerado só em TypeScript (o Go usa o `errdetails` do `genproto`).

## 6. Como rodar e testar

```bash
cp .env.example .env     # HOST_UID/HOST_GID (id -u, id -g); LEDGER_DB_*_PASSWORD; BFF_JWT_SECRET (obrigatório, >= 32)
docker compose run --rm tools make lint test                       # lint e testes (gera gen/ e sqlcgen/ antes)
docker compose --profile core up -d --build                        # Postgres, ledger-migrate (job), ledger, bff, web
docker compose run --rm tools make seed                            # 4 contas de demonstração (idempotente)
docker compose run --rm tools grpcurl -plaintext -import-path proto -proto conta/ledger/v1/ledger.proto \
  -d '{"account_id":"<id>"}' ledger:50051 conta.ledger.v1.LedgerService/GetBalance

# Fase 2: front, login e home (depois do seed)
open http://127.0.0.1:5173                                         # login com um CPF do seed (ex.: 12345678909)
curl -s -X POST localhost:8088/auth/demo-login -H 'content-type: application/json' -d '{"document":"12345678909"}'
curl -s localhost:8088/home -H "authorization: Bearer <accessToken>"
# Forçar o extrato degradado (reinicia o BFF com um prazo de 1 ms para o extrato):
LEDGER_STATEMENT_TIMEOUT_MS=1 docker compose --profile core up -d --wait bff
docker compose run --rm tools make generate                        # inclui gen/openapi/bff.json e web/src/api/schema.d.ts

# Critério de pronto da Fase 1
docker compose run --rm tools sh -c 'cd services/ledger && go test -race -count=1 -v -run Concurrent ./internal/account/'
docker compose --profile core up -d --wait ledger && docker compose --profile load run --rm k6   # saída 0

# Observabilidade (Grafana: dashboards "visão geral" e "ledger")
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317 docker compose --profile core --profile obs up -d --build
docker compose run --rm tools make obs-e2e

# Recriar só o banco (o script de init dos papéis só roda num volume novo); down -v apagaria o cache do Go
docker compose --profile core down && docker volume rm conta-digital_postgres-data
```

Atenção: a **primeira** chamada ao ledger depois que o BFF sobe é lenta (partida a frio, seção 8): se o login der 504,
repita. Contas de demonstração (CPFs fictícios): Ana 123.456.789-09 (R$ 5.000,00), Bruno 987.654.321-00 (R$ 1.500,00),
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
| BFF | `bff_home_degraded_total` (contador de respostas da home degradadas) | `home_section` (statement), `home_degraded_reason` (timeout, unavailable, error) |
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

**Traces (BFF)**: `GET /home` é raiz (o `traceparent` de entrada vira link); com extrato degradado tem
`home.degraded=true` e o evento `home.degraded`; o span do cliente que estourou o prazo fica com erro
(`DEADLINE_EXCEEDED`), e o do ledger também. Rotas autenticadas levam `enduser.id` (= `account_id`, nunca CPF).

**Logs**: JSON no stdout e OTLP para o Loki, com `trace_id` e `span_id`; um por chamada gRPC; erros internos trazem a
causa. CPF mascarado na origem (o tipo `account.Document` só sai formatado como `***.456.789-**`).

**Dashboards**: "Conta Digital: visão geral" (Fase 0, painéis do ledger visto pelo BFF atualizados para os nomes
novos; falha = só falha do servidor) **"Conta Digital: ledger"** (RED, negócio, USE do pool, logs, traces de escrita) e **"Conta Digital: BFF"** (SLI da home
contra o SLO da seção 7.3, home degradada, home por status, RED por rota, degradação por motivo, timeouts do ledger,
falhas e latência da dependência, event loop e heap, logs, traces de home degradada). O título "(Fase 0)" do visão
geral foi removido. O SLI usa a fronteira de 250 ms do histograma (não há 300 ms): é conservador.

## 8. Pendências e dívidas conhecidas

**Para você fechar**
- O `.devcontainer` **nunca rodou** (abrir o VS Code dentro do contêiner `tools`); só o CI foi validado.

**Decisões pendentes da Fase 2 (suas)**
- **Partida a frio do BFF**: a primeira chamada ao ledger depois que o BFF sobe leva ~300 ms (`/home`) a mais de 1 s
  (login, que deu 504), e as seguintes 10 a 25 ms. Com 250/200 ms, o primeiro `/home` pode vir com extrato `timeout`.
  Opções: aquecer o ledger (ping) no boot sem entrar na prontidão (recomendada), prazo maior na primeira chamada, ou
  calibrar na Fase 7. O `/readyz` não consulta o ledger (ADR-0002).
- **Sem imagem final do `web`** (só `dev`, com o proxy do Vite): foge da regra de imagem final por serviço. Se quiser,
  nginx não-root com digest fixado.
- **`BFF_JWT_SECRET` no seu `.env`** (obrigatório; valor de desenvolvimento no `.env.example`). Sem ele, nenhum
  `docker compose` funciona. O CI copia o `.env.example`, então não é afetado.

**Dívidas novas da Fase 2**
- O front guarda o token em `sessionStorage` (demonstração); produção usaria cookie HttpOnly emitido pelo BFF. Sem
  refresh de token (15 min e volta ao login).
- `GET /ping` continua **público** e chama o ledger; rate limit só na Fase 6.
- O BFF não cancela as chamadas ao ledger quando o cliente desconecta (elas terminam no prazo).
- OpenAPI descreve os erros como `application/json`, mas o BFF responde `application/problem+json` (sem efeito nos tipos).
- `openapi-typescript` 7.13 declara peer `typescript ^5`; o projeto usa 6.0.3 e funciona (conferido), mas gera aviso em
  `pnpm peers check`.
- O SLI da home no dashboard usa a fronteira de 250 ms do histograma; para medir 300 ms exatos seria preciso incluir essa
  fronteira nos buckets.
- Os dados de login (CPF no corpo do `POST /auth/demo-login`) nunca vão a log, span ou métrica (testado), mas o ledger
  recebe o CPF em `GetAccountByDocument`: é a mesma proteção da Fase 1 (mascarado na origem).

**Para fases seguintes**
- Estorno (novo lançamento invertido, motivo novo no enum) e referência externa no lançamento (ID da transferência),
  na Fase 3; listagem de lançamentos por janela, em streaming, na Fase 5.
- Idempotência: **sem expurgo** da tabela `idempotency_keys`, e a resposta gravada do `OpenAccount` contém o **CPF
  completo** (dado pessoal duplicado, sem retenção). Resolver juntos (ADR-0004).
- O gate do k6 roda contra o ledger de desenvolvimento (`air`), não contra a imagem final.
- A stack de observabilidade distorce a cauda de latência no Mac (CPU disputada na VM): medir carga só com `core`.

**Técnicas (herdadas da Fase 0, ainda abertas)**
- Retenção de 48 h do Tempo não observada na prática; CI sem cache de camadas da imagem `tools`.
- O interceptor Connect do BFF só instrumenta chamadas unárias.
- `psql` da imagem `tools` é o 15 contra servidor 18 (use `docker compose exec postgres psql`).
- `gen/` e `sqlcgen/` são pré-requisito do build das imagens finais; exportação de telemetria opt-in; `service.version` = `dev`.

**Resolvidas na Fase 2**: teste instável do BFF (era o `grpc-wire.test.ts`: 11 falhas em 13 execuções sob CPU
disputada; a causa era um prazo de 150 ms numa conexão HTTP/2 real e fria, que não fazia parte do que o teste prova;
agora os caminhos de sucesso usam prazo de 10 s e os de timeout usam um handler que nunca responde, `hang`, com prazo
curto; paralelismo e isolamento de prazos são provados com barreira e portão, sem `sleep`; 0 falhas em 20 execuções
sob a mesma carga); login de demonstração acha a conta (RPC no ledger); `ErrorInfo` gerado em TypeScript;
`traceparent` de entrada deixou de ser confiado (revisto com a autenticação); título do dashboard "visão geral"; CI do
GitHub corrigido (`additional_contexts`: o `up` do CI quebrava por `service:tools` num profile inativo) e **verde em
todos os jobs**, inclusive o gate do k6, que passou com os limites calibrados no Mac: **não é preciso recalibrar**.

**Resolvidas na Fase 1**: papel Postgres próprio do ledger; spans do `pgx` e métricas do pool; rótulo de status gRPC
unificado entre BFF e ledger; teste do ping que oscilava (espera com prazo).

**Desvios do plano (registrados nos ADRs)**
- Pasta `testkit/` (módulo Go) fora do layout da seção 4; pastas `deploy/tempo`, `deploy/loki`, `deploy/obs`,
  `deploy/postgres` além das listadas.
- `internal/accountotel` (adaptador de métricas de negócio) e `cmd/seed` além do layout da seção 4.
- Banco de desenvolvimento: há uma conta de teste (CPF 529.982.247-25) e contas `Carga N` criadas pelo k6; somem ao
  recriar o volume.

## 9. Próxima fase

**Fase 3: Transfers e antifraude (Go)**, modelo recomendado na seção 8 do plano; começa pelo prompt da Fase 3. Reaproveita
o ledger (débito, crédito, idempotência), o padrão de erros (`ErrorInfo`), o BFF (a `/home` ganha os limites, em seção
própria com `status`, como o extrato) e o Toxiproxy, que entra nessa fase. Pendências do ledger que a Fase 3 resolve:
estorno e referência externa no lançamento. Antes de começar, resolva as decisões da seção 8 (partida a frio, `.env`).
