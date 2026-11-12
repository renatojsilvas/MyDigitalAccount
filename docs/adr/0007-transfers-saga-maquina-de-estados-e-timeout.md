# ADR-0007: Transfers: saga orquestrada, máquina de estados, retomada e timeout na liquidação

- **Status**: Aceito (Fase 3), com os ajustes do autor: transição com *fencing token*; "não enviado" só com prova
  sobre todas as tentativas e marcador `settlement_maybe_sent` irreversível; devolução do limite ao dia da reserva;
  conciliação da conta `settlement` explicada (seção 8).
- **Data**: 2026-11-12

## Contexto

O Pix envolve quatro serviços (`transfers`, `antifraude`, `ledger`, `spi-fake`) e nenhuma transação distribuída: cada
um tem o seu banco (ou nenhum). Precisamos de uma forma de:

- levar o Pix do aceite até um estado final, mesmo com falhas parciais, quedas do processo e respostas lentas;
- desfazer o que já foi feito quando uma etapa posterior recusa (**compensação**);
- nunca debitar duas vezes, nunca estornar sem prova de que o SPI não liquidou e nunca concluir sem confirmação
  (regras 4 e 10, seção 4.5 do plano).

## 1. Saga orquestrada, não coreografada

**Decisão: saga orquestrada.** O `transfers` é o orquestrador: guarda a máquina de estados no próprio Postgres e chama
cada serviço por gRPC.

| | **Orquestrada** (escolhida) | Coreografada |
|---|---|---|
| Onde mora o fluxo | Num lugar só (`transfers`) | Espalhado: cada serviço reage aos eventos dos outros |
| "Em que estado está o Pix?" | Uma linha no banco | Precisa juntar eventos de vários serviços |
| Compensação e prazos | Código explícito, testável em unidade | Cada serviço precisa saber compensar e quando |
| Acoplamento | O orquestrador conhece os participantes | Os participantes se conhecem pelos eventos |
| Pré-requisito | Nenhum | Broker (o Kafka só entra na Fase 4) |

É o modelo usado em pagamentos: motores como Temporal, Cadence e Step Functions, ou motores próprios sobre um banco
relacional, como este. A coreografia se paga quando muitos times reagem ao mesmo fato sem que nenhum seja o dono do
processo, o que não é o caso: o Pix tem dono. **Alternativa descartada**: um motor de workflow (Temporal). Ele resolve
retomada, timers e histórico, mas esconderia justamente o que a fase quer ensinar, e traria mais um serviço.

## 2. Máquina de estados

```
ACCEPTED ──fan-out (errgroup)──> VALIDATED ──débito──> DEBITED ──> SETTLING
   │  antifraude + reserva do limite          │                         │
   │  + conta de destino                      │ saldo insuficiente      │ resposta do SPI
   ▼                                          ▼                         ├── liquidado ──────────────> SETTLED ──crédito no destino──> COMPLETED
REJECTED  <───────────────────────────────────┘                         ├── recusa explícita ───────> REVERSING ──estorno──> REVERSED
(terminal, sem débito, limite devolvido)                                 ├── não enviado (com prova) ─> REVERSING
                                                                         └── ambíguo ────────────────> PENDING_CONFIRMATION
                                                                                                          │ consulta: liquidado → SETTLED
                                                                                                          │ consulta: recusado  → REVERSING
                                                                                                          └ não encontrado / sem resposta → continua
                                                                                                            (prazo estourado: marcado overdue)
```

| Estado | Significado | Terminal | O cliente vê |
|---|---|---|---|
| `ACCEPTED` | Aceito e persistido; nada foi movido | Não | Em processamento |
| `VALIDATED` | Antifraude aprovou, limite reservado, destino válido | Não | Em processamento |
| `DEBITED` | Débito feito no ledger (`debit_entry_id` gravado) | Não | Em processamento |
| `SETTLING` | Pedido de liquidação em tentativa, com `settlement_id` já persistido | Não | Em processamento |
| `PENDING_CONFIRMATION` | Sem resposta definitiva do SPI; débito mantido | Não | Em processamento |
| `SETTLED` | SPI confirmou a liquidação (ponto sem volta) | Não | Em processamento |
| `COMPLETED` | Crédito feito na conta de destino | Sim | Concluído |
| `REVERSING` | Decidido estornar; estorno em andamento | Não | Em processamento |
| `REVERSED` | Estorno feito no ledger, limite devolvido | Sim | Falhou (com estorno) |
| `REJECTED` | Recusado antes de mover dinheiro | Sim | Recusado |

