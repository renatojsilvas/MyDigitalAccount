# ADR-0006: Login de demonstração, JWT e confiança no `traceparent`

- **Status**: Aceito (Fase 2), com os ajustes do autor: opção A no item 1 e CPF digitado no front, cópia dos `.proto` só para TS, `traceparent` guardado como span link
- **Data**: 2026-11-12

## Contexto

A Fase 2 introduz autenticação no BFF. Três pontos pedem decisão:

1. O login de demonstração precisa descobrir a conta do usuário, mas os `account_id` (UUIDv7 gerados pelo ledger)
   **mudam sempre que o banco é recriado**.
2. Como emitir e validar o JWT.
3. Hoje o BFF aceita o `traceparent` de **qualquer cliente** como pai do span (dívida das Fases 0 e 1). Agora que
   existe borda autenticada, isso precisa ser revisto.

Há também um pré-requisito técnico: o BFF precisa ler `google.rpc.ErrorInfo` dos erros do ledger, e esse tipo tem de
ser **gerado**, não escrito à mão (item 4).

## Decisão

### 1. Como achar a conta do usuário (decisão sua)

| | **A. RPC `GetAccountByDocument` no ledger** (recomendada, sua preferência) | **B. Mapeamento CPF → `account_id` no BFF** |
|---|---|---|
| Dono do dado | Ledger (já é o dono de contas e CPFs) | BFF passa a ter uma segunda fonte da verdade |
| Banco recriado | Funciona sem mudança | Mapeamento fica obsoleto; precisa ser regerado a cada seed |
| Contrato | Mudança **aditiva**: `buf breaking` não reclama | Nenhuma |
| Custo | Uma RPC, uma query (o `UNIQUE` do CPF já é o índice), testes no ledger | Arquivo/variável de ambiente mantidos junto com o seed, ou o BFF listaria contas |
| Risco | Um endpoint que traduz CPF em conta e nome. Aceito: só na rede interna, o CPF vai no corpo (nunca em log, mascarado na origem) e a chamada só é feita pelo login | Nenhum novo, mas o desvio de regra é permanente |

**Recomendação: A.** O mapeamento do B é exatamente o tipo de dado que a regra 1 manda manter no dono. Para a
fidelidade ao padrão: o login chama o ledger, que responde com a conta; o BFF só coloca o `account_id` no token.

Se você aprovar A, o desenho é:

- `rpc GetAccountByDocument(GetAccountByDocumentRequest) returns (GetAccountByDocumentResponse)` (reaproveita a
  mensagem `Account`), com `holder_document` só dígitos; `NOT_FOUND` (`ACCOUNT_NOT_FOUND`) se não houver conta,
  `INVALID_ARGUMENT` se o CPF for inválido. Leitura, portanto sem chave de idempotência (a regra 4 vale para escritas).
- Métrica e span do ledger como qualquer RPC (o interceptor já cobre); CPF mascarado em log.
- **No front**: o usuário **digita o CPF** no login de demonstração. O `web/` não lê o `demo-accounts.json` do seed
  (não se acopla o front a um arquivo interno do ledger). O BFF não conhece nenhuma lista de usuários.

### 2. JWT

- **Biblioteca**: `@fastify/jwt` 10.x (plugin oficial do Fastify; mantida, última versão publicada em 2026-11-02),
  sobre `fast-jwt`. Alternativa: `jose` (ótima, mas exigiria escrever o plugin e o decorator à mão, e aqui o ponto
  didático é justamente o plugin encapsulado do Fastify).
- **Algoritmo fixado em HS256**, com a lista de algoritmos aceitos explícita (nunca "o que o token disser").
  Segredo simétrico basta: quem emite e quem valida é o mesmo serviço. Com mais de um emissor, passa a RS256/EdDSA.
- **Claims**: `sub` = `account_id`, `iss`, `aud`, `iat`, `exp` (15 min, sem refresh: é demonstração). **O CPF não
  entra no token** (minimização de dado pessoal; o token circula em cabeçalhos e pode vazar em ferramentas).
- **Segredo em variável de ambiente** (`BFF_JWT_SECRET`, mínimo de 32 caracteres), validado no boot (regra 6). Sem
  valor padrão, nem em desenvolvimento: o `.env.example` traz um valor óbvio de exemplo e o compose o repassa.
- **Autorização**: a rota `/home` usa **só** o `sub` do token; o cliente não informa `account_id` em lugar nenhum
  (evita IDOR, *Insecure Direct Object Reference*).
