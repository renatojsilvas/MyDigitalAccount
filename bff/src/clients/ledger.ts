import {
  Code,
  ConnectError,
  createClient,
  createContextValues,
  type CallOptions,
  type Transport,
} from "@connectrpc/connect";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import {
  Direction,
  ErrorReason,
  ErrorReasonSchema,
  LedgerService,
  type Posting as PostingMessage,
} from "@conta/contracts/conta/ledger/v1/ledger_pb";
import { ErrorInfoSchema } from "@conta/contracts/google/rpc/error_details_pb";
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

/** Conta de cliente, sem o CPF: o BFF não precisa dele depois do login. */
export interface Account {
  readonly accountId: string;
  readonly holderName: string;
}

export interface Posting {
  readonly entryId: string;
  readonly direction: "debit" | "credit";
  /** Centavos, sempre positivos; o sentido vem de `direction`. `bigint` até o mapper. */
  readonly amountCents: bigint;
  readonly balanceAfterCents: bigint;
  readonly description: string;
  readonly postedAt: Date;
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
  /** Saldo atual em centavos. */
  getBalance(accountId: string, call: CallContext): Promise<DependencyResult<bigint>>;
  /** Do lançamento mais recente para o mais antigo, até `pageSize`. */
  listStatement(
    accountId: string,
    pageSize: number,
    call: CallContext,
  ): Promise<DependencyResult<readonly Posting[]>>;
  /** Busca a conta pelo CPF (só dígitos). `not_found` se o CPF não tem conta. */
  findAccountByDocument(document: string, call: CallContext): Promise<DependencyResult<Account>>;
}

export interface LedgerClientOptions {
  /** Prazo padrão. Vai ao ledger no header `grpc-timeout` e também aborta aqui. */
  readonly timeoutMs: number;
  readonly balanceTimeoutMs: number;
  readonly statementTimeoutMs: number;
}

// protoc-gen-es remove o prefixo dos membros do enum em TS ("ACCOUNT_NOT_FOUND"), mas o ledger
// manda o nome completo do .proto no ErrorInfo ("ERROR_REASON_ACCOUNT_NOT_FOUND"). O nome vem do
// descritor gerado, nunca de uma string escrita à mão.
function reasonName(reason: number): string | undefined {
  return ErrorReasonSchema.values.find((v) => v.number === reason)?.name;
}
const ACCOUNT_NOT_FOUND = reasonName(ErrorReason.ACCOUNT_NOT_FOUND);

export function createLedgerClient(
  transport: Transport,
  options: LedgerClientOptions,
): LedgerClient {
  const ping = createClient(PingService, transport);
  const ledger = createClient(LedgerService, transport);

  const callOptions = (timeoutMs: number, call: CallContext): CallOptions => ({
    timeoutMs,
    ...(call.signal ? { signal: call.signal } : {}),
    contextValues: createContextValues().set(otelContextKey, call.context),
  });

  async function run<T>(fn: () => Promise<T>): Promise<DependencyResult<T>> {
    try {
      return ok(await fn());
    } catch (err) {
      return fail(toDependencyError(err));
    }
  }

  return {
    ping: (message, call) =>
      run(async () => {
        const res = await ping.ping({ message }, callOptions(options.timeoutMs, call));
        return { message: res.message, service: res.service };
      }),

    getBalance: (accountId, call) =>
      run(async () => {
        const res = await ledger.getBalance(
          { accountId },
          callOptions(options.balanceTimeoutMs, call),
        );
        return res.balanceCents;
      }),

    listStatement: (accountId, pageSize, call) =>
      run(async () => {
        const res = await ledger.listStatement(
          { accountId, pageSize },
          callOptions(options.statementTimeoutMs, call),
        );
        return res.postings.map(toPosting);
      }),

    findAccountByDocument: (document, call) =>
      run(async () => {
        const res = await ledger.getAccountByDocument(
          { holderDocument: document },
          callOptions(options.timeoutMs, call),
        );
        return {
          accountId: res.account?.accountId ?? "",
          holderName: res.account?.holderName ?? "",
        };
      }),
  };
}

/** Mensagem gerada → tipo de domínio. Se um campo do .proto mudar, isto deixa de compilar. */
function toPosting(p: PostingMessage): Posting {
  return {
    entryId: p.entryId,
    direction: p.direction === Direction.CREDIT ? "credit" : "debit",
    amountCents: p.amountCents,
    balanceAfterCents: p.balanceAfterCents,
    description: p.description,
    postedAt: p.postedAt ? timestampDate(p.postedAt) : new Date(0),
  };
}

function toDependencyError(err: unknown): DependencyError {
  const e = ConnectError.from(err);
  // Só os códigos que o BFF sabe tratar viram falha específica; o resto é "inesperado".
  if (e.code === Code.DeadlineExceeded) return { kind: "timeout", dependency: DEPENDENCY };
  if (e.code === Code.Unavailable) return { kind: "unavailable", dependency: DEPENDENCY };
  if (e.code === Code.InvalidArgument) return { kind: "invalid_argument", detail: e.rawMessage };
  if (e.code === Code.NotFound) {
    // NOT_FOUND só significa "conta inexistente" quando o motivo diz isso (ErrorInfo).
    const [info] = e.findDetails(ErrorInfoSchema);
    if (info !== undefined && info.reason === ACCOUNT_NOT_FOUND) {
      return { kind: "not_found", dependency: DEPENDENCY };
    }
  }
  return { kind: "unexpected", dependency: DEPENDENCY };
}
