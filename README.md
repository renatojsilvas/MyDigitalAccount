# Mini conta digital (Go + TypeScript)

Projeto didático: uma conta digital com Pix simulado, para aprender na prática uma stack comum em
fintechs (Go nos serviços de domínio, TypeScript/Node no BFF), com observabilidade desde a primeira fase.

- Plano, arquitetura e fases: [`docs/plano-conta-digital.md`](docs/plano-conta-digital.md) e
  [`docs/arquitetura-conta-digital.md`](docs/arquitetura-conta-digital.md)
- **Onde o projeto está agora**: [`docs/estado-do-projeto.md`](docs/estado-do-projeto.md)
- Decisões de arquitetura: [`docs/adr/`](docs/adr/)

**Fases 0 (Fundação) e 1 (Ledger) concluídas.** O `ledger` em Go tem contas, lançamentos em partida dobrada,
saldo e extrato, com idempotência em todas as escritas e lock pessimista no saldo (ADR-0004), exposto por
gRPC; o BFF em TypeScript ainda só tem o `/ping` (a home é a Fase 2). Tudo instrumentado (logs, métricas,
traces) e com um gate de desempenho (k6) no CI.

## Pré-requisito

Apenas **Docker** com o plugin Compose (e Git). Go, Node, pnpm, buf e as demais ferramentas rodam em
contêiner; **nada é instalado na máquina**. Medido na Fase 0: a stack inteira (core, obs e o Postgres de
teste, 9 contêineres) usa cerca de **0,9 GB ociosa**; compilações e testes usam mais. O desenvolvimento foi
feito com ~7,7 GB no Docker Desktop. Em máquina apertada, suba a observabilidade só quando for usá-la.

## Primeiros passos

```bash
cp .env.example .env
# edite o .env: HOST_UID e HOST_GID com `id -u` e `id -g` (no Mac costuma ser 502 e 20)

docker compose run --rm tools make lint test        # critério de pronto: lint e testes das duas stacks
```

A primeira execução constrói a imagem `tools` (alguns minutos) e sobe, sozinho, um Postgres de teste.

## Comandos do dia a dia

Tarefas rodam **dentro** do contêiner `tools` (`docker compose run --rm tools make <alvo>`). Subir,
parar e apagar o ambiente são comandos diretos do Compose, porque o `tools` não controla o Docker.

| Quero... | Comando |
|---|---|
| Gerar o código dos `.proto` (Go e TS) | `docker compose run --rm tools make generate` |
| Lint e testes de tudo | `docker compose run --rm tools make lint test` |
| Só os testes / só o lint | `... make test` / `... make lint` |
| Vulnerabilidades nas dependências Go | `docker compose run --rm tools make vuln` |
| Criar as contas de demonstração (ledger no ar; idempotente) | `docker compose run --rm tools make seed` |
| Gate de desempenho do ledger (k6) | `docker compose --profile core up -d --wait ledger` e `docker compose --profile load run --rm k6` |
| Subir Postgres, ledger e BFF (recarga automática) | `docker compose --profile core up -d --build` |
| Subir a observabilidade | `docker compose --profile obs up -d --build` |
| Ver o estado | `docker compose --profile core --profile obs ps` |
| Parar (mantém os dados) | `docker compose --profile core --profile obs down` |
| Recriar só o banco (mantém o cache do Go) | `docker compose --profile core down && docker volume rm conta-digital_postgres-data` |
| Parar e apagar os dados | `docker compose --profile '*' down -v` |
| Parar **tudo**, inclusive o Postgres de teste | `docker compose --profile '*' down` |

**A imagem `tools` precisa existir antes do `up`.** Os Dockerfiles de desenvolvimento do ledger e do BFF
copiam as ferramentas dela (`additional_contexts` apontando para `docker-image://conta-digital/tools:dev`). Não
dá para apontar para o *serviço* `tools`: ele está num profile próprio, e um profile inativo é, para o Compose, um
serviço inexistente (foi o que quebrou o `up` no CI). O `make generate` (ou `docker compose build tools`) já
constrói a imagem, e ele é o primeiro passo de qualquer fluxo, local ou de CI.

Experimente:

```bash
docker compose --profile core up -d --build
curl "http://127.0.0.1:8088/ping?message=oi"          # BFF -> ledger: {"echo":"oi","servedBy":"ledger"}

# contas de demonstração (Ana, Bruno, Carla e Diego; o log mostra os account_id)
docker compose run --rm tools make seed

# chamar o ledger direto por gRPC (o ledger não tem reflection: o .proto descreve o serviço)
docker compose run --rm tools grpcurl -plaintext -import-path proto -proto conta/ledger/v1/ledger.proto \
  -d '{"account_id":"<account_id do seed>"}' ledger:50051 conta.ledger.v1.LedgerService/GetBalance

docker compose exec postgres psql -U conta -d ledger   # psql interativo (usa o psql do próprio servidor)
```

## Observabilidade

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317 \
  docker compose --profile core --profile obs up -d --build
