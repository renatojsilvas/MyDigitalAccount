import { createGrpcTransport } from "@connectrpc/connect-node";
import type { Interceptor, Transport } from "@connectrpc/connect";
import Fastify, { LogController, type FastifyBaseLogger, type FastifyInstance } from "fastify";
import {
  serializerCompiler,
  validatorCompiler,
  type ZodTypeProvider,
} from "fastify-type-provider-zod";
import { hostname } from "node:os";
import type { DestinationStream } from "pino";
import { createLedgerClient } from "./clients/ledger.js";
import type { Config } from "./config.js";
import { createLifecycle, type Lifecycle } from "./lifecycle.js";
import { pingRoutes } from "./modules/ping/route.js";
import { createPingService } from "./modules/ping/service.js";
import { createClientTelemetryInterceptor } from "./platform/connect-telemetry.js";
import { createLogger } from "./platform/logger.js";
import { registerRuntimeMetrics } from "./platform/runtime-metrics.js";
import { createTelemetry, type Telemetry, type TelemetryOverrides } from "./platform/telemetry.js";
import { errorsPlugin } from "./plugins/errors.js";
import { healthPlugin } from "./plugins/health.js";
import { telemetryPlugin } from "./plugins/telemetry.js";

export const SERVICE_NAME = "bff";

/** Pontos de troca usados pelos testes; em produção tudo vem do padrão. */
export interface AppOverrides {
  readonly telemetry?: TelemetryOverrides;
  /** Recebe os interceptors (telemetria) e devolve o transporte do ledger. */
  readonly ledgerTransport?: (interceptors: Interceptor[]) => Transport;
  readonly stdout?: DestinationStream;
  /** Identificador da instância; por padrão, o hostname (único por contêiner). */
  readonly instanceId?: string;
}

export interface BuiltApp {
  readonly app: FastifyInstance;
  readonly lifecycle: Lifecycle;
  readonly telemetry: Telemetry;
}

/**
 * Composition root do BFF: as dependências são criadas e ligadas aqui, por factory
 * functions, sem contêiner de injeção e sem singletons importados diretamente.
 */
export function buildApp(config: Config, overrides: AppOverrides = {}): BuiltApp {
  const telemetry = createTelemetry(
    {
      serviceName: SERVICE_NAME,
      version: config.serviceVersion,
      environment: config.environment,
      instanceId: overrides.instanceId ?? hostname(),
      otlpEndpoint: config.otlpEndpoint,
    },
    overrides.telemetry,
  );
  const tracer = telemetry.tracerProvider.getTracer("conta.bff");
  const meter = telemetry.meterProvider.getMeter("conta.bff");

  const logger = createLogger({
    config,
    serviceName: SERVICE_NAME,
    stdout: overrides.stdout ?? process.stdout,
    otelLogger: telemetry.loggerProvider.getLogger("conta.bff"),
  });
  // Tipo base do Fastify: o app é usado sem depender de que o logger concreto seja o pino.
  const baseLogger: FastifyBaseLogger = logger;
  const stopRuntimeMetrics = registerRuntimeMetrics(meter);

  const ledgerTelemetry = createClientTelemetryInterceptor({
    tracer,
    meter,
    propagator: telemetry.propagator,
    dependency: "ledger",
  });
  const makeTransport =
    overrides.ledgerTransport ??
    ((interceptors: Interceptor[]) =>
      createGrpcTransport({ baseUrl: config.ledgerUrl, interceptors }));
  const ledger = createLedgerClient(makeTransport([ledgerTelemetry]), {
    timeoutMs: config.ledgerTimeoutMs,
    balanceTimeoutMs: config.ledgerBalanceTimeoutMs,
    statementTimeoutMs: config.ledgerStatementTimeoutMs,
  });
  const lifecycle = createLifecycle();

  const app = Fastify({
    loggerInstance: baseLogger,
    // Um único log estruturado por requisição, emitido pelo plugin de telemetria já com trace_id.
    logController: new LogController({ disableRequestLogging: true }),
    forceCloseConnections: "idle",
  });
  app.setValidatorCompiler(validatorCompiler);
  app.setSerializerCompiler(serializerCompiler);

  void app.register(telemetryPlugin, {
    tracer,
    meter,
    propagator: telemetry.propagator,
    ignorePaths: new Set(["/healthz", "/readyz"]),
  });
  void app.register(errorsPlugin);
  void app.register(healthPlugin, { lifecycle });
  void app.withTypeProvider<ZodTypeProvider>().register(pingRoutes, {
    service: createPingService({ ledger }),
  });

  app.addHook("onClose", async () => {
    stopRuntimeMetrics();
    await telemetry.shutdown(); // descarrega traces, métricas e logs pendentes
  });

  return { app, lifecycle, telemetry };
}
