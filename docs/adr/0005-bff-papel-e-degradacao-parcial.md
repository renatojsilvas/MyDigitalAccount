# ADR-0005: BFF: papel, limites e degradação parcial da home

- **Status**: Aceito (Fase 2)
- **Data**: 2026-11-12

## Contexto

O BFF (*Backend for Frontend*) é a única porta de entrada do front. Ele existe para que a tela faça **uma**
chamada e receba uma resposta já moldada para ela, em vez de o front falar com vários serviços. Isso cria uma
tentação conhecida: como o BFF "já tem os dados na mão", acumular ali cálculos e regras. Em pouco tempo o BFF vira
um segundo núcleo de domínio, sem dono claro, que duplica o que os serviços Go já fazem (regra 1 do projeto).

A home chama o ledger duas vezes (`GetBalance` e `ListStatement`). Quando uma das chamadas falha ou demora, é
preciso decidir antes, e não no meio de um incidente, o que a tela mostra.

## Decisão

### 1. Papel do BFF: agregar, adaptar e proteger

| Faz | Exemplo na Fase 2 |
|---|---|
| **Agrega** | Duas chamadas ao ledger em paralelo, numa resposta só |
| **Adapta** | `bigint` do protobuf vira inteiro em centavos no JSON; `DIRECTION_CREDIT` vira `"credit"`; `Timestamp` vira ISO 8601 |
| **Protege** | Autentica (JWT), valida entrada e saída (Zod), traduz erros internos sem vazá-los, impõe timeout por chamada |

**O que não pode entrar no BFF** (se aparecer em revisão, vai para o serviço Go dono do dado):

- Calcular, somar, arredondar ou "corrigir" saldo, limite, tarifa ou qualquer valor financeiro. Se a tela precisa de
  um número derivado (ex.: "disponível"), o serviço dono o expõe no `.proto`.
- Ponto flutuante com dinheiro. O valor trafega como inteiro em centavos; a formatação em reais é do front
  (apresentação, via `Intl`), e não muda o valor.
- Decidir se uma operação é permitida (saldo suficiente, limite, antifraude).
- Banco de dados próprio com dado de negócio. O BFF é sem estado de domínio (Redis entra só nas Fases 4, 6 e 7 e nunca
  guarda saldo).
- Cachear saldo (regra do projeto, vale para a Fase 7).
- Acessar o banco de outro serviço, ou conhecer tipos gRPC fora de `clients/`.

### 2. Como os tipos fluem, sem escrever nada duas vezes

```
proto/conta/ledger/v1/ledger.proto          (1) fonte única do contrato com o ledger
        │  buf generate (protoc-gen-es)
        ▼
gen/ts → @conta/contracts                   tipos gerados: GetBalanceResponse { balanceCents: bigint } ...
        │  só clients/ledger.ts importa isto
        ▼
clients/ledger.ts (adaptador)               traduz mensagem gRPC → tipo de domínio do BFF
                                            (Result<…, DependencyError>; bigint ainda como bigint)
        ▼
modules/home/service.ts                     orquestra: allSettled, timeout, decide a degradação
        ▼
modules/home/mapper.ts                      domínio → view model da tela
        │  o tipo de saída é z.infer<typeof homeResponseSchema>
        ▼
modules/home/schema.ts (Zod)                (2) fonte única do contrato com o front:
                                            valida a entrada, serializa/valida a saída, e gera o OpenAPI
        │  @fastify/swagger + jsonSchemaTransform
        ▼
openapi.json (gerado, fora do Git)
        │  openapi-typescript
        ▼
web/src/api/schema.d.ts (gerado)            tipos e cliente (openapi-fetch) do front
```

Há exatamente duas fontes escritas à mão: o `.proto` e o schema Zod. Todo o resto é gerado ou inferido. A única
"duplicação" deliberada é o tipo de domínio em `clients/ledger.ts` (poucos campos, sem `Message` do protobuf): é a
*anti-corruption layer*, o que impede o resto do BFF de depender do formato do gRPC. O compilador protege contra
*drift*: a função que converte a mensagem gerada para o tipo de domínio quebra a compilação se um campo mudar.

