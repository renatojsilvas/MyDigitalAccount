import {
  Code,
  ConnectError,
  createRouterTransport,
  type Interceptor,
  type ServiceImpl,
  type Transport,
} from "@connectrpc/connect";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import {
  Direction,
  ErrorReason,
  ErrorReasonSchema,
  LedgerService,
} from "@conta/contracts/conta/ledger/v1/ledger_pb";
import { ErrorInfoSchema } from "@conta/contracts/google/rpc/error_details_pb";

export type LedgerImpl = Partial<ServiceImpl<typeof LedgerService>>;

/** Transporte em memória com o ledger falso (o mesmo contrato gerado do .proto). */
export function fakeLedgerTransport(impl: LedgerImpl, interceptors: Interceptor[] = []): Transport {
  return createRouterTransport(({ service }) => service(LedgerService, impl), {
    transport: { interceptors },
  });
}

/** Erro de domínio como o ledger real o devolve: status gRPC + google.rpc.ErrorInfo. */
export function rejection(code: Code, reason: number, message = "recusado"): ConnectError {
  const name = ErrorReasonSchema.values.find((v) => v.number === reason)?.name;
  return new ConnectError(message, code, undefined, [
    {
      desc: ErrorInfoSchema,
      value: { reason: name ?? "", domain: "ledger.conta.local", metadata: {} },
    },
  ]);
}

export const accountNotFound = (): ConnectError =>
  rejection(Code.NotFound, ErrorReason.ACCOUNT_NOT_FOUND, "account not found");

/**
 * Handler que nunca responde por conta própria: só termina quando o cliente cancela (o prazo
 * estoura). Assim "lento" não depende de quanto tempo o relógio real leva: o teste usa um prazo
 * curto e o resultado é o mesmo com a máquina folgada ou sobrecarregada.
 */
export function hang(ctx: { readonly signal: AbortSignal }): Promise<never> {
  return new Promise<never>((_, reject) => {
    const onAbort = () => {
      reject(new ConnectError("cancelado pelo cliente", Code.Canceled));
    };
    if (ctx.signal.aborted) onAbort();
    else ctx.signal.addEventListener("abort", onAbort, { once: true });
  });
}

/** Portão: `wait()` só resolve depois de `open()`. Ordena eventos sem depender de duração. */
export function gate() {
  let open!: () => void;
  const opened = new Promise<void>((resolve) => {
    open = resolve;
  });
  return { open, wait: () => opened };
}

export function posting(n: number, direction: Direction = Direction.CREDIT) {
  return {
    entryId: `entry-${String(n)}`,
    accountId: "acc-1",
    direction,
    amountCents: BigInt(n) * 100n,
    balanceAfterCents: BigInt(n) * 1000n,
    description: `lançamento ${String(n)}`,
    postedAt: timestampFromDate(new Date(Date.UTC(2026, 10, 12, 10, n, 0))),
  };
}

export { Code, ConnectError, Direction, ErrorReason };
