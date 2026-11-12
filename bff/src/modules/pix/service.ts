import type { Context } from "@opentelemetry/api";
import type { Pix, TransfersClient } from "../../clients/transfers.js";
import type { BffError } from "../../errors.js";
import type { Result } from "../../result.js";
import type { StartPixBody } from "./schema.js";

export interface PixService {
  start(
    accountId: string,
    idempotencyKey: string,
    body: StartPixBody,
    context: Context,
  ): Promise<Result<Pix, BffError>>;
  get(accountId: string, pixId: string, context: Context): Promise<Result<Pix, BffError>>;
}

/**
 * O BFF só repassa: a conta vem do token, a chave de idempotência vem do cliente e vai intacta,
 * e quem decide tudo sobre o Pix é o transfers (regra 1). Não há agregação aqui.
 */
export function createPixService(deps: { readonly transfers: TransfersClient }): PixService {
  return {
    start: (accountId, idempotencyKey, body, context) =>
      deps.transfers.startPix(
        {
          accountId,
          idempotencyKey,
          destinationCpf: body.destinationKey.value,
          amountCents: BigInt(body.amountCents),
          description: body.description ?? "",
        },
        { context },
      ),
    get: (accountId, pixId, context) => deps.transfers.getPix(accountId, pixId, { context }),
  };
}
