import {
  ROOT_CONTEXT,
  SpanKind,
  SpanStatusCode,
  defaultTextMapGetter,
  isSpanContextValid,
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
 * Observabilidade do servidor HTTP: span de servidor raiz (o `traceparent` de entrada vira só um link),
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

    app.addHook("onRequest", (request, reply, done) => {
      const path = request.url.split("?")[0] ?? request.url;
      if (opts.ignorePaths.has(path)) {
        done();
        return;
      }
      // Rota não encontrada vira "unmatched": nunca o caminho cru, que teria cardinalidade ilimitada.
      const route = request.routeOptions.url ?? "unmatched";
      // O BFF é a borda pública: o `traceparent` de entrada NÃO é confiável (o cliente poderia
      // escolher o trace_id e a amostragem). O trace começa aqui, como raiz; o contexto recebido
      // fica só como span link, para correlação (ADR-0006).
      const incoming = trace.getSpanContext(
        opts.propagator.extract(ROOT_CONTEXT, request.headers, defaultTextMapGetter),
      );
      const links =
        incoming !== undefined && isSpanContextValid(incoming) ? [{ context: incoming }] : [];
      const span = opts.tracer.startSpan(
        `${request.method} ${route}`,
        {
          root: true,
          kind: SpanKind.SERVER,
          links,
          attributes: {
            "http.request.method": request.method,
            "http.route": route,
            "url.path": path,
          },
        },
        ROOT_CONTEXT,
      );
      request.telemetry = {
        span,
        context: trace.setSpan(ROOT_CONTEXT, span),
        startedAt: performance.now(),
      };
      const { traceId, spanId } = span.spanContext();
      // Permite achar o trace de uma requisição sem o cliente ter que enviar um traceparent.
      void reply.header("x-trace-id", traceId);
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
