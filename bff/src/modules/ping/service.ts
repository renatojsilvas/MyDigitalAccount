import type { Context } from "@opentelemetry/api";
import type { LedgerClient } from "../../clients/ledger.js";
import type { DependencyResult } from "../../errors.js";
import { fail, ok } from "../../result.js";
import { toPingView } from "./mapper.js";
import type { PingView } from "./schema.js";

export interface PingService {
  ping(message: string, context: Context): Promise<DependencyResult<PingView>>;
}

export function createPingService(deps: { readonly ledger: LedgerClient }): PingService {
  return {
    async ping(message, context) {
      const result = await deps.ledger.ping(message, { context });
      return result.ok ? ok(toPingView(result.value)) : fail(result.error);
    },
  };
}
