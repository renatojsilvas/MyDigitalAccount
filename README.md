# Mini conta digital (Go + TypeScript)

Projeto didático: conta digital com Pix simulado. Referência: `docs/plano-conta-digital.md`.
Estado atual: `docs/estado-do-projeto.md` (criado ao fim da Fase 0).

## Pré-requisito

Apenas **Docker** com o plugin Compose. Go, Node, pnpm, buf e demais ferramentas rodam em contêiner.

## Estado da Fase 0

Em andamento. Os comandos abaixo passam a existir conforme os passos forem concluídos;
esta seção é reescrita no passo 10.

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

Porta ocupada no host? Copie `.env.example` para `.env` e troque só a `*_HOST_PORT`
(por exemplo `POSTGRES_HOST_PORT=6432`). Novas portas publicadas seguem o mesmo padrão e entram aqui.