Os motivos de recusa e de estorno são um enum fechado: `ANTIFRAUD_DENIED`, `LIMIT_EXCEEDED`, `DESTINATION_INVALID`,
`SAME_ACCOUNT`, `INSUFFICIENT_FUNDS`, `DEPENDENCY_UNAVAILABLE` (recusas); `SETTLEMENT_REJECTED`, `SETTLEMENT_NOT_SENT`
(estornos).

**Transições explícitas.** O domínio tem a tabela dos pares (de, para) permitidos; qualquer outro par é bug e
falha. A tabela é coberta por um teste table-driven com todos os pares.

**Persistência e histórico.** Cada transição é, numa só transação:

```sql
UPDATE transfers SET state = $novo, ...
 WHERE id = $id AND state = $esperado AND lease_token = $token;   -- 0 linhas = transição perdida
INSERT INTO transfer_state_history (transfer_id, from_state, to_state, reason, at) ...;
```

É um ***compare-and-set*** que confere também o **dono do lease** (`lease_token`, ver seção 4). Um runner cujo lease
expirou e foi tomado por outro recebe "0 linhas", desiste da transferência e não grava nada por cima do runner novo:
é o ***fencing token***. O histórico alimenta o "consultar Pix com histórico de estados".

## 3. Etapas: retentáveis, compensáveis e o pivô

| Etapa | Tipo | Chave ou ID de idempotência | Se falhar |
|---|---|---|---|
| Antifraude (`Evaluate`) | Leitura com efeito nos contadores; retentável | `transfer_id` (o antifraude só conta uma vez, ADR-0008) | Recusa (`ANTIFRAUD_DENIED` ou `DEPENDENCY_UNAVAILABLE`) |
| Conta de destino (`GetAccountByDocument`) | Leitura; retentável | não precisa | Recusa (`DESTINATION_INVALID` ou `DEPENDENCY_UNAVAILABLE`) |
| Reserva do limite | Local (Postgres do transfers); **compensável** | a própria transição | Recusa (`LIMIT_EXCEEDED`) |
| Débito (`Debit`) | **Compensável** (estorno) e **retentável até resposta definitiva** | `transfer:{id}:debit` | `INSUFFICIENT_FUNDS` = recusa; erro ambíguo = repetir com a mesma chave |
| Liquidação (`Settle`) | **Pivô** | `settlement_id`, gerado no aceite | Seção 5 |
| Crédito no destino (`Credit`) | **Retentável até concluir** (*forward recovery*) | `transfer:{id}:credit` | Repete com backoff; nunca compensa |
| Estorno (`Reverse`) | **Retentável até concluir** | `transfer:{id}:reversal` | Repete com backoff; nunca desiste |

A liquidação é a ***pivot transaction***: antes dela, uma falha leva à compensação; depois dela, o Pix só anda para
frente (crédito no destino). Como o crédito e o estorno não têm volta, eles nunca "falham": repetem com backoff, com
a mesma chave, até o ledger responder. Se isso demorar, o Pix aparece parado no dashboard (métrica de idade por
estado).

O **débito também tem timeout ambíguo**: se o ledger não responde, não se sabe se debitou. A chave
`transfer:{id}:debit` resolve: o transfers repete com a mesma chave até ter uma resposta definitiva (sucesso ou
`INSUFFICIENT_FUNDS`, que o ledger grava e repete igual, ADR-0004). Por isso o débito nunca leva a "recusa por
indisponibilidade": desistir sem saber se debitou deixaria um débito sem Pix.

A **compensação** de cada etapa: reserva do limite → devolução (seção 6); débito → estorno (ADR-0009). A recusa antes
do débito só devolve o limite, se ele foi reservado.

## 4. Execução assíncrona após o aceite, e retomada após uma queda

**Por que o `POST /pix` responde 202 e não espera a saga:**

- a saga pode durar minutos (backoff, `PENDING_CONFIRMATION`), e uma requisição HTTP não deve ficar aberta tanto tempo;
- se a saga rodasse no `context` da requisição, o cliente desconectar cancelaria a saga no meio, com o débito feito;
- o SLO "iniciar um Pix" (seção 7.3 do plano) mede o aceite, e ele não deve depender da latência do SPI;
- prepara o acompanhamento em tempo real da Fase 4 (SSE): o cliente recebe o ID e acompanha o status.

