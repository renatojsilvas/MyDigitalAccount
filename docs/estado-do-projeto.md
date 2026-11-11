# Estado do projeto

Atualizado ao fim da **Fase 0 (Fundação)**, em 2026-11-11. Leia este arquivo no início de cada fase.
Referências: `docs/plano-conta-digital.md` (plano), `docs/arquitetura-conta-digital.md` (diagrama),
`docs/adr/` (decisões).

## 1. Fases

| Fase | Estado |
|---|---|
| 0. Fundação | **Concluída** |
| 1. Ledger (Go) | Próxima |
| 2 a 8 | Pendentes |

**Critério de pronto da Fase 0**, verificado: em um **clone limpo** (sem `.env`, `gen/` ou `node_modules`),
`docker compose run --rm tools make lint test` passa nas duas stacks (código 0, 78 s com cache de imagens), e
`docker compose run --rm tools make obs-e2e` prova que um `/ping` do BFF ao ledger é **um único trace**
(`bff servidor -> bff cliente gRPC -> ledger servidor`), com logs dos dois serviços ligados ao mesmo
`trace_id`. O dashboard no Grafana foi conferido visualmente pelo usuário, que confirmou que ele funciona.

## 2. O que existe

- **ledger** (Go): só o `Ping`, mais a base de plataforma: config validada no boot, `slog` com `trace_id`, OpenTelemetry
  (traces, métricas, logs) por OTLP, porta admin `:8081` (`/healthz`, `/readyz` que consulta o Postgres), gRPC `:50051` com
  `grpc.health.v1`, graceful shutdown, pool `pgx` aberto no boot (falha cedo) e subcomando `ledger healthcheck`.
- **bff** (TypeScript, Fastify): `GET /ping` (chama o ledger por gRPC com Connect), `/healthz`, `/readyz`, config com Zod,
  telemetria própria (span de servidor, interceptor do cliente gRPC, métricas RED e USE, logs `pino` reenviados ao OTel).
- **Infra em contêineres**: imagem `tools`, Postgres, Postgres de teste, e a stack de observabilidade (Collector, Prometheus,
  Tempo, Loki, Grafana).
- **Não existe ainda**: domínio (contas, lançamentos, Pix), migrations, `sqlc`, autenticação, Kafka, Redis, os demais serviços
  (`transfers`, `antifraude`, `spi-fake`, `notifications`, `reconciler`), front-end, k6, Alertmanager.

## 3. Árvore de arquivos (versionados)

```
.devcontainer/devcontainer.json   VS Code dentro do contêiner tools (NÃO testado: sem editor na sessão)
.github/workflows/ci.yml          CI: verify (lint, testes, buf breaking, imagens), e2e-observability (main), vuln (semanal)
.vscode/extensions.json           extensões recomendadas
.golangci.yml                     golangci-lint v2 (gen/ excluído)
.env.example  .dockerignore  .gitignore
Makefile                          generate, clean-gen, proto-lint, go-lint, go-test, ts-lint, ts-test, vuln, obs-smoke, obs-e2e, lint, test
buf.yaml  buf.gen.yaml            buf v2: lint STANDARD, breaking FILE; plugins Go locais e protoc-gen-es do workspace
compose.yaml                      ponto de entrada; inclui deploy/compose/core.yaml e obs.yaml
go.work  go.work.sum              módulos: gen/go, services/ledger, testkit
package.json  pnpm-workspace.yaml  pnpm-lock.yaml   workspace pnpm (bff, web, gen/ts); catalog de versões; allowBuilds negado
proto/conta/ping/v1/ping.proto    único contrato
tools/Dockerfile                  imagem tools: ÚNICA fonte das versões de ferramenta
tools/templates/                  go.mod, package.json e tsconfig de gen/ (gen/ não vai para o Git)
testkit/testpg/                   helper: banco Postgres isolado e descartável por teste (módulo Go próprio)
services/ledger/
  Dockerfile  .air.toml           alvos dev (tools + air) e final (distroless nonroot)
  cmd/ledger/                     main.go (composition root), healthcheck.go, e testes
  internal/grpcapi/               servidor gRPC, interceptors, handler do ping
  internal/platform/              admin, config, logging, postgres, telemetry
services/{transfers,antifraude,spi-fake,reconciler,notifications}/   vazios (.gitkeep)
bff/
  Dockerfile                      alvos dev (tools + tsx watch) e final (Node slim, usuário node)
  src/                            app.ts (composition root), server.ts, main.ts, config.ts, errors.ts, result.ts, lifecycle.ts
  src/clients/ledger.ts           único lugar que conhece gRPC
  src/modules/ping/               route, schema, service, mapper
  src/platform/                   telemetry, logger, connect-telemetry (interceptor), runtime-metrics
  src/plugins/                    telemetry, errors, health
  test/                           harness, ping, grpc-wire (gRPC real sobre HTTP/2), server
deploy/compose/core.yaml          tools, postgres, postgres-test, ledger, bff
deploy/compose/obs.yaml           otel-collector, prometheus, tempo, loki, grafana
deploy/obs/                       Dockerfile da sonda (busybox), smoke.sh, e2e-trace.sh
deploy/{otel,prometheus,tempo,loki}/   configuração de cada peça
deploy/grafana/                   provisioning (datasources, provedor de dashboards) e dashboards/visao-geral.json
deploy/postgres/init/             cria o banco do ledger na primeira inicialização
deploy/alertmanager/  docs/runbooks/  load/  web/   vazios (.gitkeep): fases 6, 7 e 2
docs/                             plano, arquitetura, estado, ADRs (0001 a 0003)
```

