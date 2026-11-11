# ADR-0003: Ambiente em contêineres e testes de integração

- **Status**: Aceito (Fase 0). Testes de integração: opção B.
- **Data**: 2026-11-11

## Contexto

A máquina só tem Docker. Go, Node, pnpm, buf e as demais ferramentas não existem no host e não serão instalados (regra 8). O CI deve usar as mesmas imagens e comandos do ambiente local. Os testes de integração precisam de Postgres agora, e de Redis e Kafka reais nas fases 3 e 4.

## Decisão

### 1. Imagem `tools`

Uma imagem única com Go, Node, pnpm, buf, sqlc, golangci-lint, migrations, grpcurl, psql e redis-cli, com versões fixadas. Uso: `docker compose run --rm tools <comando>`. O código entra por **bind mount**; caches (módulos Go, store do pnpm) ficam em **volumes nomeados**, para não baixar tudo a cada execução.

- O **Makefile** é só um catálogo de tarefas (`lint`, `test`, `generate`, ...) e roda dentro de `tools`. Não controla o Docker.
- **Ciclo de vida** (`up`, `down`, `down -v`) são comandos diretos do Compose, documentados no README.
- Arquivos criados pelo contêiner no bind mount não podem ficar como root no host: o `tools` roda com o UID/GID do usuário (a definir no passo da imagem).

### 2. Dois alvos por Dockerfile (multi-stage)

| Alvo | Conteúdo | Uso |
|---|---|---|
| `dev` | Ferramentas e recarga automática (`air` no Go, `tsx watch` no TS), código por volume | `docker compose up` |
| `final` | Binário estático em **distroless nonroot** (Go); Node mínimo sem root (BFF) | Imagem de entrega e do CI |

Por quê: a imagem final não leva compilador nem shell, o que reduz superfície de ataque e tamanho; rodar sem root limita o dano de uma invasão.

### 3. Compose

`compose.yaml` na raiz usando `include:` para os arquivos de `deploy/compose/` (`core`, `events`, `obs`, `load`). Versões de imagem fixadas, healthcheck em tudo, `depends_on` com `condition: service_healthy`, **profiles** para subir só o necessário e volumes nomeados para dados. Configuração por variáveis de ambiente, com `.env.example` versionado.

### 4. Testes de integração: como acessar dependências reais

> **Decisão: opção B**, aprovada. A opção A fica registrada como alternativa descartada.

**Opção A: Testcontainers, com o socket do Docker montado em `tools`**

O próprio teste sobe e derruba o Postgres (depois Redis e Kafka) via API do Docker.

- (+) Isolamento forte: cada pacote de teste pode ter sua instância limpa.
- (+) O teste é autossuficiente; o `go test` funciona sem preparar nada antes.
- (−) Monta `/var/run/docker.sock` no `tools`: quem controla o socket controla o Docker do host (equivale a root). Contraria o espírito de "o `tools` não controla o Docker".
- (−) **Rede aninhada**: o teste roda num contêiner e sobe contêineres irmãos; é preciso resolver como alcançá-los (`host.docker.internal`, `--add-host`, ou rede compartilhada), e isso difere entre Docker Desktop (Mac) e Linux do CI. Também há o contêiner auxiliar do Testcontainers (Ryuk) que precisa do socket.
- (−) Mais lento: sobe e derruba contêiner a cada execução, e o Kafka é pesado.
- (−) Versões das imagens ficam no código dos testes, fora do `deploy/compose/`, duplicando a fonte de verdade.

**Opção B (recomendada): serviços de teste dedicados no compose**

Postgres, Redis e Kafka de teste ficam como serviços do compose (versão fixada, healthcheck, `tmpfs` para os dados, portas internas, sem volume persistente). O `tools` depende deles com `condition: service_healthy`, então `docker compose run --rm tools make test` já os sobe e espera ficarem saudáveis. Os testes recebem o endereço por variável de ambiente.

- (+) Nenhum socket do Docker montado: o `tools` continua sem controle do Docker.
- (+) Mesma imagem e mesmo comando no local e no CI; as versões vivem num lugar só (`deploy/compose/`).
- (+) Subida única e rápida; os testes só se conectam.
- (+) Rede simples: o Compose resolve pelo nome do serviço.
- (−) Isolamento é **por convenção**, não por contêiner. Mitigação por dependência:
  - **Postgres**: banco por teste ou por pacote, criado a partir de um template e removido no fim.
  - **Redis**: prefixo de chave único por teste (já exigido pelo projeto) e, se preciso, um índice de banco por pacote.
  - **Kafka**: tópicos e consumer groups com nome único por teste.
