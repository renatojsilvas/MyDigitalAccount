import { W3CTraceContextPropagator } from "@opentelemetry/core";
import { OTLPLogExporter } from "@opentelemetry/exporter-logs-otlp-grpc";
import { OTLPMetricExporter } from "@opentelemetry/exporter-metrics-otlp-grpc";
import { OTLPTraceExporter } from "@opentelemetry/exporter-trace-otlp-grpc";
import { resourceFromAttributes } from "@opentelemetry/resources";
import {
  BatchLogRecordProcessor,
  LoggerProvider,
  SimpleLogRecordProcessor,
  type LogRecordExporter,
} from "@opentelemetry/sdk-logs";
import {
  MeterProvider,
  PeriodicExportingMetricReader,
  type IMetricReader,
} from "@opentelemetry/sdk-metrics";
import {
  BatchSpanProcessor,
  SimpleSpanProcessor,
  TracerProvider,
  type SpanExporter,
} from "@opentelemetry/sdk-trace";
import type { TextMapPropagator } from "@opentelemetry/api";

export interface TelemetryConfig {
  readonly serviceName: string;
  readonly version: string;
  readonly environment: string;
  /** Distingue réplicas do mesmo serviço (service.instance.id); em contêiner, o hostname é único. */
  readonly instanceId: string;
  /** Ausente: os providers existem, mas nada é exportado. */
  readonly otlpEndpoint: string | undefined;
}

/** Substitutos usados pelos testes (exportadores em memória). */
export interface TelemetryOverrides {
  readonly spanExporter?: SpanExporter;
  readonly metricReader?: IMetricReader;
  readonly logExporter?: LogRecordExporter;
}

/**
 * Os providers são criados aqui e passados por construtor. Nada é registrado como global
 * do OpenTelemetry: o contexto de trace também trafega de forma explícita (ver
 * `otelContext` nas requisições), o que mantém o composition root como único dono deles.
 */
export interface Telemetry {
  readonly tracerProvider: TracerProvider;
  readonly meterProvider: MeterProvider;
  readonly loggerProvider: LoggerProvider;
  readonly propagator: TextMapPropagator;
  shutdown(): Promise<void>;
}

const METRIC_EXPORT_INTERVAL_MS = 10_000;

export function createTelemetry(
  cfg: TelemetryConfig,
  overrides: TelemetryOverrides = {},
): Telemetry {
  const resource = resourceFromAttributes({
    "service.name": cfg.serviceName,
    "service.version": cfg.version,
    "deployment.environment.name": cfg.environment,
    "service.instance.id": cfg.instanceId,
  });
  const url = cfg.otlpEndpoint;

  // Em produção, spans e logs seguem em lote; nos testes, o processador síncrono evita esperar.
  const spanProcessors = overrides.spanExporter
    ? [new SimpleSpanProcessor({ exporter: overrides.spanExporter })]
    : url !== undefined
      ? [new BatchSpanProcessor({ exporter: new OTLPTraceExporter({ url }) })]
      : [];
  const readers: IMetricReader[] = overrides.metricReader
    ? [overrides.metricReader]
    : url !== undefined
      ? [
          new PeriodicExportingMetricReader({
            exporter: new OTLPMetricExporter({ url }),
            exportIntervalMillis: METRIC_EXPORT_INTERVAL_MS,
          }),
        ]
      : [];
  const logProcessors = overrides.logExporter
    ? [new SimpleLogRecordProcessor({ exporter: overrides.logExporter })]
    : url !== undefined
      ? [new BatchLogRecordProcessor({ exporter: new OTLPLogExporter({ url }) })]
      : [];

  const tracerProvider = new TracerProvider({ resource, spanProcessors });
  const meterProvider = new MeterProvider({ resource, readers });
  const loggerProvider = new LoggerProvider({ resource, processors: logProcessors });

  return {
    tracerProvider,
    meterProvider,
    loggerProvider,
    propagator: new W3CTraceContextPropagator(),
    async shutdown() {
      // allSettled: uma falha ao descarregar um sinal não pode impedir os outros.
      await Promise.allSettled([
        tracerProvider.shutdown(),
        meterProvider.shutdown(),
        loggerProvider.shutdown(),
      ]);
    },
  };
}
