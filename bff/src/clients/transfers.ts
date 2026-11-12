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
  ErrorReason,
  ErrorReasonSchema,
  PixKeyType,
  PixReason,
  PixState,
  PixStatus,
  TransfersService,
  type Pix as PixMessage,
} from "@conta/contracts/conta/transfers/v1/transfers_pb";
import { ErrorInfoSchema } from "@conta/contracts/google/rpc/error_details_pb";
import { PingService } from "@conta/contracts/conta/ping/v1/ping_pb";
import { fail, ok } from "../result.js";
import type { DependencyError, DependencyResult } from "../errors.js";
import { otelContextKey } from "../platform/connect-telemetry.js";
import type { CallContext } from "./ledger.js";

const DEPENDENCY = "transfers";

export type PixStatusName = "processing" | "completed" | "failed" | "rejected";
export type PixStateName =
  | "accepted"
  | "validated"
  | "debited"
  | "settling"
  | "pending_confirmation"
  | "settled"
  | "completed"
  | "reversing"
  | "reversed"
  | "rejected";
export type PixReasonName =
  | "antifraud_denied"
  | "limit_exceeded"
  | "destination_invalid"
  | "same_account"
  | "insufficient_funds"
  | "dependency_unavailable"
  | "settlement_rejected"
  | "settlement_not_sent"
  | "source_account_invalid";

export interface PixChange {
  readonly state: PixStateName;
  readonly reason: PixReasonName | undefined;
  readonly at: Date;
}

/** O Pix como o transfers o descreve, em tipos do BFF (sem protobuf). Centavos em `bigint`. */
export interface Pix {
  readonly pixId: string;
  readonly status: PixStatusName;
  readonly state: PixStateName;
  readonly reason: PixReasonName | undefined;
  readonly amountCents: bigint;
  readonly description: string;
  readonly destinationKeyMasked: string;
  readonly confirmationOverdue: boolean;
  readonly createdAt: Date;
  readonly updatedAt: Date;
  readonly history: readonly PixChange[];
}

export interface Limits {
  readonly dailyLimitCents: bigint;
  readonly usedTodayCents: bigint;
  readonly availableTodayCents: bigint;
  readonly resetsAt: Date;
}

export interface StartPixInput {
  readonly accountId: string;
  /** Repassada como veio do cliente: o transfers garante "mesma chave, mesmo Pix". */
  readonly idempotencyKey: string;
  /** CPF, só dígitos. Dado sensível: não vai para log nem trace. */
  readonly destinationCpf: string;
  readonly amountCents: bigint;
  readonly description: string;
}

/**
 * Adaptador do transfers. É o único lugar do BFF que conhece o gRPC dele: devolve tipos do BFF
 * e falhas esperadas como dados, nunca mensagens protobuf nem ConnectError.
 */
export interface TransfersClient {
  /** Diagnóstico e aquecimento da conexão no boot. */
  ping(message: string, call: CallContext): Promise<DependencyResult<string>>;
  startPix(input: StartPixInput, call: CallContext): Promise<DependencyResult<Pix>>;
  /** `not_found` se o Pix não existe ou não é da conta (o transfers não revela qual). */
  getPix(accountId: string, pixId: string, call: CallContext): Promise<DependencyResult<Pix>>;
  getLimits(accountId: string, call: CallContext): Promise<DependencyResult<Limits>>;
}

export interface TransfersClientOptions {
  readonly timeoutMs: number;
  /** Prazo da seção de limites da home (opcional, menor que o SLO de 300 ms). */
  readonly limitsTimeoutMs: number;
}

// O transfers manda o nome completo do .proto no ErrorInfo; o nome vem do descritor gerado.
function reasonName(reason: number): string | undefined {
  return ErrorReasonSchema.values.find((v) => v.number === reason)?.name;
}
const INVALID_REQUEST = reasonName(ErrorReason.INVALID_REQUEST);
const KEY_REUSED = reasonName(ErrorReason.IDEMPOTENCY_KEY_REUSED);
const PIX_NOT_FOUND = reasonName(ErrorReason.PIX_NOT_FOUND);

export function createTransfersClient(
  transport: Transport,
  options: TransfersClientOptions,
): TransfersClient {
  const transfers = createClient(TransfersService, transport);
  const ping = createClient(PingService, transport);
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
      run(async () => (await ping.ping({ message }, callOptions(options.timeoutMs, call))).service),

    startPix: (input, call) =>
      run(async () => {
        const res = await transfers.startPix(
          {
            idempotencyKey: input.idempotencyKey,
            accountId: input.accountId,
            destinationKey: { type: PixKeyType.CPF, value: input.destinationCpf },
            amountCents: input.amountCents,
            description: input.description,
          },
          callOptions(options.timeoutMs, call),
        );
        return toPix(res.pix);
      }),

    getPix: (accountId, pixId, call) =>
      run(async () => {
        const res = await transfers.getPix(
          { accountId, pixId },
          callOptions(options.timeoutMs, call),
        );
        return toPix(res.pix);
      }),

    getLimits: (accountId, call) =>
      run(async () => {
        const res = await transfers.getLimits(
          { accountId },
          callOptions(options.limitsTimeoutMs, call),
        );
        return {
          dailyLimitCents: res.dailyLimitCents,
          usedTodayCents: res.usedTodayCents,
          availableTodayCents: res.availableTodayCents,
          resetsAt: res.resetsAt ? timestampDate(res.resetsAt) : new Date(0),
        };
      }),
  };
}

