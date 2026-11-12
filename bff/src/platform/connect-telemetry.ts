import {
  ROOT_CONTEXT,
  SpanKind,
  SpanStatusCode,
  trace,
  type Context,
  type Meter,
  type TextMapPropagator,
  type TextMapSetter,
  type Tracer,
} from "@opentelemetry/api";
import { Code, ConnectError, createContextKey, type Interceptor } from "@connectrpc/connect";

/**
 * Chave para a chamada carregar o contexto de trace do request HTTP até o interceptor
 * (`client.method(req, { contextValues })`). É o que liga o span de cliente ao span do
 * servidor HTTP sem depender de contexto global assíncrono.
 */
export const otelContextKey = createContextKey<Context>(ROOT_CONTEXT, {
  description: "contexto de trace do OpenTelemetry",
});

const headersSetter: TextMapSetter<Headers> = {
  set: (carrier, key, value) => {
    carrier.set(key, value);
  },
};

export interface ClientTelemetryDeps {
  readonly tracer: Tracer;
  readonly meter: Meter;
  readonly propagator: TextMapPropagator;
  /** Nome lógico da dependência (ex.: "ledger"). Conjunto pequeno e fixo: vira label de métrica. */
  readonly dependency: string;
}

/**
 * Interceptor do cliente Connect. A instrumentação HTTP do OpenTelemetry não cobre HTTP/2,
 * que é o que o gRPC usa, então este interceptor faz o que ela faria:
 *  1. cria o span de cliente (atributos rpc.* e status gRPC);
 *  2. injeta o `traceparent` nos headers da chamada;
 *  3. registra a latência e o erro por dependência.
 * Só chamadas unárias são instrumentadas; streaming apenas propaga o traceparent.
 */
/**
 * Nome canônico de cada código gRPC, como o `otelgrpc` do ledger emite em
 * `rpc.response.status_code` (semconv de RPC v1.43.0). Os códigos do Connect são
 * numericamente iguais aos do gRPC.
 */
const GRPC_STATUS_NAMES: readonly string[] = [
  "OK",
  "CANCELLED",
  "UNKNOWN",
  "INVALID_ARGUMENT",
  "DEADLINE_EXCEEDED",
  "NOT_FOUND",
  "ALREADY_EXISTS",
  "PERMISSION_DENIED",
  "RESOURCE_EXHAUSTED",
  "FAILED_PRECONDITION",
  "ABORTED",
  "OUT_OF_RANGE",
  "UNIMPLEMENTED",
  "INTERNAL",
  "UNAVAILABLE",
  "DATA_LOSS",
  "UNAUTHENTICATED",
];

export function grpcStatusName(code: number): string {
  return GRPC_STATUS_NAMES[code] ?? "UNKNOWN";
}

export function createClientTelemetryInterceptor(deps: ClientTelemetryDeps): Interceptor {
  // Nomes da semconv de RPC v1.43.0, os mesmos que o otelgrpc usa no ledger (ADR-0004,
  // seção 6): a convenção ainda evolui; confira a cada atualização do SDK.
  const duration = deps.meter.createHistogram("rpc.client.call.duration", {
    unit: "s",
    description: "Duração das chamadas gRPC do BFF a uma dependência",
    advice: { explicitBucketBoundaries: [0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10] },
  });

  return (next) => async (req) => {
    const parent = req.contextValues.get(otelContextKey);

    if (req.stream) {
      deps.propagator.inject(parent, req.header, headersSetter);
      return next(req);
    }

    // rpc.method é o nome completo (pacote.Serviço/Método); não existe mais rpc.service.
    const method = `${req.service.typeName}/${req.method.name}`;
    const attributes = {
      "rpc.system.name": "grpc",
      "rpc.method": method,
      "peer.service": deps.dependency,
    };
    const span = deps.tracer.startSpan(method, { kind: SpanKind.CLIENT, attributes }, parent);
    const ctx = trace.setSpan(parent, span);
    deps.propagator.inject(ctx, req.header, headersSetter);

    const start = performance.now();
    // Os códigos do Connect são numericamente iguais aos do gRPC; o sucesso (OK) é 0.
    let statusCode = 0;
    try {
      return await next(req);
    } catch (err) {
      const connectErr = ConnectError.from(err);
      statusCode = connectErr.code;
      span.recordException(connectErr);
      span.setStatus({ code: SpanStatusCode.ERROR, message: Code[connectErr.code] });
      throw err;
    } finally {
      const statusName = grpcStatusName(statusCode);
      span.setAttribute("rpc.response.status_code", statusName);
      span.end();
      duration.record((performance.now() - start) / 1000, {
        dependency: deps.dependency,
        "rpc.system.name": "grpc",
        "rpc.method": method,
        "rpc.response.status_code": statusName,
      });
    }
  };
}
