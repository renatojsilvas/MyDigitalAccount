# ADR-0001: Monorepo e contrato primeiro

- **Status**: Aceito (Fase 0)
- **Data**: 2026-11-11

## Contexto

O sistema tem seis serviços Go, um BFF em TypeScript e um front-end, que se falam por gRPC (serviço a serviço), HTTP e SSE (front e BFF) e Kafka (eventos). Duas linguagens, vários donos de dado e muitas fronteiras tipadas. O risco central é o **drift de contrato**: o servidor muda um campo e o cliente só descobre em execução.

Precisamos decidir (1) onde mora o código e (2) de onde nascem os tipos que cruzam fronteiras.

## Decisão

### 1. Monorepo, com um módulo por unidade de deploy

- Um repositório só, com a estrutura da seção 4 do plano.
- **Go**: um módulo por serviço (`services/<nome>/go.mod`), amarrados por `go.work` na raiz. O `go.work` serve ao desenvolvimento local; o build de imagem de cada serviço não depende dele.
- **TypeScript**: workspace **pnpm** (`pnpm-workspace.yaml`) com `bff/`, `web/` e o pacote de contratos gerados.
- Versões de ferramenta (Go, Node, pnpm, buf, golangci-lint) fixadas na imagem `tools` (ADR-0003), que é a única fonte de versões.

### 2. Contrato primeiro, com `buf`

- Entre serviços: o `.proto` em `proto/` é a fonte. Código Go e TS é **gerado**, nunca escrito à mão.
- `buf lint` (estilo) e `buf breaking` (compatibilidade contra a `main`) rodam no CI. Quebrar compatibilidade exige decisão explícita.
- Estrutura de pacotes protobuf com versão (`conta.ledger.v1`), para poder evoluir sem quebrar.
- Plugins de geração **locais** (instalados na imagem `tools`), não os plugins remotos do BSR: o build não depende de um serviço externo e fica reprodutível.
- Entre front e BFF: o contrato é o OpenAPI gerado dos schemas Zod do BFF; os tipos do front são gerados dele (fase 2).
- Eventos do Kafka usam Protobuf do mesmo `proto/` (seção 4.2 do plano).

### 3. Código gerado não é versionado

O código gerado vai para `gen/` (ignorado pelo Git). `make generate` o gera **sobrescrevendo os arquivos no lugar** e `lint` e `test` dependem dele. O CI parte de um checkout limpo, então gera do zero; localmente, `make clean-gen generate` limpa antes (por exemplo, depois de remover um `.proto`). Não apagar `gen/` a cada geração foi uma decisão do passo 9: com o ambiente de desenvolvimento no ar, o `tsx` (BFF) e o `air` (ledger) observam esses diretórios, e apagá-los e recriá-los os derrubava e eles não voltavam (o monitor perde o diretório recriado). Sobrescrever gera eventos de escrita comuns, e os dois se recompilam sozinhos em poucos segundos. `gen/` é **excluído do golangci-lint e do ESLint**: não se faz lint de código que não escrevemos, e a correção dele vem do gerador e do `buf lint` sobre o `.proto`.

### 4. Cliente gRPC em TypeScript: Connect (`protobuf-es`)

- O BFF usa `@connectrpc/connect` e `@connectrpc/connect-node`, com `createGrpcTransport` (gRPC sobre HTTP/2): o ledger em Go não muda.
- Geração com `@bufbuild/protoc-gen-es`, instalado como **devDependency do workspace pnpm**, junto com `@bufbuild/protobuf`, ambos fixados no lockfile. O `buf.gen.yaml` aponta para `node_modules/.bin`. Os plugins Go (`protoc-gen-go`, `protoc-gen-go-grpc`) ficam na imagem `tools`.
- Motivos: `signal` (`AbortSignal`) e `timeoutMs` nativos em cada chamada, como exige o projeto; servidor falso em memória com `createRouterTransport`; mantenedor é a Buf, a mesma do `buf`; versões e cadência de release conferidas na documentação oficial.
- Custo: não há instrumentação oficial do OpenTelemetry para Connect. Um interceptor próprio cria o span de cliente (atributos `rpc.*` e status gRPC), registra a métrica de latência e erro por dependência e injeta o `traceparent` (ver ADR-0002).
- Alternativa descartada: `@grpc/grpc-js` + `ts-proto`. Tem instrumentação oficial, mas ela é experimental (0.x), e o `AbortSignal` depende de opção do ts-proto cujo suporte à saída grpc-js não foi confirmado.

## Alternativas consideradas

| Alternativa | Por que não |
|---|---|
| **Um repositório por serviço** (polirepo) | Mudar um `.proto` exigiria PRs coordenados em vários repositórios e um mecanismo para publicar os contratos. Para um projeto didático de um só dev, é custo sem ganho. |
| **Um único módulo Go** | Perde a fronteira de dependência entre serviços: um serviço poderia importar `internal/` de outro por engano. Um módulo por serviço torna isso um erro de compilação. |
| **Versionar o código gerado** | Evita rodar a geração antes de compilar e deixa o diff do contrato visível na revisão. Em troca, enche o repositório de diff mecânico e permite o arquivo gerado divergir do `.proto`. Seria razoável com um `git diff --exit-code` no CI; escolhemos a regra mais simples (gerar sempre). |
| **Plugins remotos do buf (BSR)** | Mais simples de configurar, mas exige rede e depende de terceiros em todo build. |
| **Escrever tipos à mão** | Proibido pela regra 3 do projeto. |

## Consequências

- (+) Uma mudança de contrato e seus consumidores entram no mesmo commit, e o CI as valida juntas.
- (+) `buf breaking` transforma compatibilidade em verificação automática.
- (−) Nada compila sem `make generate`. O `lint` e o `test` dependem dele, e o devcontainer roda a geração ao subir.
- (−) Monorepo exige CI que saiba rodar só o que mudou; por ora roda tudo, porque o projeto é pequeno.
- Escolhido o Connect, o `protoc-gen-es` passa a ser dependência do workspace pnpm; a geração do TS exige `pnpm install` antes do `buf generate`.
