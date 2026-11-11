# Mini conta digital (Go + TypeScript)

Projeto didático: conta digital com Pix simulado. Referência: `docs/plano-conta-digital.md`.
Estado atual: `docs/estado-do-projeto.md` (criado ao fim da Fase 0).

## Pré-requisito

Apenas **Docker** com o plugin Compose. Go, Node, pnpm, buf e demais ferramentas rodam em contêiner.

## Estado da Fase 0

Em andamento; este README é reescrito no passo 10. Já funciona:

```bash
export HOST_UID=$(id -u) HOST_GID=$(id -g)                # ou defina no .env
cp .env.example .env                                      # uma vez; ajuste as portas se precisar

docker compose run --rm tools make generate               # gera gen/ a partir do .proto (obrigatório antes de build)
docker compose run --rm tools make lint test              # lint e testes das duas stacks

docker compose --profile core up -d --build               # Postgres, ledger e BFF, com recarga automática
curl "http://127.0.0.1:${BFF_HOST_PORT:-8088}/ping?message=oi"
docker compose --profile core ps                          # todos devem aparecer (healthy)
docker compose --profile core down                        # para (mantém os dados)
docker compose --profile core down -v                     # para e apaga os dados
docker compose --profile '*' down                         # para TUDO, inclusive o Postgres de teste
```

### Testes de integração

`docker compose run --rm tools make test` sobe sozinho o `postgres-test` (Postgres em memória,
sem porta publicada) e espera ele ficar saudável antes de rodar. Ele **continua rodando** depois
do comando, para os testes seguintes serem rápidos; pare-o com `docker compose --profile '*' down`.
Cada teste recebe um banco próprio (`testkit/testpg`), criado e removido por ele. Rodar `go test`
fora do `tools` pula esses testes, com a instrução de como rodá-los.

Comandos úteis:

```bash
docker compose exec postgres psql -U conta -d ledger      # psql interativo (usa o psql do próprio servidor)
docker compose run --rm tools grpcurl -plaintext ledger:50051 list
```

Imagens finais (mínimas, sem root; o `make generate` precisa ter rodado):

```bash
docker compose build tools
docker build -f services/ledger/Dockerfile --target final \
  --build-context tools=docker-image://conta-digital/tools:dev -t conta-digital/ledger:final .
docker build -f bff/Dockerfile --target final \
  --build-context tools=docker-image://conta-digital/tools:dev -t conta-digital/bff:final .
```

## Portas

Há dois tipos de porta, e não se confundem:

- **Internas**, na rede do compose (ex.: `POSTGRES_PORT=5432`, `BFF_PORT=8088`): fixas; é como os serviços se falam.
- **Publicadas no host**, só em `127.0.0.1`, parametrizadas por `*_HOST_PORT` com padrão no compose:
  `"127.0.0.1:${POSTGRES_HOST_PORT:-5432}:5432"`.

| Variável | Padrão | Para quê |
|---|---|---|
| `POSTGRES_HOST_PORT` | 5432 | `psql` e ferramentas na máquina |
| `BFF_HOST_PORT` | 8088 | BFF (escuta 8088 por dentro) |
| `GRAFANA_HOST_PORT` | 3000 | Grafana |
| `PROMETHEUS_HOST_PORT` | 9090 | Prometheus |
| `LEDGER_GRPC_HOST_PORT` | 50051 | gRPC do ledger (`grpcurl` a partir do host) |
| `LEDGER_ADMIN_HOST_PORT` | 8081 | `/healthz` e `/readyz` do ledger |

Porta ocupada no host? Copie `.env.example` para `.env` e troque só a `*_HOST_PORT`
(por exemplo `POSTGRES_HOST_PORT=6432`). Novas portas publicadas seguem o mesmo padrão e entram aqui.
