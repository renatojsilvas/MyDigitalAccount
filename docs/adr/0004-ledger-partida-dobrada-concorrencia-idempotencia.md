# ADR-0004: Ledger: partida dobrada, concorrência no saldo e idempotência

- **Status**: Aceito (Fase 1). Partida dobrada com contas internas por finalidade; concorrência: opção A;
  idempotência: opção 2; dívidas da Fase 0: as três propostas.
- **Data**: 2026-11-11

## Contexto

O ledger é a fonte da verdade do dinheiro (regras 2, 4 e 9). Ele precisa:

- registrar todo movimento de forma auditável e sem criar nem destruir dinheiro;
- garantir que o saldo de uma conta de cliente nunca fique negativo, inclusive com débitos concorrentes na mesma conta
  (critério de pronto da fase);
- aceitar a mesma escrita repetida sem efeito duplicado, porque toda chamada entre serviços é *at-least-once* (retries
  do `transfers` na Fase 3, reentrega do Kafka na Fase 4, reprocessamento do `reconciler` na Fase 5);
- aguentar o cenário de **conta quente** da Fase 7 (milhares de Pix saindo da mesma conta ao mesmo tempo).

## 1. Modelo de partida dobrada

**Decisão: aceita, com contas internas por finalidade** (ajuste do usuário à proposta inicial, que tinha uma
`settlement` única; ver "Revisão" ao fim desta seção). Toda operação gera um **lançamento** (`journal_entry`) com duas ou mais **pernas** (`postings`). Cada perna
movimenta uma conta, em débito (D) ou crédito (C), com valor `int64` positivo em centavos. Invariante: em todo lançamento,
soma dos débitos = soma dos créditos.

Contas (`accounts`) de dois tipos:

| Tipo | Exemplo | Saldo pode ser negativo? | Saldo materializado? |
|---|---|---|---|
| `customer` | conta de cada cliente | Não | Sim, na linha da conta |
| `internal` | `opening_balance` (saldo inicial), `settlement` (dinheiro que entra e sai pelo SPI) | Sim | Não: derivado das pernas |

Do ponto de vista do banco, a conta do cliente é um **passivo** (o banco deve esse dinheiro ao cliente), então ela é de
natureza credora: crédito aumenta, débito diminui. As operações da API são casos de uso sobre esse modelo. Quem chama
informa o **motivo** (enum no `.proto`) e o ledger mapeia motivo para conta interna:

| Operação e motivo | Perna D | Perna C |
|---|---|---|
| `Credit`, `CREDIT_REASON_OPENING` (saldo inicial, usado pelo seed) | `opening_balance` | cliente |
| `Credit`, `CREDIT_REASON_EXTERNAL` (dinheiro que chega de fora) | `settlement` | cliente |
| `Debit`, `DEBIT_REASON_EXTERNAL` (dinheiro que sai, o Pix da Fase 3) | cliente | `settlement` |

**Revisão**: com uma `settlement` única, o saldo inicial do seed se misturaria ao dinheiro que de fato passou pelo SPI,
e a conciliação externa da Fase 5 (que compara `settlement` com o relatório do `spi-fake`) acusaria divergência falsa.
Uma conta interna por finalidade mantém cada contrapartida conciliável contra a sua própria fonte. Os enums de motivo
entram já no contrato inicial, inclusive no `Debit` (que hoje só tem `EXTERNAL`): acrescentar um campo de motivo
depois deixaria clientes antigos enviando "não informado", que o ledger teria de adivinhar. Acrescentar **valores** a
um enum existente, por outro lado, não quebra o contrato (`buf breaking`, regra `FILE`).

O cliente da API nunca escolhe a contrapartida: ela é regra contábil e fica no ledger. Motivo não informado
(`*_UNSPECIFIED`) é argumento inválido. Estorno (Fase 3) será um novo
lançamento com as pernas invertidas e referência ao original; **lançamentos nunca são alterados nem apagados**
(append-only). O extrato é a lista das pernas da conta, em ordem, com paginação por cursor (*keyset*).

