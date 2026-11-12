# ADR-0009: Ledger: estorno e referência externa nos lançamentos

- **Status**: Aceito (Fase 3). Estorno: opção A (RPC `Reverse`, `reverses_entry_id UNIQUE`), com o ajuste do autor:
  o motivo do lançamento também é exposto no `Posting`, num enum.
- **Data**: 2026-11-12

## Contexto

A Fase 3 precisa de duas coisas do ledger (pendências registradas no ADR-0004 e no estado do projeto):

1. **Estorno**: devolver um débito de Pix quando o SPI recusa ou quando a liquidação comprovadamente não foi enviada
   (ADR-0007). Lançamentos são append-only: o estorno é **um lançamento novo, com as pernas invertidas**, que aponta
   para o original. O mesmo débito **nunca** pode ser estornado duas vezes, nem com chaves de idempotência diferentes.
2. **Referência externa**: cada lançamento passa a levar o ID do negócio que o originou (o `transfer_id`), para a
   conciliação da Fase 5 casar `transfers` com `ledger`.

## 1. Estorno: RPC dedicada

| | **A. RPC `Reverse`** (escolhida) | B. `Credit` com `CREDIT_REASON_REVERSAL` |
|---|---|---|
| O que quem chama informa | Só o lançamento original | Valor, conta e o original |
| Valor e contas do estorno | Tirados do original pelo ledger: não há como errar | Informados por quem chama; o ledger teria de conferir contra o original de qualquer jeito |
| Contrato | Uma RPC nova (aditiva) | Um campo obrigatório só para um dos motivos |
| Semântica | "Desfazer aquele lançamento" | "Creditar", com uma exceção escondida |

**Decisão: A.**

```proto
rpc Reverse(ReverseRequest) returns (ReverseResponse);
message ReverseRequest {
  string idempotency_key = 1;
  string entry_id = 2;            // lançamento a estornar
  string external_reference = 3;  // opcional; o transfers manda o transfer_id
  string description = 4;
}
message ReverseResponse { Posting posting = 1; }   // a perna do cliente no estorno
```

**Regras do domínio:**

- Só se estorna um lançamento de motivo `EXTERNAL` cuja perna de cliente é **débito** (o Pix enviado). Estornar um
  crédito poderia deixar o saldo negativo, um estorno não se estorna, e o saldo inicial do seed não é estornável.
  Qualquer outro caso: `FAILED_PRECONDITION` com `ENTRY_NOT_REVERSIBLE`. Ampliar isso depois é uma mudança
  compatível.
- O estorno é **integral**: mesmo valor, mesmas contas, sentidos trocados (D `settlement` / C cliente). Não há
  estorno parcial.
- Lançamento inexistente: `NOT_FOUND` com `ENTRY_NOT_FOUND`.

**Como impedir o estorno duplo**, em três camadas:

1. **Idempotência** (ADR-0004): o `transfers` usa sempre a chave `transfer:{id}:reversal`. Repetir devolve o mesmo
   estorno.
2. **Regra do domínio sob o lock da conta**: o caso de uso trava a conta de cliente do lançamento original (o mesmo
   `FOR UPDATE` do débito e do crédito) e só então procura um estorno existente. Como todo estorno de um lançamento
   daquela conta passa pelo mesmo lock, a checagem é livre de corrida. Encontrou: `FAILED_PRECONDITION` com
   `ENTRY_ALREADY_REVERSED`, recusa **gravada** na chave e repetida igual.
3. **Banco**: `journal_entries.reverses_entry_id uuid UNIQUE REFERENCES journal_entries (id)`. Se o domínio errar, o
   segundo estorno falha no `INSERT` e a transação inteira é desfeita.

## 2. Motivo exposto no `Posting`

O lançamento passa a gravar o seu motivo (`journal_entries.reason`: `opening`, `external`, `reversal`), e o `Posting`
ganha:

```proto
enum EntryReason {
  ENTRY_REASON_UNSPECIFIED = 0;
  ENTRY_REASON_OPENING = 1;    // saldo inicial (seed)
  ENTRY_REASON_EXTERNAL = 2;   // dinheiro que entra ou sai pelo SPI
  ENTRY_REASON_REVERSAL = 3;   // estorno de um lançamento EXTERNAL
}
message Posting {
  ...
  EntryReason reason = 8;
  string external_reference = 9;   // vazio se não houver
  string reverses_entry_id = 10;   // só nos estornos
}
```

Com isso, o extrato mostra "Estorno" a partir de um campo tipado, sem o BFF interpretar texto. Os enums de pedido
(`CreditReason`, `DebitReason`) continuam como estão: eles dizem **por que** se credita ou debita (e escolhem a
contrapartida); o `EntryReason` diz **o que o lançamento é**, e só o ledger o produz.

**Lançamentos já gravados**: a migration `00002` preenche `reason` a partir da contrapartida (perna em
`opening_balance` = `opening`; em `settlement` = `external`) e só então torna a coluna obrigatória. É a única vez que
uma migration altera linhas de `journal_entries`, e ela roda com o papel `ledger_owner`; o papel do serviço continua
sem `UPDATE` na tabela.

## 3. Referência externa

- Campo `external_reference` opcional em `CreditRequest`, `DebitRequest`, `ReverseRequest` e `Posting`, gravado em
  `journal_entries.external_reference`.
- Formato: de 1 a 64 caracteres ASCII visíveis (o `transfer_id` é um UUID), conferido no domínio e por `CHECK` no
  banco. Não é única: um Pix gera até três lançamentos com a mesma referência (débito, crédito no destino ou estorno).
- Entra no hash do pedido de idempotência **só quando informado**. Assim, o hash de um pedido antigo, sem referência,
  continua o mesmo depois da mudança, e uma repetição feita antes da atualização não vira "chave reutilizada".
- O índice por referência (e a listagem por janela, em streaming) entram na Fase 5, junto com quem os usa.

## Consequências

- Contrato com mudanças só aditivas (RPC, campos e valores de enum novos): o `buf breaking` passa. Novos
  `ErrorReason`: `ENTRY_NOT_FOUND` (6), `ENTRY_ALREADY_REVERSED` (7), `ENTRY_NOT_REVERSIBLE` (8).
- Respostas gravadas na tabela de idempotência **antes** da migration não têm `reason`: uma repetição delas devolve
  `ENTRY_REASON_UNSPECIFIED`. Só afeta bancos de desenvolvimento antigos.
- Métricas: a operação `reverse` entra nos contadores existentes (`ledger_entries_total{ledger_operation="reverse",
  ledger_reason="reversal"}`), e as recusas ganham `entry_not_found`, `entry_already_reversed` e
  `entry_not_reversible`.
- O estorno trava a conta de cliente, como o débito: na conta quente da Fase 7, os estornos disputam o mesmo lock.