- **Login de demonstração**: `POST /auth/demo-login` com `{ document }`. Só é registrado quando
  `ENVIRONMENT` ∈ {`development`, `test`}; em `production` a rota **não existe** (404) e, além disso, o boot falha se
  `BFF_DEMO_LOGIN=true` com `ENVIRONMENT=production`. Sem senha: é uma ferramenta de demonstração, e isso fica
  escrito no OpenAPI.
- **Token nunca em log nem trace**: o log da requisição não registra cabeçalhos; falha de autenticação sai como
  `401` `problem+json` genérico (não diz se o token expirou, estava malformado ou com assinatura errada, só o motivo
  vai ao log).
- A forma de autenticar o stream SSE (o `EventSource` do navegador não envia cabeçalho `Authorization`) fica para a
  **Fase 4**, em ADR próprio, como já prevê o plano.

### 3. `traceparent` de entrada

**O problema.** O BFF é a borda pública. Aceitar o `traceparent` de qualquer cliente permite que ele escolha o
`trace_id` (poluir ou colidir com traces alheios, injetar IDs em logs) e force a flag de amostragem (empurrar volume
para o backend de traces). A própria especificação W3C Trace Context permite reiniciar o trace na fronteira de
confiança.

| Opção | Efeito |
|---|---|
| **a. Ignorar o `traceparent` de entrada na borda e iniciar um trace novo** (recomendada) | O `trace_id` é sempre nosso. O que o cliente mandou não é usado |
| b. Aceitar só de origem confiável (allowlist de IP ou de um gateway) | Faz sentido quando houver um API gateway à frente; hoje não há, e seria configuração sem uso |
| c. Aceitar e sanitizar (ex.: zerar a flag de amostragem) | Ainda permite escolher o `trace_id` |

**Decisão: a.** O BFF cria o span de servidor como raiz. Se a requisição trouxer um `traceparent` válido, o contexto
recebido é guardado como **span link** no trace novo (recomendação do OpenTelemetry para contexto não confiável): dá
para correlacionar sem herdar o `trace_id` nem a amostragem do cliente. Internamente (BFF → ledger e entre os serviços Go) o
`traceparent` continua propagado normalmente, pois a rede interna é confiável. O front não faz tracing nesta fase. Se
um dia houver um gateway ou um front instrumentado e confiável, a opção b entra por ADR. O `trace_id` é devolvido no
corpo dos erros (já é) e também no cabeçalho de resposta `x-trace-id`, o que permite o suporte achar o trace sem o
cliente ter que enviá-lo. Teste: requisição com `traceparent` forjado gera span com `trace_id` diferente e o contexto forjado como link. O
`deploy/obs/e2e-trace.sh` deixa de esperar a continuação do trace injetado e acha o trace pelo `x-trace-id` da resposta.

### 4. `google.rpc.ErrorInfo` em TypeScript, gerado

O ledger devolve `google.rpc.Status` com `ErrorInfo` nos detalhes (ADR-0004). Para o BFF decodificá-lo é preciso o
descritor desses tipos.

- **Decisão**: trazer `google/rpc/error_details.proto` e `google/rpc/status.proto` do repositório oficial
  `googleapis/googleapis` para `proto/google/rpc/`, **fixados por commit** e com cabeçalho de licença (Apache 2.0)
  preservado. Geram **só TypeScript**: o Go já usa o `errdetails` do `genproto`, e gerar `google.rpc` em Go
  registraria o mesmo tipo duas vezes no binário (pânico de registro duplicado no protobuf). Ficam fora do `buf lint`
  e do `buf breaking` (arquivos de terceiros).
- **Alternativa**: dependência do BSR (`buf.build/googleapis/googleapis`) em `buf.yaml`. Evita copiar arquivos, mas
  traz uma dependência de rede e de um serviço externo na geração, contra o que o ADR-0001 definiu para os plugins.
- Só o gerado é usado no código: o adaptador do ledger lê `ErrorInfo.reason` e o compara com o enum `ErrorReason`
  gerado, sem string solta.

## Consequências

- O ledger ganha uma RPC de consulta (aditiva) e o BFF não guarda nenhum mapeamento de conta.
- O login depende do ledger estar no ar (aceitável: sem ele não há home de qualquer forma).
- Há uma credencial simétrica a rotacionar em produção; fora do escopo (projeto didático), mas o segredo vem de ambiente.
- `traceparent` externo vira só um link no trace do BFF: ganho de segurança, perda zero hoje (nenhum cliente envia).
- Arquivos de terceiros no `proto/` pedem atualização manual do commit fixado, com `buf generate` e testes.