**Alternativas**: rodar a saga dentro da requisição (mais simples, mas com os problemas acima) ou publicar num broker
e consumir (é o que o outbox fará na Fase 4 para os **eventos**; o trabalho da saga continua no Postgres).

**Aceite.** Numa só transação: valida o pedido, reserva a chave de idempotência do cliente, grava a transferência em
`ACCEPTED` (com `settlement_id` já gerado, UUIDv7, e o contexto de trace) e a primeira linha do histórico. Depois do
commit, responde com o ID. A chave de idempotência do cliente tem escopo pela conta de origem; a tabela
`transfer_idempotency_keys` guarda só `(conta, chave, HMAC-SHA256 do pedido, transfer_id)`. O HMAC usa um segredo de
ambiente (`TRANSFERS_REQUEST_HASH_KEY`), porque um SHA-256 simples de um pedido com CPF e valor poderia ser revertido
por força bruta: a tabela de idempotência **não guarda dado pessoal**. Mesma chave e mesmo pedido = mesmo ID (202 de
novo); mesma chave com outro pedido = `ALREADY_EXISTS`.

**Runner.** Depois do commit, o aceite avisa um *pool* de goroutines por um canal (caminho rápido). A fonte da verdade
é o banco: o runner pega trabalho assim:

```sql
UPDATE transfers
   SET lease_owner = $instancia, lease_until = now() + $lease, lease_token = lease_token + 1
 WHERE id IN (SELECT id FROM transfers
               WHERE state NOT IN (terminais) AND next_run_at <= now()
                 AND (lease_until IS NULL OR lease_until < now())
               ORDER BY next_run_at
               LIMIT $n
               FOR UPDATE SKIP LOCKED)
RETURNING ...;
```

- `FOR UPDATE SKIP LOCKED` deixa várias instâncias pegarem trabalho sem disputar a mesma linha (padrão de ***fila
  sobre o Postgres***).
- O **lease** (`lease_until`, 30 s, renovado por *heartbeat* durante etapas longas) diz que a transferência tem dono.
  Se o processo cair, o lease expira e outra instância (ou a mesma, reiniciada) pega a transferência.
- O `lease_token` cresce a cada tomada; toda escrita confere o token (seção 2). Um runner antigo que acorde depois
  de perder o lease não consegue gravar.
- Uma varredura a cada 500 ms cobre o aviso perdido (queda entre o commit e o aviso, ou outra instância).
- Uma etapa que precisa esperar (backoff, próxima consulta de status) grava `next_run_at` e solta o lease.

**Retomada.** Como cada etapa externa é idempotente (chave por etapa, `settlement_id`, `transfer_id` no antifraude),
reexecutar a etapa em que o processo caiu é seguro: o ledger devolve o resultado gravado, o spi-fake devolve o
status da liquidação já existente. O runner sempre continua **do estado persistido**, nunca de memória. Uma chamada
externa feita por um runner que depois perdeu o lease é, no máximo, uma repetição idempotente.

**Trace.** O contexto de trace do `StartPix` é gravado na transferência. Os spans da saga nascem como filhos dele,
inclusive depois de uma retomada por outra instância (que acrescenta um *span link* ao trace do próprio runner). Assim,
um único trace mostra o Pix passando por BFF, transfers, antifraude, ledger e spi-fake (critério de pronto).

## 5. Timeout na liquidação (seção 4.5 do plano)

**Classificação do desfecho de cada tentativa:**

| Desfecho | O que se sabe | Como se reconhece |
|---|---|---|
| Liquidado | Liquidou | resposta `SETTLED` |
| Recusa explícita | Não liquidou | resposta `REJECTED` |
| **Não enviado** | Não liquidou | **o circuit breaker recusou antes da chamada** (nenhum byte saiu do processo) |
| Ambíguo | Nada | qualquer outro erro: `DEADLINE_EXCEEDED`, `UNAVAILABLE`, `INTERNAL`, conexão cortada... |

Só o circuito aberto conta como prova de "não enviado". Um `UNAVAILABLE` do gRPC também aparece quando a conexão cai
**depois** de o pedido sair, então ele é ambíguo.