## 4. Contratos `.proto`

`proto/conta/ping/v1/ping.proto` (pacote `conta.ping.v1`):

```proto
service PingService { rpc Ping(PingRequest) returns (PingResponse); }
message PingRequest  { string message = 1; }
message PingResponse { string message = 1; string service = 2; }
```

Código gerado em `gen/go` (módulo `conta.local/gen/go`) e `gen/ts` (pacote `@conta/contracts`, compilado para `dist/`).
Os nomes de módulo `conta.local/...` são fictícios (não há remoto Git ainda).

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
- Tempo 3.x monolítico (`-target=all`), disco local, sem Kafka; Loki em instância única; retenção de 48 h (Tempo), 72 h (Loki), 7 d (Prometheus).
- Healthcheck de imagens sem shell com um `busybox` estático copiado; Grafana sem login só em `127.0.0.1`.
- `/readyz` do BFF não consulta o ledger (degradação parcial na Fase 2); o do ledger consulta o Postgres.

**ADR-0003, ambiente em contêineres**
- Imagem `tools` com tudo (Go 1.27.4, Node 24.22.2, pnpm 12.18.0, buf 1.75.0, sqlc 1.31.1, golangci-lint 2.15.0, goose 3.30.0, grpcurl 1.9.4, air 1.67.4, govulncheck 1.9.0); `GOTOOLCHAIN=local`.
- Dois alvos por Dockerfile: `dev` (reaproveita a imagem `tools`) e `final` (distroless nonroot / Node slim, bases por tag e digest).
- Compose por grupo, incluído pela raiz e autocontido; portas publicadas só em `127.0.0.1` por `*_HOST_PORT`.
- Testes de integração: serviços de teste no compose (opção B), com `tools` dependendo do `postgres-test` por `service_healthy` (profiles `tools` e `test`); isolamento por banco.
- Testes de falha das fases 3 e 4 (Redis e Kafka fora do ar) usarão o Toxiproxy como serviço do compose, que só entra na Fase 3.

## 6. Como rodar e testar

```bash
cp .env.example .env     # defina HOST_UID e HOST_GID (id -u, id -g); POSTGRES_HOST_PORT etc. se houver conflito
docker compose run --rm tools make lint test                        # lint e testes (gera gen/ antes)
docker compose --profile core up -d --build                         # Postgres, ledger, bff
curl "http://127.0.0.1:8088/ping?message=oi"
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317 \
  docker compose --profile core --profile obs up -d --build         # com observabilidade
docker compose run --rm tools make obs-e2e                          # critério de pronto: um ping, um trace
docker compose --profile '*' down                                   # para tudo
```

Contagem de testes: Go, 8 pacotes (`-race`); BFF, 41 testes Vitest. Detalhes e portas: `README.md`.

## 7. Métricas, logs e alertas