Defesa em profundidade da invariante: o domínio só constrói lançamentos balanceados, e o banco confere de novo com uma
*constraint trigger* `DEFERRABLE INITIALLY DEFERRED` (soma por lançamento no commit) e `CHECK (balance >= 0)` nas contas
de cliente.

**Alternativa**: partida simples (uma tabela `transactions` com valor com sinal por conta). Mais curta, mas não mostra de
onde o dinheiro veio nem para onde foi, não permite provar que o total do sistema fecha e não é o que se usa em core
bancário.

## 2. Controle de concorrência no saldo

Problema: dois débitos de R$ 80 numa conta com R$ 100. Ambos leem 100, ambos acham que cabe, ambos gravam: saldo -60.

### Opção A: lock pessimista com saldo materializado

`SELECT ... FOR UPDATE` na linha da conta, o domínio decide (`account.Debit` devolve `ErrInsufficientFunds`), grava o
saldo novo e as pernas na **mesma transação**, em `READ COMMITTED`.

- Prós: correto sem retry; o segundo débito espera o primeiro e enxerga o saldo já atualizado; leitura de saldo O(1);
  é o padrão de core bancário; fácil de raciocinar e testar.
- Contras: serializa por conta (latência cresce com a contenção na conta quente); exige ordem fixa de lock quando um
  lançamento tocar mais de uma conta de cliente (evita deadlock); exige `lock_timeout` para não esperar para sempre.

### Opção B: lock otimista com versão

Lê saldo e `version`, decide, e grava com `UPDATE ... WHERE id = $1 AND version = $2`; zero linhas afetadas = conflito,
repete.

- Prós: não segura lock durante a leitura; ótimo quando conflitos são raros.
- Contras: na conta quente vira **tempestade de retries** (trabalho desperdiçado, cauda de latência, possível
  inanição); precisa de política de retry com backoff e limite, e de um erro para "desisti". O conflito é exatamente o
  caso que o critério de pronto testa.

### Opção C: saldo derivado das pernas (sem saldo gravado)

Saldo = `SUM` das pernas. Só uma fonte da verdade, nada para dessincronizar.

- Prós: append-only puro; impossível o saldo divergir do extrato.
- Contras: o `SUM` cresce com o histórico (exige snapshots periódicos para escalar); **não resolve a concorrência
  sozinho**: ler a soma e inserir é *write skew*, então ainda exige `SERIALIZABLE` (com retry em falha de serialização)
  ou um lock por conta (advisory lock), voltando à opção A com mais custo.

### Decisão: opção A

Saldo materializado **só nas contas de cliente**, travado com `FOR UPDATE`, atualizado na mesma transação das pernas
(por isso não há como divergir). As contas internas não têm saldo gravado: se cada débito também atualizasse a linha
de `settlement` ou de `opening_balance`, **toda** operação do sistema disputaria o mesmo lock, e o gargalo seria global, não por conta. O saldo
delas é derivado das pernas, consultado raramente.

Complementos: `CHECK (balance >= 0)` como rede de segurança (se o domínio errar, a transação falha em vez de gravar
saldo negativo); `SET LOCAL lock_timeout` curto, com o erro traduzido para status gRPC de indisponibilidade
temporária; teste de integração que confere `balance == soma das pernas` depois da carga concorrente.

## 3. Idempotência das escritas

Toda escrita (`OpenAccount`, `Credit`, `Debit`) recebe uma `idempotency_key` do cliente.

### Opção 1: chave única no próprio lançamento

`UNIQUE (idempotency_key)` em `journal_entries`; conflito na inserção = repetição, devolve o lançamento existente.

