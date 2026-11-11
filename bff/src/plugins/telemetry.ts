import {
  ROOT_CONTEXT,
  SpanKind,
  SpanStatusCode,
  defaultTextMapGetter,
  trace,
  type Context,
  type Meter,
  type Span,
  type TextMapPropagator,
  type Tracer,
} from "@opentelemetry/api";
import fp from "fastify-plugin";

/** Telemetria de uma requisição em andamento. */
export interface RequestTelemetry {
  readonly span: Span;
  /** Contexto de trace desta requisição; é repassado às chamadas a dependências. */
  readonly context: Context;
  readonly startedAt: number;
}

declare module "fastify" {
  interface FastifyRequest {
    /** `null` nas rotas fora do tracing de negócio (health). */
    telemetry: RequestTelemetry | null;
  }
}

export interface TelemetryPluginOptions {
  readonly tracer: Tracer;
  readonly meter: Meter;
  readonly propagator: TextMapPropagator;
  /** Caminhos fora do tracing e das métricas de negócio (sondas de saúde). */
  readonly ignorePaths: ReadonlySet<string>;
}

/**
 * Observabilidade do servidor HTTP: span de servidor (com extração do `traceparent`),
 * métricas RED (taxa, erros e duração por rota), logger da requisição com trace_id e
 * span_id, e um único log estruturado por requisição concluída.
 *
 * Feito com hooks do Fastify, sem auto-instrumentação: nada é monkey-patched e o contexto
 * de trace trafega de forma explícita pela requisição.
 */
export const telemetryPlugin = fp<TelemetryPluginOptions>(
  (app, opts, done) => {
    const duration = opts.meter.createHistogram("http.server.request.duration", {
      unit: "s",
      description: "Duração das requisições HTTP atendidas pelo BFF",
      advice: {
        explicitBucketBoundaries: [0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10],
      },
    });

    app.decorateRequest("telemetry", null);

    app.addHook("onRequest", (request, _reply, done) => {
      const path = request.url.split("?")[0] ?? request.url;
      if (opts.ignorePaths.has(path)) {
        done();
        return;
      }
      // Rota não encontrada vira "unmatched": nunca o caminho cru, que teria cardinalidade ilimitada.
      const route = request.routeOptions.url ?? "unmatched";
      const parent = opts.propagator.extract(ROOT_CONTEXT, request.headers, defaultTextMapGetter);
      const span = opts.tracer.startSpan(
        `${request.method} ${route}`,
        {
          kind: SpanKind.SERVER,
          attributes: {
            "http.request.method": request.method,
            "http.route": route,
            "url.path": path,
          },
        },
        parent,
      );
      request.telemetry = {
        span,
        context: trace.setSpan(parent, span),
        startedAt: performance.now(),
      };
      const { traceId, spanId } = span.spanContext();
      request.log = request.log.child({ trace_id: traceId, span_id: spanId });
      done();
    });

    app.addHook("onError", (request, _reply, error, done) => {
      request.telemetry?.span.recordException(error);
      done();
    });

    app.addHook("onResponse", (request, reply, done) => {
      const t = request.telemetry;
      if (t === null) {
        done();
        return;
      }
      const status = reply.statusCode;
      const route = request.routeOptions.url ?? "unmatched";
      const seconds = (performance.now() - t.startedAt) / 1000;

      t.span.setAttribute("http.response.status_code", status);
      if (status >= 500) t.span.setStatus({ code: SpanStatusCode.ERROR });
      t.span.end();

      duration.record(seconds, {
        "http.request.method": request.method,
        "http.route": route,
        "http.response.status_code": status,
      });

      request.log[status >= 500 ? "error" : "info"](
        {
          method: request.method,
          route,
          status,
          duration_ms: Math.round(seconds * 1000 * 100) / 100,
        },
        "request completed",
      );
      done();
    });

    done();
  },
  { name: "telemetry" },
);
