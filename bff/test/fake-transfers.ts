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
  ErrorReason,
  ErrorReasonSchema,
  PixKeyType,
  PixReason,
  PixState,
  PixStatus,
  TransfersService,
} from "@conta/contracts/conta/transfers/v1/transfers_pb";
import { ErrorInfoSchema } from "@conta/contracts/google/rpc/error_details_pb";
import { PingService } from "@conta/contracts/conta/ping/v1/ping_pb";

export type TransfersImpl = Partial<ServiceImpl<typeof TransfersService>>;

/** Transporte em memória com o transfers falso (o mesmo contrato gerado do .proto). */
export function fakeTransfersTransport(
  impl: TransfersImpl,
  interceptors: Interceptor[] = [],
): Transport {
  return createRouterTransport(
    ({ service }) => {
      service(TransfersService, impl);
      service(PingService, { ping: (req) => ({ message: req.message, service: "transfers" }) });
    },
    { transport: { interceptors } },
  );
}

/** Erro de domínio como o transfers real o devolve: status gRPC + google.rpc.ErrorInfo. */
export function transfersRejection(
  code: Code,
  reason: number,
  metadata: Record<string, string> = {},
): ConnectError {
  const name = ErrorReasonSchema.values.find((v) => v.number === reason)?.name;
  return new ConnectError("recusado", code, undefined, [
    {
      desc: ErrorInfoSchema,
      value: { reason: name ?? "", domain: "transfers.conta.local", metadata },
    },
  ]);
}

const at = (minute: number) => timestampFromDate(new Date(Date.UTC(2026, 10, 12, 15, minute, 0)));

/** Um Pix como o transfers devolve (protobuf), no estado pedido. */
export function pixMessage(
  overrides: { state?: PixState; status?: PixStatus; reason?: PixReason; id?: string } = {},
) {
  return {
    pixId: overrides.id ?? "01a1d0a3-fccc-7c3b-a643-8f3eaf603028",
    sourceAccountId: "acc-1",
    destinationAccountId: "acc-2",
    destinationKeyMasked: "***.654.321-**",
    destinationKeyType: PixKeyType.CPF,
    amountCents: 12_345n,
    description: "aluguel",
    state: overrides.state ?? PixState.ACCEPTED,
    status: overrides.status ?? PixStatus.PROCESSING,
    reason: overrides.reason ?? PixReason.UNSPECIFIED,
    confirmationOverdue: false,
    createdAt: at(0),
    updatedAt: at(1),
    history: [
      {
        fromState: PixState.UNSPECIFIED,
        toState: PixState.ACCEPTED,
        reason: PixReason.UNSPECIFIED,
        at: at(0),
      },
    ],
  };
}

export const okLimits: TransfersImpl["getLimits"] = () => ({
  accountId: "acc-1",
  dailyLimitCents: 200_000n,
  usedTodayCents: 12_345n,
  availableTodayCents: 187_655n,
  resetsAt: timestampFromDate(new Date("2026-11-13T03:00:00.000Z")),
});

export { Code, ConnectError, ErrorReason, PixReason, PixState, PixStatus };
