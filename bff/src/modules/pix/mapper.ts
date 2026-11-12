import type { Pix } from "../../clients/transfers.js";
import { centsToNumber } from "../../money.js";
import type { PixAcceptedView, PixView } from "./schema.js";

/** Pix do transfers → resposta do aceite (202). Só adapta forma. */
export function toPixAccepted(p: Pix): PixAcceptedView {
  return { pixId: p.pixId, status: p.status, state: p.state };
}

/** Pix do transfers → view model da tela de acompanhamento. Só adapta forma, nunca decide. */
export function toPixView(p: Pix): PixView {
  return {
    pixId: p.pixId,
    status: p.status,
    state: p.state,
    ...(p.reason !== undefined ? { reason: p.reason } : {}),
    amountCents: centsToNumber(p.amountCents),
    description: p.description,
    destinationKeyMasked: p.destinationKeyMasked,
    confirmationOverdue: p.confirmationOverdue,
    createdAt: p.createdAt.toISOString(),
    updatedAt: p.updatedAt.toISOString(),
    history: p.history.map((c) => ({
      state: c.state,
      ...(c.reason !== undefined ? { reason: c.reason } : {}),
      at: c.at.toISOString(),
    })),
  };
}