**Marcador irreversível `settlement_maybe_sent`.** Antes de cada tentativa que de fato chama o SPI, o runner grava
`settlement_maybe_sent = true` (escrita antecipada, com o *fencing token*). Se o processo cair durante a chamada, o
marcador já está verdadeiro. Ele **nunca volta a falso**: o domínio não tem essa transição, e um trigger no banco
recusa `true → false`. A decisão depois das tentativas:

- alguma resposta definitiva → `SETTLED` ou `REVERSING (SETTLEMENT_REJECTED)`;
- todas as tentativas foram "não enviado" **e** `settlement_maybe_sent = false` → `REVERSING (SETTLEMENT_NOT_SENT)`;
- qualquer outro caso → `PENDING_CONFIRMATION`. Exemplo do teste obrigatório: tentativa 1 com timeout, tentativa 2
  com circuito aberto → pendente, nunca estorno.

**Parâmetros** (variáveis de ambiente; os testes usam milissegundos):

| Parâmetro | Valor | Variável |
|---|---|---|
| Prazo de cada tentativa de liquidação | 1 s | `TRANSFERS_SPI_SETTLE_TIMEOUT` |
| Tentativas, todas com o mesmo `settlement_id` | 3 | `TRANSFERS_SPI_SETTLE_ATTEMPTS` |
| Backoff entre tentativas | exponencial, *full jitter*, base 200 ms, teto 1 s | `TRANSFERS_SPI_SETTLE_BACKOFF_BASE`, `_MAX` |
| Primeira consulta de status | 2 s depois de entrar em `PENDING_CONFIRMATION` | `TRANSFERS_CONFIRM_INTERVAL_BASE` |
| Intervalo das consultas seguintes | dobra a cada consulta, com jitter, até 30 s | `TRANSFERS_CONFIRM_INTERVAL_MAX` |
| Prazo máximo de confirmação | 10 min | `TRANSFERS_CONFIRM_DEADLINE` |

Com o spi-fake respondendo em dezenas de ms, o caminho feliz fica muito abaixo do SLO de 2 s para concluir. O pior
caso das três tentativas (~4 s) afeta só a cauda, que o SLO de 99% comporta. O prazo de 10 min é curto o bastante
para aparecer durante uma simulação e longo o bastante para um SPI degradado voltar.

**Worker de confirmação.** Usa o mesmo mecanismo de lease (seção 4), em um loop próprio, sobre as transferências em
`PENDING_CONFIRMATION` com `next_run_at` vencido. Consulta `GetSettlement(settlement_id)`:

- `SETTLED` → `SETTLED` (segue para o crédito no destino);
- `REJECTED` → `REVERSING (SETTLEMENT_REJECTED)`;
- `NOT_FOUND` ou erro → continua pendente; "não encontrado" **não prova** que não liquidou (o pedido pode estar em
  trânsito);
- prazo máximo estourado → continua pendente, com `confirmation_overdue = true` e intervalo de consulta no teto. Até a
  Fase 5 (conciliação) e a Fase 6 (alerta), esses Pix ficam visíveis no dashboard.

**Nunca**: estornar por timeout, concluir sem confirmação ou reenviar com outro `settlement_id`.

## 6. Limite diário

O `transfers` é o dono do limite. No fan-out, a etapa do limite **reserva** o valor de forma atômica:

```sql
INSERT INTO daily_limit_usage (account_id, day, used_cents) VALUES ($conta, $dia, 0) ON CONFLICT DO NOTHING;
UPDATE daily_limit_usage SET used_cents = used_cents + $valor
 WHERE account_id = $conta AND day = $dia AND used_cents + $valor <= $limite;   -- 0 linhas = LIMIT_EXCEEDED
```

Na mesma transação, a transferência grava `limit_day = $dia` e `limit_reserved = true`. Uma verificação sem reserva
deixaria dois Pix simultâneos passarem juntos do limite (*check-then-act*).

- **Dia** em `America/Sao_Paulo`, pela hora do aceite (gravada na transferência, para a reexecução usar o mesmo dia).
- **Devolução** na recusa e no estorno, **sempre ao dia gravado em `limit_day`**, nunca ao dia corrente. Teste: Pix às
  23h59, estornado às 00h05 → o uso do dia anterior diminui e o do dia novo fica intacto.
- A devolução é idempotente: `limit_reserved` vai a `false` na mesma transação que devolve.
- Padrão de R$ 2.000,00 por conta por dia (`TRANSFERS_DAILY_LIMIT_CENTS`). Limite noturno e limite por conta ficam
  fora do escopo.

