import {
  Code,
  ConnectError,
  createClient,
  createContextValues,
  type Transport,
} from "@connectrpc/connect";
import { PingService } from "@conta/contracts/conta/ping/v1/ping_pb";
import type { Context } from "@opentelemetry/api";
import { fail, ok } from "../result.js";
import type { DependencyError, DependencyResult } from "../errors.js";
import { otelContextKey } from "../platform/connect-telemetry.js";

const DEPENDENCY = "ledger";

export interface PingReply {
  readonly message: string;
  readonly service: string;
}

export interface CallContext {
  /** Contexto de trace da requisição que originou a chamada. */
  readonly context: Context;
  /** Cancelamento externo opcional; o prazo da chamada já vem do timeout do cliente. */
  readonly signal?: AbortSignal;
}

/**
 * Adaptador do ledger. É o único lugar do BFF que conhece gRPC: devolve tipos de domínio
 * simples e falhas esperadas como dados, nunca mensagens protobuf nem ConnectError.
 */
export interface LedgerClient {
  ping(message: string, call: CallContext): Promise<DependencyResult<PingReply>>;
}

export interface LedgerClientOptions {
  /** Prazo de cada chamada. Vai ao ledger no header `grpc-timeout` e também aborta aqui. */
  readonly timeoutMs: number;
}

export function createLedgerClient(
  transport: Transport,
  options: LedgerClientOptions,
): LedgerClient {
  const client = createClient(PingService, transport);

  return {
    async ping(message, call) {
      try {
        const res = await client.ping(
          { message },
          {
            timeoutMs: options.timeoutMs,
            ...(call.signal ? { signal: call.signal } : {}),
            contextValues: createContextValues().set(otelContextKey, call.context),
          },
        );
        return ok({ message: res.message, service: res.service });
      } catch (err) {
        return fail(toDependencyError(err));
      }
    },
  };
}

function toDependencyError(err: unknown): DependencyError {
  const e = ConnectError.from(err);
  // Só os códigos que o BFF sabe tratar viram falha específica; o resto é "inesperado".
  if (e.code === Code.DeadlineExceeded) return { kind: "timeout", dependency: DEPENDENCY };
  if (e.code === Code.Unavailable) return { kind: "unavailable", dependency: DEPENDENCY };
  if (e.code === Code.InvalidArgument) return { kind: "invalid_argument", detail: e.rawMessage };
  return { kind: "unexpected", dependency: DEPENDENCY };
}
