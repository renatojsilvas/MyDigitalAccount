import {
  Code,
  ConnectError,
  createRouterTransport,
  type HandlerContext,
  type Interceptor,
} from "@connectrpc/connect";
import {
  PingService,
  type PingRequest,
  type PingResponse,
} from "@conta/contracts/conta/ping/v1/ping_pb";
import {
  AggregationTemporality,
  InMemoryMetricExporter,
  PeriodicExportingMetricReader,
  type MetricData,
} from "@opentelemetry/sdk-metrics";
import { InMemoryLogRecordExporter } from "@opentelemetry/sdk-logs";
import { InMemorySpanExporter, type ReadableSpan } from "@opentelemetry/sdk-trace";
import type { FastifyInstance } from "fastify";
import { buildApp, type BuiltApp } from "../src/app.js";
import { loadConfig, type Config } from "../src/config.js";

export type LedgerBehavior = (
  req: PingRequest,
  ctx: HandlerContext,
) => PingResponse | Promise<PingResponse>;

const echo: LedgerBehavior = (req) => ({ message: req.message, service: "ledger" }) as PingResponse;

export interface Harness {
  readonly built: BuiltApp;
  readonly app: FastifyInstance;
  readonly spans: InMemorySpanExporter;
  readonly logs: InMemoryLogRecordExporter;
  /** Headers de cada chamada que chegou ao ledger falso. */
  readonly ledgerRequests: Headers[];
  stdout(): Record<string, unknown>[];
  spanNamed(name: string): ReadableSpan;
  metric(name: string): Promise<MetricData | undefined>;
  close(): Promise<void>;
}

export function testConfig(env: Record<string, string> = {}): Config {
  return loadConfig({ ENVIRONMENT: "test", LOG_LEVEL: "debug", LEDGER_TIMEOUT_MS: "150", ...env });
}

export function createHarness(
  behavior: LedgerBehavior = echo,
  config: Config = testConfig(),
  /** `realTransport`: usa o transporte gRPC de verdade (config.ledgerUrl) em vez do ledger em memória. */
  options: { realTransport?: boolean } = {},
): Harness {
  const spans = new InMemorySpanExporter();
  const logs = new InMemoryLogRecordExporter();
  const metricExporter = new InMemoryMetricExporter(AggregationTemporality.CUMULATIVE);
  const reader = new PeriodicExportingMetricReader({
    exporter: metricExporter,
    exportIntervalMillis: 3_600_000, // só exporta quando o teste pede (forceFlush)
  });
  const lines: string[] = [];
  const ledgerRequests: Harness["ledgerRequests"] = [];

  const built = buildApp(config, {
    telemetry: { spanExporter: spans, metricReader: reader, logExporter: logs },
    stdout: { write: (line: string) => void lines.push(line) },
    ...(options.realTransport
      ? {}
      : {
          ledgerTransport: (interceptors: Interceptor[]) =>
            createRouterTransport(
              ({ service }) => {
                service(PingService, {
                  ping: (req, ctx) => {
                    ledgerRequests.push(new Headers(ctx.requestHeader));
                    return behavior(req, ctx);
                  },
                });
              },
              { transport: { interceptors } },
            ),
        }),
  });
  return {
    built,
    app: built.app,
    spans,
    logs,
    ledgerRequests,
    stdout: () => lines.map((l) => JSON.parse(l) as Record<string, unknown>),
    spanNamed(name) {
      const found = spans.getFinishedSpans().find((s) => s.name === name);
      if (!found) {
        throw new Error(
          `span "${name}" não encontrado; há: ${spans
            .getFinishedSpans()
            .map((s) => s.name)
            .join(", ")}`,
        );
      }
      return found;
    },
    async metric(name) {
      await built.telemetry.meterProvider.forceFlush();
      const last = metricExporter.getMetrics().at(-1);
      return last?.scopeMetrics.flatMap((s) => s.metrics).find((m) => m.descriptor.name === name);
    },
    close: () => built.app.close(),
  };
}

export { Code, ConnectError };