## 7. Resiliência: timeouts, retry e circuit breaker

- **Timeout por etapa**: antifraude 300 ms, ledger 500 ms, SPI 1 s por tentativa; o fan-out inteiro tem 1 s. Os prazos
  são propagados por `context` até o servidor.
- **Retry com backoff exponencial e *full jitter*** (espera aleatória entre 0 e o teto da tentativa), escrito à mão
  sobre a biblioteca padrão. O jitter evita que todos os clientes repitam no mesmo instante (*thundering herd*).
  Repete só erros transitórios (`UNAVAILABLE`, `DEADLINE_EXCEEDED`, `RESOURCE_EXHAUSTED`) e só chamadas idempotentes;
  recusa de negócio nunca é repetida. Etapas da pré-validação: até 3 tentativas, base 50 ms, teto 300 ms.
- **Circuit breaker por dependência** (`antifraude`, `ledger`, `spi`), com `sony/gobreaker/v2`: abre com 5 falhas
  seguidas, ou com 50% de falhas em pelo menos 20 chamadas numa janela de 10 s; meio-aberto depois de 5 s, com uma
  chamada de prova. Só falha de infraestrutura conta como falha: `NOT_FOUND` e `FAILED_PRECONDITION` são sucessos para
  o circuito.

**Bibliotecas**: `sony/gobreaker/v2` (v2.4.0, de 2026-01-01; genérica e pequena) foi preferida ao `failsafe-go`
(v0.9.8, de 2026-09-30), que junta retry, circuito e timeout mas ainda é pré-1.0 e esconderia o retry, que aqui é
didático.

## 8. Crédito no destino e a conciliação da conta `settlement`

Na demonstração, origem e destino são contas do próprio ledger, e a chave Pix é só do tipo CPF (resolvida por
`GetAccountByDocument`). Mesmo assim, o Pix passa pelo spi-fake, de propósito, para exercitar o fluxo. Num banco real,
um Pix entre clientes da mesma instituição (*on-us*) não passaria pelo SPI. Lançamentos de um Pix, todos com
`external_reference = transfer_id` (ADR-0009):

| Momento | Lançamento | Conta `settlement` |
|---|---|---|
| Débito | D cliente de origem / C `settlement` | +valor (dinheiro a caminho do SPI) |
| Crédito no destino (após `SETTLED`) | D `settlement` / C cliente de destino | −valor |
| Estorno (em vez do crédito) | D `settlement` / C cliente de origem | −valor |

Num Pix terminado (`COMPLETED` ou `REVERSED`), a `settlement` fecha em zero para aquela referência. Em andamento
(`DEBITED` até `SETTLED`), fica com o valor em trânsito.

**Como a Fase 5 evita divergência falsa:**

- A **conciliação externa** não compara o saldo da `settlement` com o relatório do spi-fake. Ela casa **linha a
  linha**: cada liquidação do relatório (`settlement_id`, `transfer_id`, valor, status) com a transferência de mesmo
  `settlement_id`. Liquidado lá exige `SETTLED`/`COMPLETED` aqui, com o mesmo valor; recusado lá exige `REVERSED` aqui.
- A **conciliação interna** agrupa os lançamentos do ledger por `external_reference` e confere o esperado para o
  estado da transferência: `COMPLETED` = débito de origem + crédito de destino, de mesmo valor; `REVERSED` = débito +
  estorno que aponta para ele; `REJECTED` = nenhum lançamento. A soma da `settlement` por referência deve ser zero
  nos estados terminais.
- Ela só olha **janelas fechadas** com atraso de tolerância, então Pix em andamento (valor em trânsito na
  `settlement`) não entram na conta.
- A `settlement` só recebe movimentos do SPI: o saldo inicial do seed fica em `opening_balance` (ADR-0004), e não
  polui a conciliação.

## Consequências

- O `transfers` ganha banco próprio (`transfers`, papéis `transfers_owner` e `transfers_app`, no padrão do ledger) com
  `transfers`, `transfer_state_history`, `transfer_idempotency_keys` e `daily_limit_usage`.
- O registro da transferência guarda a chave Pix de destino (é o dado de negócio, como num comprovante); logs, traces
  e métricas só a veem mascarada. A tabela de idempotência não guarda dado pessoal.
