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
export function createClientTelemetryInterceptor(deps: ClientTelemetryDeps): Interceptor {
  const duration = deps.meter.createHistogram("rpc.client.duration", {
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

    const attributes = {
      "rpc.system": "grpc",
      "rpc.service": req.service.typeName,
      "rpc.method": req.method.name,
      "peer.service": deps.dependency,
    };
    const span = deps.tracer.startSpan(
      `${req.service.typeName}/${req.method.name}`,
      { kind: SpanKind.CLIENT, attributes },
      parent,
    );
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
      span.setAttribute("rpc.grpc.status_code", statusCode);
      span.end();
      duration.record((performance.now() - start) / 1000, {
        dependency: deps.dependency,
        "rpc.service": req.service.typeName,
        "rpc.method": req.method.name,
        "rpc.grpc.status_code": statusCode,
      });
    }
  };
}