- Prós: nenhuma tabela extra; atômico com o efeito.
- Contras: só registra o que deu certo. Um débito recusado por saldo não deixa rastro, então a mesma chave repetida
  depois de um crédito pode ter **outra resposta**. Não cobre `OpenAccount` sem gambiarra. Detectar "mesma chave,
  pedido diferente" exige comparar campo a campo.

### Opção 2: tabela de idempotência no Postgres, na mesma transação do efeito

`idempotency_keys (scope, key, request_hash, response, created_at)`, `PRIMARY KEY (scope, key)`, com `scope` = nome da
operação. Fluxo, numa transação só:

1. `INSERT` da chave **antes** de qualquer outro trabalho. Se outra requisição com a mesma chave estiver em andamento,
   o índice único faz esta esperar o commit da outra; aí ela vê a chave e não repete o trabalho.
2. Chave já existe: se o `request_hash` bate, devolve a resposta gravada (*replay*); se não bate, erro de conflito de
   idempotência.
3. Chave nova: executa o caso de uso e grava a resposta, **inclusive a recusa de domínio** (saldo insuficiente), e faz
   commit de tudo junto.

- Prós: a mesma chave sempre devolve a mesma resposta (semântica do Stripe e de APIs de pagamento); cobre todas as
  escritas com um mecanismo só; conflito de payload detectado por hash; métricas de replay e conflito saem naturais.
- Contras: uma tabela a mais e uma política de retenção (sem expurgo nesta fase: o volume é pequeno; anotado como
  pendência); a resposta gravada precisa ser serializada (bytes do Protobuf da resposta).

### Opção 3: idempotência no Redis

Descartada: a regra 9 proíbe. Se o Redis perdesse a chave depois do efeito no Postgres, um retry duplicaria o débito.

### Decisão: opção 2

Semântica, em uma frase: **a mesma chave, com o mesmo pedido, devolve sempre a mesma resposta, seja ela sucesso ou
recusa de negócio.**

| Resultado da primeira execução | É gravado? | O que a repetição com a mesma chave recebe |
|---|---|---|
| Sucesso | Sim, com a resposta | A mesma resposta (mesmo `entry_id`, mesmo saldo da época), sem novo lançamento |
| Recusa de negócio (saldo insuficiente, conta inexistente, CPF já cadastrado) | **Sim**, com o status gRPC e o `ErrorInfo` | **A mesma recusa**, mesmo que o saldo já tenha mudado. Para tentar de novo, o cliente usa outra chave |
| Mesma chave, pedido diferente (hash não bate) | Não altera o registro | Erro de conflito de idempotência |
| Pedido inválido (validação de formato, antes do caso de uso) | Não | O mesmo erro de validação, recalculado |
| Erro de infraestrutura (timeout, lock timeout, banco fora) | **Não**: rollback junto com a chave | Execução nova, como deve ser |

Por que gravar a recusa: quem chama decide com base na resposta. Se o `transfers` recebe "saldo insuficiente", marca o
Pix como falho; uma reentrega com a mesma chave que, minutos depois, debitasse com sucesso deixaria o dinheiro saindo
de um Pix já dado como falho. Gravar a recusa torna a resposta uma função da chave, não do momento.

**Formato da resposta gravada**: o resultado é gravado como uma de duas formas, mutuamente exclusivas: os bytes
Protobuf da mensagem de resposta (sucesso) ou o **status gRPC (código e mensagem) com o `google.rpc.ErrorInfo`**
(recusa). Na repetição, a borda gRPC devolve exatamente o mesmo status e os mesmos detalhes, então a recusa repetida é
idêntica à original, byte a byte nos detalhes. O domínio não conhece gRPC: o caso de uso grava o erro de domínio
tipado, e a tradução para status e `ErrorInfo` é a mesma função da borda, usada nos dois caminhos.

## 4. Uma conta por CPF

**Decisão (do usuário)**: cada CPF tem no máximo uma conta. `UNIQUE (holder_document)` em `accounts`; um segundo
`OpenAccount` com o mesmo CPF e **outra** chave falha com `ALREADY_EXISTS` e `ERROR_REASON_ACCOUNT_ALREADY_EXISTS`.
Com a **mesma** chave e o mesmo pedido, é repetição e devolve a conta criada na primeira vez.

