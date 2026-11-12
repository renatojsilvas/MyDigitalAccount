import {
  Code,
  ConnectError,
  createRouterTransport,
  type HandlerContext,
  type Interceptor,
} from "@connectrpc/connect";
import { LedgerService } from "@conta/contracts/conta/ledger/v1/ledger_pb";
import { createHmac } from "node:crypto";
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
import type { LedgerImpl } from "./fake-ledger.js";

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
  /** Token válido (assinado pelo próprio BFF) para a conta informada. */
  token(accountId: string): Promise<string>;
  stdout(): Record<string, unknown>[];
  spanNamed(name: string): ReadableSpan;
  metric(name: string): Promise<MetricData | undefined>;
  close(): Promise<void>;
}

export const TEST_JWT_SECRET = "segredo-de-teste-com-32-caracteres-no-minimo";

export function testConfig(env: Record<string, string> = {}): Config {
  return loadConfig({
    ENVIRONMENT: "test",
    LOG_LEVEL: "debug",
    // Prazos generosos: o caminho de sucesso nunca deve depender da velocidade da máquina. Os
    // testes de timeout passam um prazo curto e um handler que nunca responde (`hang`).
    LEDGER_TIMEOUT_MS: "10000",
    LEDGER_BALANCE_TIMEOUT_MS: "10000",
    LEDGER_STATEMENT_TIMEOUT_MS: "10000",
    BFF_JWT_SECRET: TEST_JWT_SECRET,
    ...env,
  });
}

/**
 * Token HS256 montado à mão, para os testes poderem forjar o que o plugin nunca emitiria:
 * expirado, outro emissor, assinatura errada ou `alg: none`.
 */
export function forgeToken(
  claims: Record<string, unknown>,
  options: { secret?: string; alg?: string } = {},
): string {
  const b64 = (v: unknown): string => Buffer.from(JSON.stringify(v)).toString("base64url");
  const alg = options.alg ?? "HS256";
  const signingInput = `${b64({ alg, typ: "JWT" })}.${b64(claims)}`;
  const signature =
    alg === "none"
      ? ""
      : createHmac("sha256", options.secret ?? TEST_JWT_SECRET)
          .update(signingInput)
          .digest("base64url");
  return `${signingInput}.${signature}`;
}

/** Claims válidos para o BFF dos testes (emissor, destinatário, validade). */
export function validClaims(sub: string, overrides: Record<string, unknown> = {}) {
  const now = Math.floor(Date.now() / 1000);
  return { sub, iss: "conta-bff", aud: "conta-web", iat: now, exp: now + 600, ...overrides };
}

export function createHarness(
  behavior: LedgerBehavior = echo,
  config: Config = testConfig(),
  /** `realTransport`: usa o transporte gRPC de verdade (config.ledgerUrl) em vez do ledger em memória. */
  options: { realTransport?: boolean; ledger?: LedgerImpl } = {},
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
    instanceId: "bff-test-1",
    telemetry: { spanExporter: spans, metricReader: reader, logExporter: logs },
    stdout: { write: (line: string) => void lines.push(line) },
    ...(options.realTransport
      ? {}
      : {
          ledgerTransport: (interceptors: Interceptor[]) =>
            createRouterTransport(
              ({ service }) => {
                service(LedgerService, options.ledger ?? {});
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
    token: async (accountId) => {
      await built.app.ready(); // o plugin de JWT só existe depois que o app carrega
      return built.app.jwt.sign({ sub: accountId });
    },
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