docker compose run --rm tools make obs-e2e    # um /ping vira UM trace e os logs se ligam a ele
```

Os serviços **só exportam telemetria se** `OTEL_EXPORTER_OTLP_ENDPOINT` estiver definido (descomente no
`.env`, ou passe na linha de comando); sem ele, nada é exportado e os logs seguem no stdout.

- **Grafana**: `http://127.0.0.1:3000`, sem login e só em `127.0.0.1` (decisão de desenvolvimento local).
  Datasources e os dashboards "Conta Digital: visão geral" e "Conta Digital: ledger" (RED, negócio, pool de
  conexões, logs e traces) são provisionados como código. Nos logs, clique
  no `trace_id` para saltar ao trace no Tempo.
- **Prometheus**: `http://127.0.0.1:9090`.
- `make obs-smoke` prova que traces, métricas e logs chegam a Tempo, Prometheus e Loki.
- No Mac, a stack de observabilidade disputa CPU com os serviços e distorce a cauda de latência: meça carga só
  com o profile `core` ([`load/README.md`](load/README.md)).

## Código gerado

`gen/` (código do `.proto`) e `services/*/internal/postgres/sqlcgen/` (código do sqlc) não vão para o Git
(ADR-0001, ADR-0004) e são pré-requisito de lint, testes e build das imagens; `make generate` cria os dois.
`make generate` o **sobrescreve no lugar**; com o ambiente de desenvolvimento no ar, o BFF e o ledger
se recompilam sozinhos em poucos segundos. Para gerar do zero (por exemplo, ao remover um `.proto`):

```bash
docker compose run --rm tools make clean-gen generate
docker compose --profile core restart bff ledger
```

## Testes de integração

`make test` sobe o `postgres-test` (Postgres em memória, sem porta publicada) e espera ele ficar saudável.
Ele **continua rodando** depois, para os testes seguintes serem rápidos (pare-o com `down` acima). Cada
teste recebe um banco próprio, criado e removido pelo helper `testkit/testpg`. Rodar `go test` fora do
`tools` pula esses testes, com a instrução de como rodá-los.

## Imagens finais

Mínimas e sem root (ledger em distroless, ~18 MB; BFF em Node slim). Exigem `make generate` antes:

```bash
docker compose build tools
docker build -f services/ledger/Dockerfile --target final \
  --build-context tools=docker-image://conta-digital/tools:dev -t conta-digital/ledger:final .
docker build -f bff/Dockerfile --target final \
  --build-context tools=docker-image://conta-digital/tools:dev -t conta-digital/bff:final .
```

## Portas

Há dois tipos, e não se confundem:

- **Internas**, na rede do compose (`POSTGRES_PORT=5432`, `BFF_PORT=8088`): fixas; é como os serviços se falam.
- **Publicadas no host**, só em `127.0.0.1`, parametrizadas por `*_HOST_PORT`:
  `"127.0.0.1:${POSTGRES_HOST_PORT:-5432}:5432"`.

| Variável | Padrão | Para quê |
|---|---|---|
| `POSTGRES_HOST_PORT` | 5432 | `psql` e ferramentas na máquina |
| `BFF_HOST_PORT` | 8088 | BFF (escuta 8088 por dentro) |
| `GRAFANA_HOST_PORT` | 3000 | Grafana |
| `PROMETHEUS_HOST_PORT` | 9090 | Prometheus |
| `LEDGER_GRPC_HOST_PORT` | 50051 | gRPC do ledger (`grpcurl` a partir do host) |
| `LEDGER_ADMIN_HOST_PORT` | 8081 | `/healthz` e `/readyz` do ledger |

Porta ocupada no host? Troque só a `*_HOST_PORT` no `.env` (por exemplo `POSTGRES_HOST_PORT=6432`).
Novas portas publicadas seguem o mesmo padrão e entram nesta tabela.

## CI e editor

- **CI** (`.github/workflows/ci.yml`): usa as mesmas imagens e comandos. A cada push e PR roda
  `make lint test`, o `buf breaking` contra a branch base (só em PR) e a compilação das imagens finais.
  A cada push e PR roda também o gate de desempenho (k6 contra o ledger; o resumo vira artefato). Em `main`
  (ou sob demanda) roda `make obs-e2e`; toda segunda-feira, o `govulncheck`.
- **Editor**: `.devcontainer/` abre o VS Code dentro do contêiner `tools` (autocompletar e lint sem instalar
  as linguagens). `.vscode/extensions.json` recomenda as extensões.

## Estrutura

```
proto/            contratos (buf); gen/ é gerado, fora do Git
services/ledger/  Go: contas, lançamentos, saldo e extrato (domínio em internal/account); demais serviços nas suas fases
bff/              TypeScript (Fastify): agrega e protege
testkit/          helpers de teste compartilhados (Postgres isolado por teste)
tools/            Dockerfile da imagem de ferramentas (única fonte de versões)
deploy/           compose por grupo (core, obs, load), OTel, Prometheus, Tempo, Loki, Grafana
load/             roteiros k6 (linha de base do ledger) e a origem dos thresholds
docs/             plano, arquitetura, ADRs, estado do projeto
```
