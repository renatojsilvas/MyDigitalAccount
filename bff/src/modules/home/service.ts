import type { Context } from "@opentelemetry/api";
import type { LedgerClient, Posting } from "../../clients/ledger.js";
import type { Limits, TransfersClient } from "../../clients/transfers.js";
import type { BffError, DependencyError } from "../../errors.js";
import { fail, ok, type Result } from "../../result.js";
import type { HomeObserver } from "./observe.js";
import type { DegradedReason } from "./schema.js";

/** Quantos lançamentos a home mostra. */
export const HOME_STATEMENT_SIZE = 10;

export type StatementOutcome =
  | { readonly status: "ok"; readonly postings: readonly Posting[] }
  | { readonly status: "unavailable"; readonly reason: DegradedReason };

export type LimitsOutcome =
  | { readonly status: "ok"; readonly limits: Limits }
  | { readonly status: "unavailable"; readonly reason: DegradedReason };

/** Dados de domínio da home, antes do mapper. */
export interface HomeData {
  readonly balanceCents: bigint;
  readonly statement: StatementOutcome;
  readonly limits: LimitsOutcome;
}

export interface HomeService {
  home(
    accountId: string,
    context: Context,
    signal?: AbortSignal,
  ): Promise<Result<HomeData, BffError>>;
}

export interface HomeServiceDeps {
  readonly ledger: LedgerClient;
  readonly transfers: TransfersClient;
  readonly observer: HomeObserver;
}

function degradedReason(error: DependencyError): DegradedReason {
  switch (error.kind) {
    case "timeout":
      return "timeout";
    case "unavailable":
      return "unavailable";
    case "not_found":
    case "invalid_argument":
    case "idempotency_key_reused":
    case "unexpected":
      return "error";
    default:
      return error satisfies never;
  }
}

/**
 * Agregação com degradação parcial (ADR-0005). As três chamadas saem em paralelo, cada uma
 * com o próprio prazo (definido no adaptador). O saldo é essencial: sem ele a home falha. O
 * extrato (ledger) e os limites (transfers) são opcionais: se faltarem, a home segue com a
 * seção marcada como indisponível.
 */
export function createHomeService(deps: HomeServiceDeps): HomeService {
  return {
    async home(accountId, context, signal) {
      const call = { context, ...(signal ? { signal } : {}) };
      // allSettled: uma falha não cancela nem esconde a outra. O adaptador nunca lança (devolve
      // Result), então "rejected" só acontece por bug: o saldo propaga (500), o extrato degrada.
      const [balance, statement, limits] = await Promise.allSettled([
        deps.ledger.getBalance(accountId, call),
        deps.ledger.listStatement(accountId, HOME_STATEMENT_SIZE, call),
        deps.transfers.getLimits(accountId, call),
      ]);

      if (balance.status === "rejected") throw balance.reason;
      if (!balance.value.ok) {
        // A conta vem do token: se ela não existe, o token está velho (ex.: banco recriado).
        if (balance.value.error.kind === "not_found") return fail({ kind: "unauthenticated" });
        return fail(balance.value.error);
      }

      let outcome: StatementOutcome;
      if (statement.status === "rejected") {
        outcome = { status: "unavailable", reason: "error" };
      } else if (statement.value.ok) {
        outcome = { status: "ok", postings: statement.value.value };
      } else {
        outcome = { status: "unavailable", reason: degradedReason(statement.value.error) };
      }
      if (outcome.status === "unavailable") {
        deps.observer.degraded("statement", outcome.reason, context);
      }

      let limitsOutcome: LimitsOutcome;
      if (limits.status === "rejected") {
        limitsOutcome = { status: "unavailable", reason: "error" };
      } else if (limits.value.ok) {
        limitsOutcome = { status: "ok", limits: limits.value.value };
      } else {
        limitsOutcome = { status: "unavailable", reason: degradedReason(limits.value.error) };
      }
      if (limitsOutcome.status === "unavailable") {
        deps.observer.degraded("limits", limitsOutcome.reason, context);
      }
      return ok({ balanceCents: balance.value.value, statement: outcome, limits: limitsOutcome });
    },
  };
}
