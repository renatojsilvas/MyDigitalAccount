import type { Context } from "@opentelemetry/api";
import type { Account, LedgerClient } from "../../clients/ledger.js";
import type { BffError } from "../../errors.js";
import { fail, ok, type Result } from "../../result.js";

export interface AuthService {
  /** Acha a conta do CPF. Sem conta, é 401 (e não 404): não revela quais CPFs existem. */
  demoLogin(document: string, context: Context): Promise<Result<Account, BffError>>;
}

export function createAuthService(deps: { readonly ledger: LedgerClient }): AuthService {
  return {
    async demoLogin(document, context) {
      const result = await deps.ledger.findAccountByDocument(document, { context });
      if (result.ok) return ok(result.value);
      if (result.error.kind === "not_found") return fail({ kind: "unauthenticated" });
      return fail(result.error);
    },
  };
}
