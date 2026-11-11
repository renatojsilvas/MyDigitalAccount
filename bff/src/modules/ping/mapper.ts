import type { PingReply } from "../../clients/ledger.js";
import type { PingView } from "./schema.js";

/** Resposta do ledger (domínio) para o view model que o cliente HTTP recebe. */
export function toPingView(reply: PingReply): PingView {
  return { echo: reply.message, servedBy: reply.service };
}