- **Implementação (passo 6)**: o aceite reserva a chave de idempotência **antes** de gravar a transferência (FK
  `DEFERRABLE INITIALLY DEFERRED`, conferida no `COMMIT`), como no ledger: um pedido concorrente com a mesma chave
  espera no índice único em vez de gravar uma transferência órfã. O binário embute a base de fusos
  (`time/tzdata`): a imagem final é distroless, e sem ela o dia do limite em `America/Sao_Paulo` falharia só em
  produção. O papel `transfers_app` só pode atualizar as colunas da saga; valor, origem, destino informado,
  `settlement_id` e o dia do limite são imutáveis também por privilégio.
- **Implementação (passo 7)**:
  - Motivo de recusa novo, `SOURCE_ACCOUNT_INVALID`: o débito pode voltar `ACCOUNT_NOT_FOUND` (conta de origem
    inexistente), e nenhum motivo descrevia isso. Aditivo no contrato; a migration `00002` amplia o `CHECK`.
  - "Não enviado" com prova usa o modo de dois passos do `gobreaker/v2`: o circuito autoriza, o runner grava
    `settlement_maybe_sent` e só então a chamada sai. Circuito recusado, ou marcador que não pôde ser gravado, não
    enviam nada. Por construção, **marcador falso prova que nenhuma tentativa saiu**, e é o que decide o estorno.
  - `ENTRY_ALREADY_REVERSED` no estorno conta como sucesso: com a mesma chave a resposta seria a gravada, então
    essa recusa só aparece se o débito já foi estornado por outro caminho, e o estado final é o correto.
  - O lease (`TRANSFERS_LEASE_TTL`) precisa passar do dobro do pior caso da liquidação (tentativas × (prazo +
    backoff máximo)); a configuração recusa o boot se não passar.
  - O texto do extrato é cortado em 140 caracteres no transfers: passar do limite do ledger seria uma recusa por
    formato, que a etapa repetiria para sempre.
  - A validação com dependência fora do ar (inclusive durante a subida dos serviços) recusa o Pix com
    `DEPENDENCY_UNAVAILABLE`, sem mover dinheiro; o span `pix.saga.accepted` mostra a causa.
- **Implementação (passo 8)**:
  - O worker de confirmação usa o mesmo mecanismo de fila e lease, num loop e num pool próprios
    (`TRANSFERS_CONFIRM_WORKERS`): pendentes demorados nunca atrasam Pix novos. O prazo máximo conta de
    `pending_since`, gravado na entrada em `PENDING_CONFIRMATION` (migration `00003`).
  - O intervalo entre consultas usa *equal jitter* (metade fixa, metade sorteada), não *full jitter*: a espera nunca
    sorteia perto de zero, o que martelaria um SPI degradado.
  - Pendentes, além do prazo e idade do mais antigo são lidos do banco a cada coleta de métricas: valem para todas
    as instâncias, não só para a que coleta.
  - **Partida a frio**: o primeiro Pix depois de um reinício foi recusado com `DEPENDENCY_UNAVAILABLE`, porque a
    conexão com o antifraude ainda não estava pronta. O boot agora aquece as conexões (`platform/grpcclient.WarmUp`,
    nunca derruba o boot), como o BFF faz desde a Fase 2.
  - Teste com o Toxiproxy entre transfers e SPI: numa conexão nova, um prazo estourado antes de o servidor responder
    o *preface* do HTTP/2 impede o pedido de sair. Do lado do cliente, "não saiu" e "saiu e a resposta não voltou"
    são indistinguíveis, o que confirma a regra: só o circuito aberto (ou o marcador falso) prova "não enviado".
- Pix em `PENDING_CONFIRMATION` além do prazo **não se resolvem sozinhos** até a Fase 5. É intencional (regra 10).
- Um ledger fora do ar por muito tempo deixa Pix parados em `VALIDATED`, `REVERSING` ou `SETTLED`. A métrica de idade
  por estado mostra isso, e o alerta vem na Fase 6.
- O lease de 30 s é o tempo máximo que um Pix fica parado depois de uma queda (mais o intervalo da varredura).
- **Métricas** (rótulos só de conjunto fixo): transições da saga por `from`/`to`, desfechos por estado terminal e
  motivo, compensações por tipo, duração de cada etapa, tentativas de retry e estado do circuito por dependência,
  timeouts de liquidação, Pix pendentes de confirmação e idade do mais antigo, resultado das consultas de status,
  leases perdidos.