/**
 * Enum gerado → nome do BFF. `switch` exaustivo: um estado novo no .proto faz o BFF deixar de
 * compilar até alguém decidir como mostrá-lo, em vez de quebrar em produção.
 */
function stateName(s: PixState): PixStateName {
  switch (s) {
    case PixState.ACCEPTED:
      return "accepted";
    case PixState.VALIDATED:
      return "validated";
    case PixState.DEBITED:
      return "debited";
    case PixState.SETTLING:
      return "settling";
    case PixState.PENDING_CONFIRMATION:
      return "pending_confirmation";
    case PixState.SETTLED:
      return "settled";
    case PixState.COMPLETED:
      return "completed";
    case PixState.REVERSING:
      return "reversing";
    case PixState.REVERSED:
      return "reversed";
    case PixState.REJECTED:
      return "rejected";
    case PixState.UNSPECIFIED:
      throw new Error("transfers devolveu um Pix sem estado");
    default:
      return s satisfies never;
  }
}

function statusName(s: PixStatus): PixStatusName {
  switch (s) {
    case PixStatus.PROCESSING:
      return "processing";
    case PixStatus.COMPLETED:
      return "completed";
    case PixStatus.FAILED:
      return "failed";
    case PixStatus.REJECTED:
      return "rejected";
    case PixStatus.UNSPECIFIED:
      throw new Error("transfers devolveu um Pix sem status");
    default:
      return s satisfies never;
  }
}

function reasonOf(r: PixReason): PixReasonName | undefined {
  switch (r) {
    case PixReason.UNSPECIFIED:
      return undefined;
    case PixReason.ANTIFRAUD_DENIED:
      return "antifraud_denied";
    case PixReason.LIMIT_EXCEEDED:
      return "limit_exceeded";
    case PixReason.DESTINATION_INVALID:
      return "destination_invalid";
    case PixReason.SAME_ACCOUNT:
      return "same_account";
    case PixReason.INSUFFICIENT_FUNDS:
      return "insufficient_funds";
    case PixReason.DEPENDENCY_UNAVAILABLE:
      return "dependency_unavailable";
    case PixReason.SETTLEMENT_REJECTED:
      return "settlement_rejected";
    case PixReason.SETTLEMENT_NOT_SENT:
      return "settlement_not_sent";
    case PixReason.SOURCE_ACCOUNT_INVALID:
      return "source_account_invalid";
    default:
      return r satisfies never;
  }
}

function toPix(p: PixMessage | undefined): Pix {
  if (p === undefined) throw new Error("transfers respondeu sem o Pix");
  return {
    pixId: p.pixId,
    status: statusName(p.status),
    state: stateName(p.state),
    reason: reasonOf(p.reason),
    amountCents: p.amountCents,
    description: p.description,
    destinationKeyMasked: p.destinationKeyMasked,
    confirmationOverdue: p.confirmationOverdue,
    createdAt: p.createdAt ? timestampDate(p.createdAt) : new Date(0),
    updatedAt: p.updatedAt ? timestampDate(p.updatedAt) : new Date(0),
    history: p.history.map((c) => ({
      state: stateName(c.toState),
      reason: reasonOf(c.reason),
      at: c.at ? timestampDate(c.at) : new Date(0),
    })),
  };
}

function toDependencyError(err: unknown): DependencyError {
  const e = ConnectError.from(err);
  const [info] = e.findDetails(ErrorInfoSchema);
  const reason = info?.reason;
  if (e.code === Code.DeadlineExceeded) return { kind: "timeout", dependency: DEPENDENCY };
  if (e.code === Code.Unavailable) return { kind: "unavailable", dependency: DEPENDENCY };
  if (e.code === Code.InvalidArgument && reason === INVALID_REQUEST) {
    // O campo vem no ErrorInfo; a mensagem é a do transfers, sem dado do cliente (o CPF não volta).
    const field = info?.metadata.field;
    return { kind: "invalid_argument", detail: field ? `campo inválido: ${field}` : e.rawMessage };
  }
  if (e.code === Code.AlreadyExists && reason === KEY_REUSED)
    return { kind: "idempotency_key_reused" };
  if (e.code === Code.NotFound && reason === PIX_NOT_FOUND) {
    return { kind: "not_found", dependency: DEPENDENCY };
  }
  return { kind: "unexpected", dependency: DEPENDENCY };
}
