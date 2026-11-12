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

export const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));

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