Detalhe de implementação que importa: no Postgres, uma violação de `UNIQUE` aborta a transação inteira, e aí não dá
mais para gravar a recusa na tabela de idempotência. Por isso a inserção usa `INSERT ... ON CONFLICT (holder_document)
DO NOTHING RETURNING ...`: nenhuma linha devolvida significa "CPF já cadastrado", a transação continua saudável e a
recusa é gravada junto com a chave. Duas aberturas concorrentes com o mesmo CPF são resolvidas pelo próprio índice: a
segunda espera o commit da primeira e então cai no `DO NOTHING`.

## 5. Contrato de erros

Erros de domínio voltam como **status gRPC canônico + `google.rpc.ErrorInfo`** nos detalhes (modelo de erros do
Google, AIP-193): o status diz *a classe* do erro (o cliente decide se repete), o `ErrorInfo.reason` diz *qual* erro de
negócio foi. Os motivos são um `enum ErrorReason` no próprio `.proto`, para que Go e TS usem nomes gerados.

| Erro de domínio | Status gRPC | `ErrorReason` |
|---|---|---|
| Conta inexistente | `NOT_FOUND` | `ACCOUNT_NOT_FOUND` |
| Saldo insuficiente | `FAILED_PRECONDITION` | `INSUFFICIENT_FUNDS` |
| CPF já tem conta (`OpenAccount`) | `ALREADY_EXISTS` | `ACCOUNT_ALREADY_EXISTS` |
| Chave reutilizada com outro pedido | `ALREADY_EXISTS` | `IDEMPOTENCY_KEY_REUSED` |
| Formato inválido | `INVALID_ARGUMENT` | `INVALID_REQUEST` |
| Lock da conta além do `lock_timeout` | `UNAVAILABLE` (seguro repetir com a mesma chave) | nenhum: não é erro de domínio |

Alternativa descartada: devolver o resultado de negócio dentro da resposta (`oneof` sucesso/recusa). Funciona, mas
mistura duas convenções no mesmo serviço e faz cada cliente reimplementar o que o status gRPC já padroniza.

## 6. Dívidas da Fase 0 que entram nesta fase

**Decisão: aprovadas as três.**

| Item | Decisão |
|---|---|
| Papel Postgres próprio | Dois papéis por serviço: `ledger_owner` (dono do schema, roda as migrations) e `ledger_app` (só DML nas tabelas, sem DDL). Senhas por variável de ambiente, validadas no boot. O usuário `conta` vira só o superusuário de administração. |
| Spans do `pgx` e métricas do pool | `pgx` com `QueryTracer` do OpenTelemetry e métricas do pool (`pgxpool.Stat`) como métricas assíncronas. A biblioteca será escolhida conferindo a manutenção atual na documentação oficial, no passo correspondente. |
| Rótulo de status gRPC | Unificar no nome da convenção mais nova, que o `otelgrpc` do ledger já usa (`rpc_response_status_code`, por nome do código), ajustando a instrumentação manual do BFF, que é a que temos controle. Confirmar na especificação de semconv antes de mudar. |

**Como os papéis foram implementados (passo 3a)**

- O script de init (`deploy/postgres/init/01-ledger.sh`) cria `ledger_owner` e `ledger_app`, cria o banco `ledger` com
  dono `ledger_owner` e tira o `CONNECT` de `PUBLIC`. Senhas em `LEDGER_DB_OWNER_PASSWORD` e `LEDGER_DB_APP_PASSWORD`.
  O serviço lê as suas em `LEDGER_DB_USER` e `LEDGER_DB_PASSWORD`; `POSTGRES_USER` é só o superusuário.
  **Atualização (Fase 3)**: o script de init deu lugar ao job idempotente `postgres-bootstrap`
  (`deploy/postgres/bootstrap.sh`), que provisiona banco e papéis de todos os serviços a cada `up`, inclusive num
  volume já existente; trocar uma senha no `.env` passa a valer no `up` seguinte, sem recriar o volume.
