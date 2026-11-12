import type { Posting } from "../../clients/ledger.js";
import type { HomeData } from "./service.js";
import type { HomeView, StatementItemView } from "./schema.js";

/** `bigint` do protobuf → inteiro do JSON. Estourar o limite seguro é falha, nunca arredondamento. */
export function centsToNumber(cents: bigint): number {
  if (cents > BigInt(Number.MAX_SAFE_INTEGER) || cents < BigInt(Number.MIN_SAFE_INTEGER)) {
    throw new RangeError("valor em centavos fora do intervalo inteiro seguro do JSON");
  }
  return Number(cents);
}

function toStatementItem(p: Posting): StatementItemView {
  return {
    id: p.entryId,
    direction: p.direction,
    amountCents: centsToNumber(p.amountCents),
    balanceAfterCents: centsToNumber(p.balanceAfterCents),
    description: p.description,
    postedAt: p.postedAt.toISOString(),
  };
}

/** Dados de domínio da home → view model da tela. Só adapta forma, nunca calcula valor. */
export function toHomeView(data: HomeData): HomeView {
  return {
    balanceCents: centsToNumber(data.balanceCents),
    statement:
      data.statement.status === "ok"
        ? { status: "ok", items: data.statement.postings.map(toStatementItem) }
        : { status: "unavailable", reason: data.statement.reason },
  };
}
