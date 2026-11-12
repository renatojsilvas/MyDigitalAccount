import type { Posting } from "../../clients/ledger.js";
import { centsToNumber } from "../../money.js";
import type { HomeData } from "./service.js";
import type { HomeView, StatementItemView } from "./schema.js";

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
    limits:
      data.limits.status === "ok"
        ? {
            status: "ok",
            dailyLimitCents: centsToNumber(data.limits.limits.dailyLimitCents),
            usedTodayCents: centsToNumber(data.limits.limits.usedTodayCents),
            availableTodayCents: centsToNumber(data.limits.limits.availableTodayCents),
            resetsAt: data.limits.limits.resetsAt.toISOString(),
          }
        : { status: "unavailable", reason: data.limits.reason },
  };
}