- Privilégios concedidos **tabela a tabela na própria migration**: `SELECT` e `INSERT` em `journal_entries` e
  `postings` (append-only também por privilégio), `UPDATE` só da coluna `balance_cents` em `accounts`, e só de
  `status_code` e `body` em `idempotency_keys`. Nada de `DELETE`, `TRUNCATE` ou DDL. Um teste de integração tenta cada
  operação proibida como `ledger_app` e espera `42501`.
- **Migration como job, não no boot do serviço**: as migrations ficam embutidas no binário (`embed.FS`) e rodam com
  `ledger migrate`, num contêiner `ledger-migrate` que executa uma vez com `ledger_owner` e sai; o ledger só sobe depois
  (`service_completed_successfully`). Rodar no boot exigiria dar DDL ao papel do serviço. Goose como biblioteca (mesma
  versão da CLI da imagem `tools`), com *advisory lock* para réplicas concorrentes do job. Migrations só para frente.
- Ordem do extrato: `postings.id` é uma identidade gerada pelo banco. Como a conta fica travada durante o lançamento,
  as pernas de uma conta são inseridas em fila e o `id` segue a ordem em que o saldo mudou; ele é o cursor.

**Persistência (passo 3b)**

- **sqlc** com `pgx/v5`: queries em `services/ledger/internal/postgres/queries/`, conferidas contra o schema das
  migrations. O código gerado (`internal/postgres/sqlcgen/`) fica fora do Git e é recriado por `make generate`,
  pela mesma razão do `gen/` do `.proto` (ADR-0001). Alternativa descartada: ORM, que esconderia o `FOR UPDATE`, o
  `ON CONFLICT` e a ordem dos comandos, justamente o que precisa ser explícito num ledger.
- `Store.WithTx(ctx, fn)` abre a transação, faz commit se `fn` devolver `nil` e rollback em erro ou panic. As
  operações que exigem transação (travar conta, lançar, idempotência) só existem no tipo `Tx`.
- `lock_timeout` como parâmetro de sessão do pool (`LEDGER_DB_LOCK_TIMEOUT`, padrão 1s, mínimo 1ms). Lock timeout,
  deadlock e falha de serialização voltam como erro com `Retryable() bool`; quem trata pergunta pelo comportamento,
  sem importar o adaptador.

**Observabilidade do banco (passo 6)**

- Biblioteca `github.com/exaring/otelpgx` v0.13.0 (publicada em 8/10/2026, mantida, pgx v5), escolhida depois de
  conferir a manutenção na documentação oficial. Providers passados explicitamente (o padrão dela são os globais).
- Um span por query, filho do span gRPC, com `db.query.text` (SQL com placeholders) e **sem os valores dos
  parâmetros** (o padrão da biblioteca; um teste garante que o CPF não aparece). Sem span de aquisição de conexão:
  a espera pelo pool aparece em `pgxpool.empty_acquire_wait_time`.
- A sonda `/readyz` usa o ping do protocolo (`PgConn().Ping`), que não passa pelo tracer: sem traces órfãos.
- Métricas do pool (`pgxpool.*`) para o método USE, com tamanho explícito (`LEDGER_DB_MAX_CONNS`, padrão 10).
- Métricas de negócio por uma porta `account.Observer` (o domínio não importa OpenTelemetry), implementada em
  `internal/accountotel`: `ledger.entries`, `ledger.posted_amount`, `ledger.rejections`,
  `ledger.idempotency.replays`, `ledger.idempotency.conflicts` e `ledger.accounts.opened`, com rótulos de conjunto
  fixo. O ID da conta vai só para o span (`ledger.account_id`).