**Alertas: nenhum.** O Alertmanager e os runbooks (`docs/runbooks/`) entram na Fase 6.

**Métricas no Prometheus** (nomes como chegam do Collector; `job` = serviço, `instance` = contêiner):

| Origem | Métrica | Rótulos |
|---|---|---|
| BFF | `http_server_request_duration_seconds_{bucket,count,sum}` | `http_request_method`, `http_route` (`unmatched` se não existe), `http_response_status_code` |
| BFF | `rpc_client_duration_seconds_*` (latência e erro por dependência) | `dependency`, `rpc_service`, `rpc_method`, `rpc_grpc_status_code` (numérico) |
| BFF | `nodejs_eventloop_delay_p99_seconds`, `nodejs_heap_used_bytes` | |
| ledger | `rpc_server_call_duration_seconds_*` | `rpc_method`, `rpc_response_status_code` (por nome), `rpc_system_name` |
| ledger | `go_goroutine_count`, `go_memory_used_bytes` e demais `go_*` do runtime | `go_memory_type` |
| stack | `up` dos 5 componentes | `job` |

As sondas de saúde ficam fora dos traces e das métricas de negócio. **Logs**: JSON no stdout e OTLP para o Loki, com
`trace_id` e `span_id`; um log por requisição no BFF, um por chamada gRPC no ledger. CPF, token e chave Pix são mascarados.

**Dashboard**: `Conta Digital: visão geral` (stack, RED do BFF, ledger visto pelo BFF, RED do ledger, USE, logs, traces).

## 8. Pendências e dívidas conhecidas

**Para você fechar**
- O CI e o `.devcontainer` **nunca rodaram**: o CI foi validado com `actionlint` (e o `buf breaking` testado localmente nos dois sentidos), mas não há remoto no GitHub ainda; o devcontainer só teve o JSON validado.

**Técnicas**
- A retenção de 48 h do Tempo é aceita pelo `-config.verify`, mas seu **efeito** não foi observado (exigiria esperar 48 h).
- As métricas de status gRPC diferem entre BFF (`rpc_grpc_status_code` numérico) e ledger (`rpc_response_status_code` por nome); unificar quando as versões da convenção convergirem.
- O CI reconstrói a imagem `tools` a cada execução (alguns minutos); falta cache de camadas (`buildx` com `type=gha`).
- O interceptor Connect do BFF só instrumenta chamadas unárias; streaming apenas propaga o `traceparent`.
- Um `traceparent` de entrada é aceito de qualquer cliente no BFF; revisar na Fase 2 (autenticação) e na 6 (borda pública).
- Um único usuário Postgres (`conta`) para tudo; papéis por serviço entram na Fase 1.
- O `psql` da imagem `tools` é o 15 (Debian) contra um servidor 18; use `docker compose exec postgres psql` para uso interativo.
- `gen/` é pré-requisito do build das imagens finais (o build não gera o código); o CI roda `generate` antes.
- A exportação de telemetria é opt-in; sem Collector no ar mas com o endpoint definido, o export falha com ruído.
- Ledger sem métricas do pool de conexões nem spans do `pgx` (Fase 1).
- `service.version` fica `dev` (sem pipeline de versão).

**Desvios do plano (registrados nos ADRs)**
- Pasta `testkit/` (módulo Go) fora do layout da seção 4, para os helpers de teste serem compartilhados entre serviços.
- Pastas `deploy/tempo`, `deploy/loki`, `deploy/obs` e `deploy/postgres` além das listadas na seção 4.
- O ledger já depende do Postgres na Fase 0 (pool no boot e `/readyz`): o mínimo para o teste de integração ter o que provar; domínio e migrations são da Fase 1.

## 9. Próxima fase

**Fase 1: Ledger (Go)**, modelo recomendado Claude Opus 5.5 (seção 8 do plano). Antes do código, o plano pede ADRs sobre a
partida dobrada, o controle de concorrência no saldo (lock pessimista, otimista ou saldo derivado) e a idempotência, com
as opções e uma recomendação, aguardando decisão. Reaproveita desta fase: `testkit/testpg` para os testes de integração,
`internal/platform` (config, logging, telemetry, postgres, admin) e a imagem `tools` (`sqlc` e `goose` já estão nela).