`bigint` merece atenção: o JSON não tem `bigint`. O mapper converte com `Number.isSafeInteger` como guarda (limite
de 2^53 centavos, ~R$ 90 trilhões) e, se estourar, falha em vez de arredondar. Nunca passa por `float` aritmético.

### 3. Política de degradação parcial

| Dado | Classe | Se faltar | Por quê |
|---|---|---|---|
| **Saldo** (`GetBalance`) | **Essencial** | A home falha: 504 (timeout), 503 (indisponível), 401 (conta do token não existe), com `application/problem+json` | A tela sem saldo não tem utilidade, e um saldo antigo ou inventado é pior que erro: o usuário decidiria com um número errado. Não se mostra saldo em cache (regra do projeto) |
| **Extrato** (`ListStatement`) | **Opcional** | A home responde **200**, com o saldo e `statement: { status: "unavailable", reason }` | O usuário ainda enxerga o essencial; o extrato volta na próxima abertura |

Regras:

1. As duas chamadas saem **em paralelo** (`Promise.allSettled`), cada uma com o próprio `AbortSignal`/timeout. O prazo
   também vai ao ledger no header `grpc-timeout` (deadline propagado).
2. **A degradação é explícita no contrato**, não um campo vazio: o extrato é uma união discriminada por `status`
   (`"ok"` com itens, ou `"unavailable"` com `reason` em `timeout | unavailable | error`). O front não precisa
   adivinhar se "lista vazia" significa "sem movimentos" ou "falhou".
3. **Orçamento de tempo**: o SLO da home é 300 ms (plano, seção 7.3). Como as chamadas são paralelas, o tempo da home
   é o do mais lento. Os timeouts padrão são, portanto, menores que 300 ms: `LEDGER_BALANCE_TIMEOUT_MS=250` e
   `LEDGER_STATEMENT_TIMEOUT_MS=200` (configuráveis, validados no boot). O extrato é cortado mais cedo de propósito:
   é o que pode faltar.
4. Erro "esperado" de dependência não é `throw`: é `Result` com união discriminada (`DependencyError`). Só o inesperado
   chega ao handler central, que responde 500 genérico e manda a causa ao log e ao trace.
5. **`ACCOUNT_NOT_FOUND` no saldo vira 401, não 404.** A conta vem do token; se ela não existe, o token aponta para
   uma conta que sumiu (comum ao recriar o banco) e o cliente precisa autenticar de novo: o front volta ao login.
   No extrato, o mesmo erro conta como degradação, mas o 401 do saldo prevalece quando os dois chegam.

### 4. Observabilidade da degradação

- Contador `bff.home.degraded` com rótulos `section` (`statement`) e `reason` (`timeout | unavailable | error`),
  conjuntos pequenos e fixos. Nenhum ID em rótulo.
- No span do servidor: atributo `home.degraded = true` e `home.degraded.sections`; o span do cliente da chamada que
  falhou já traz o status gRPC e fica vermelho no trace. O log da requisição traz `degraded`.
- Gatilho automático: **nenhum** nesta fase. A degradação é o comportamento de proteção em si. O alerta por esse
  contador, se fizer sentido, vem na Fase 6.

## Alternativas consideradas

- **Tudo ou nada** (qualquer falha derruba a home): simples, mas uma lentidão só no extrato (a chamada mais pesada)
  deixaria o usuário sem ver o saldo. Descartada.
- **Tudo opcional** (saldo também degrada, mostrando "—"): a tela abre, mas o 200 mascara uma falha da parte que
  mais importa e esconde o problema do SLO. Descartada.
- **Cachear o último saldo para degradar**: proibido pela regra do projeto (e pela seção 4.3 do plano).
- **GraphQL/BFF genérico com resolvers**: mais flexível, fora do plano, e não ensina o padrão que queremos.

## Consequências

- O BFF fica fino e substituível; regra nova de negócio exige mudar um serviço Go e o `.proto`, não o BFF.
- O contrato da home carrega a degradação; todo campo opcional futuro (limites, na Fase 3) segue o mesmo molde: seção
  com `status`.
- Dois timeouts para configurar e calibrar; a calibração real vem da medição (Fase 7).
- Custo aceito: se o saldo falha, o usuário vê um erro, não um valor "aproximado".