- (−) Quem roda `go test` direto, fora do compose, não terá as dependências. Aceito: o caminho oficial é sempre pelo `tools`.
- (−) Estado vaza se um teste não limpar. Mitigação: os helpers de teste (um pacote compartilhado) criam e removem os recursos, e o `tmpfs` zera tudo a cada ciclo.

A mesma estratégia vale para Postgres (fase 0 em diante), Redis (fase 3) e Kafka (fase 4).

**Ponto a verificar na implementação:** como combinar `depends_on` com profiles do Compose para que `run --rm tools` suba só as dependências de teste. Será conferido na documentação da versão do Compose antes de escrever o arquivo.

### 5. Decisões do passo 6 (Dockerfiles e compose)

- **O alvo `dev` reaproveita a imagem `tools`** (contexto adicional `tools: service:tools`), em vez de repetir Go, Node e `air` com outras versões. Assim `tools` continua a única fonte de versões; o alvo `final` também compila no estágio `tools`.
- **Alvo `final`**: ledger em `distroless/static-debian13:nonroot` (18 MB, sem shell, sem root); BFF em `node:24-bookworm-slim` com `USER node` e só dependências de produção (`pnpm deploy`). Bases fixadas por tag **e digest**.
- **Healthcheck dentro do Dockerfile**, um por alvo: `curl` no `dev` e, no `final`, o subcomando `ledger healthcheck` (distroless não tem shell nem curl) e um `fetch` do Node. O compose herda. A sonda usa `/readyz`, que é o que `depends_on: service_healthy` precisa.
- **Arquivos incluídos pelo compose são autocontidos**: o Compose carrega cada um isoladamente, então `tools`, Postgres, ledger e bff vivem juntos em `core.yaml`, e `obs`, `events` e `load` serão arquivos independentes.
- **Postgres 18**: o volume é montado em `/var/lib/postgresql` (o diretório de dados passou a ser `/var/lib/postgresql/18/docker`). Um banco por serviço, criado por script de init que só roda na primeira inicialização do volume.
- **O `psql` do `tools` é o 15 do Debian**; para uso interativo, use o `psql` do próprio contêiner do Postgres (`docker compose exec postgres psql`), que sempre casa com a versão do servidor.
- **`gen/` é pré-requisito do build das imagens** (`make generate` antes). O build não gera o código para não duplicar o pipeline; o CI roda `generate` primeiro.

## Alternativas consideradas (decisões 1 a 3)

| Alternativa | Por que não |
|---|---|
| **Instalar as ferramentas no host** (asdf, brew) | Proibido pela regra 8; e o "funciona na minha máquina" é o problema que o contêiner resolve. |
| **Só devcontainer, sem imagem `tools` no compose** | O devcontainer é opcional e específico de editor; o CI e o terminal precisam do mesmo ambiente sem editor. O devcontainer **reaproveita** a imagem `tools`. |
| **Imagem única para dev e produção** | Imagem grande, com compilador e shell, e geralmente rodando como root. |
| **`make` no host para orquestrar o Docker** | O host não tem `make`; e o Makefile dentro do `tools` evita misturar tarefa com ciclo de vida. |
| **Alpine em vez de distroless** | Alpine traz shell e gerenciador de pacotes (úteis, mas superfície maior). Distroless fica mais mínimo; para depurar, usa-se a imagem `dev`. |

## Consequências

- **Testes de falha das fases 3 e 4** (Redis e Kafka fora do ar, lentos ou com conexão cortada) usarão o **Toxiproxy como serviço do compose**: o teste se conecta à dependência por meio do proxy e injeta a falha pela API HTTP do Toxiproxy. É a consequência direta da opção B, já que o `tools` não controla o Docker e portanto não pode parar um contêiner. O Toxiproxy **só entra no compose na fase 3**; a fase 0 não o cria.
- (+) Em uma máquina nova, só o Docker basta; `docker compose run --rm tools make lint test` é o comando único de verificação.
- (+) Ambiente reproduzível entre máquina local e CI.
- (−) Os volumes de cache e o bind mount tornam o desempenho de I/O no Docker Desktop (Mac) um ponto de atenção; se ficar lento, avaliamos otimizações.
- (−) A pilha completa exige memória (reservar 8 GB ou mais para o Docker); os profiles permitem subir só uma parte.
