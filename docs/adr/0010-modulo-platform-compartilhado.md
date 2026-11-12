# ADR-0010: Módulo `platform` compartilhado entre os serviços Go

- **Status**: Aceito (Fase 3), opção do autor: módulo compartilhado.
- **Data**: 2026-11-12

## Contexto

Na Fase 3 nascem três serviços Go (`antifraude`, `spi-fake`, `transfers`), e todos precisam da mesma
infraestrutura transversal que o ledger tinha em `services/ledger/internal/platform` e em `grpcapi/server.go`:
telemetria OTLP (ADR-0002), `slog` em JSON com `trace_id` e `span_id`, servidor gRPC com os interceptors comuns
(RED, log, recuperação de panic), porta admin com `/healthz` e `/readyz`, desligamento gracioso e o subcomando de
healthcheck da imagem distroless. São cerca de 600 linhas, e por estarem em `internal/` outros módulos não podem
importá-las.

## Decisão

Um módulo Go próprio, `platform/` (`conta.local/platform`), no `go.work`, ao lado de `gen/go` e `testkit`. É o padrão
***microservice chassis***: a infraestrutura comum fica num lugar só, e cada serviço contém apenas o seu domínio.

| Pacote | O que tem |
|---|---|
| `platform/telemetry` | providers de traces, métricas e logs; exportação OTLP; métricas de runtime |
| `platform/logging` | `slog` em JSON, `trace_id`/`span_id`, redação de campos sensíveis |
| `platform/grpcserver` | servidor gRPC (otelgrpc, log por chamada, recuperação de panic), `Serve` com parada graciosa, `PingService` |
| `platform/admin` | `/healthz` e `/readyz` numa porta HTTP separada |
| `platform/healthcheck` | `Probe`, a sonda do `HEALTHCHECK` das imagens finais |
| `platform/pg` | pool do Postgres com `otelpgx` (spans e métricas do pool), `Ping` sem trace, `Migrate` (goose embutido) |

**O que não vai para a plataforma**: domínio, configuração (cada serviço tem as suas variáveis, em
`internal/platform/config`), queries e schema de cada banco (sqlc, migrations, testes de schema) e a tradução de erros de domínio para status gRPC, que entra no servidor
como interceptor (`ServerOptions.Interceptors`, entre o log e a recuperação de panic).

| | **Módulo compartilhado** (escolhido) | Cópia por serviço |
|---|---|---|
| Correção (por exemplo, na propagação do trace) | Um lugar | Quatro lugares, com risco de divergir |
| Acoplamento | Mudar a plataforma afeta todos os serviços | Nenhum entre serviços |
| Layout da seção 4 do plano | Desvio (pasta `platform/` na raiz) | Seguido à risca |

**Revisão (passo 5)**: no preview da decisão, o acesso ao Postgres aparecia "por serviço". Ao chegar o segundo serviço
com banco (spi-fake), ficou claro que abrir o pool e aplicar migrations é infraestrutura sem domínio, igual nos
serviços; foi para `platform/pg`. O que é de cada banco continua no serviço.

## Consequências

- **Desvio do layout da seção 4** (`internal/platform` por serviço): registrado aqui e no estado do projeto.
- A plataforma não pode depender de nenhum serviço, só de `gen/go` (o `PingService`). Ela entra no lint e nos testes
  como os demais módulos (`GO_MODULES` no Makefile) e é copiada no estágio de build de cada Dockerfile.
- Os serviços continuam independentes em tempo de execução: a plataforma é biblioteca, não serviço.
- Mudança de comportamento na plataforma (por exemplo, nos interceptors) muda os quatro serviços ao mesmo tempo; os
  testes de cada serviço passam a cobrir essa integração.