**Atenção, semconv de RPC em evolução**: as convenções semânticas de RPC do OpenTelemetry ainda não são estáveis, e
nomes de métricas e atributos já mudaram entre versões (o BFF e o ledger divergem justamente por isso). A unificação
segue a versão que o `otelgrpc` do ledger emite hoje; a cada atualização do `otelgrpc` ou do SDK JS, conferir se os
nomes mudaram, porque dashboards e alertas (Fase 6) dependem deles.

**Unificação feita (passo 5)**, conferida no código-fonte do `otelgrpc` v0.72, que segue a semconv **v1.43.0**: o
BFF passou a emitir os mesmos nomes do ledger.

| | Antes, no BFF | Agora, nos dois |
|---|---|---|
| Métrica | `rpc.client.duration` | `rpc.client.call.duration` (ledger: `rpc.server.call.duration`) |
| Método | `rpc.service` + `rpc.method` curto | `rpc.method` completo (`conta.ledger.v1.LedgerService/Debit`) |
| Sistema | `rpc.system` | `rpc.system.name` = `grpc` |
| Status | `rpc.grpc.status_code` numérico | `rpc.response.status_code` com o nome canônico (`OK`, `UNAVAILABLE`, `CANCELLED`) |

No Prometheus: `rpc_client_call_duration_seconds_*` e `rpc_server_call_duration_seconds_*`, com
`rpc_response_status_code`. O painel de falhas passa a contar só falhas do servidor (`UNKNOWN`, `DEADLINE_EXCEEDED`,
`INTERNAL`, `UNAVAILABLE`, `DATA_LOSS`), porque `NOT_FOUND`, `FAILED_PRECONDITION` e `ALREADY_EXISTS` agora são
recusas de negócio esperadas, não falhas da dependência.

## Consequências

- O saldo de cliente é lido em O(1) e nunca fica negativo, por três barreiras: domínio, `FOR UPDATE` e `CHECK`.
  O lock é a barreira que importa: numa checagem de mutação (passo 4), sem o `FOR UPDATE` o teste de débitos
  concorrentes falhou por *lost update* (50 débitos aceitos onde cabiam 33; conta que devia zerar terminou com
  5.000), e o `CHECK` não disparou, porque o saldo nunca ficou negativo: o dinheiro simplesmente "apareceu".
- Débitos na mesma conta são serializados: a latência da conta quente (Fase 7) cresce com a fila no lock. É o custo
  aceito; a Fase 7 vai medi-lo.
- Contas internas não têm saldo gravado nem lock, então não viram gargalo global; consultar o saldo delas custa um
  `SUM`, aceitável porque só a conciliação e os testes o fazem.
- O saldo gravado é uma cópia derivada das pernas; um teste de integração (e, na Fase 5, a conciliação) confere
  `balance == soma das pernas`.
- Toda escrita grava uma linha em `idempotency_keys`. **Pendências**, a resolver juntas:
  - **Expurgo**: não há retenção definida; a tabela cresce sem limite (aceitável com o volume desta fase).
  - **Dado pessoal duplicado**: a resposta gravada de um `OpenAccount` bem-sucedido contém a `Account` com o **CPF
    completo**. O CPF passa a existir em dois lugares (`accounts` e `idempotency_keys`), o segundo sem retenção
    definida. Na definição do expurgo, decidir também se a resposta gravada guarda só o `account_id` e remonta a
    conta na repetição, ou se o CPF é mascarado nela.
- Uma recusa de negócio é definitiva para aquela chave: o cliente que quiser tentar de novo precisa de chave nova.
- Um CPF, uma conta: garantido pelo índice único, não por verificação prévia (que teria corrida entre ler e inserir).
- Novos motivos de crédito e débito (estorno, tarifa) entram como novos valores de enum e novas contas internas, sem
  quebrar o contrato.
- O serviço passa a usar um papel Postgres sem DDL; as migrations rodam com outro papel.
