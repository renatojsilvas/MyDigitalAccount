import { ROOT_CONTEXT, trace, type Context } from "@opentelemetry/api";
import { SeverityNumber, type Logger as OtelLogger } from "@opentelemetry/api-logs";
import { pino, type DestinationStream, type Level, type Logger } from "pino";
import type { Config } from "../config.js";

// Dados sensíveis nunca saem em log (CPF, chave Pix, token): mascarados na origem.
const REDACT_PATHS = ["cpf", "token", "password", "authorization", "pix_key", "*.cpf", "*.token"];

const SEVERITY: Record<number, [SeverityNumber, string]> = {
  10: [SeverityNumber.TRACE, "TRACE"],
  20: [SeverityNumber.DEBUG, "DEBUG"],
  30: [SeverityNumber.INFO, "INFO"],
  40: [SeverityNumber.WARN, "WARN"],
  50: [SeverityNumber.ERROR, "ERROR"],
  60: [SeverityNumber.FATAL, "FATAL"],
};

const INFO: [SeverityNumber, string] = [SeverityNumber.INFO, "INFO"];

const DROPPED = new Set(["level", "time", "msg", "trace_id", "span_id", "pid", "hostname"]);

function toAttributeValue(v: unknown): string | number | boolean {
  return typeof v === "string" || typeof v === "number" || typeof v === "boolean"
    ? v
    : JSON.stringify(v);
}

/**
 * Stream do pino que reenvia cada linha de log ao OpenTelemetry (OTLP -> Loki).
 * O trace_id e o span_id viram o contexto do registro, para o Grafana saltar do log ao trace.
 * O stdout continua recebendo o JSON como caminho de reserva (ADR-0002).
 */
export function createOtelLogStream(otelLogger: OtelLogger): { write(line: string): void } {
  return {
    write(line: string): void {
      let rec: Record<string, unknown>;
      try {
        rec = JSON.parse(line) as Record<string, unknown>;
      } catch {
        return; // linha que não é JSON do pino: ignora
      }
      const [severityNumber, severityText] = SEVERITY[Number(rec.level)] ?? INFO;

      let context: Context | undefined;
      if (typeof rec.trace_id === "string" && typeof rec.span_id === "string") {
        context = trace.setSpanContext(ROOT_CONTEXT, {
          traceId: rec.trace_id,
          spanId: rec.span_id,
          traceFlags: 1,
        });
      }
      const attributes = Object.fromEntries(
        Object.entries(rec)
          .filter(([k]) => !DROPPED.has(k))
          .map(([k, v]) => [k, toAttributeValue(v)]),
      );
      otelLogger.emit({
        severityNumber,
        severityText,
        body: typeof rec.msg === "string" ? rec.msg : "",
        attributes,
        ...(typeof rec.time === "string" ? { timestamp: new Date(rec.time) } : {}),
        ...(context ? { context } : {}),
      });
    },
  };
}

export interface LoggerDeps {
  readonly config: Pick<Config, "logLevel">;
  readonly serviceName: string;
  readonly stdout: DestinationStream;
  /** Quando presente, os logs também vão ao OpenTelemetry. */
  readonly otelLogger?: OtelLogger;
}

export function createLogger(deps: LoggerDeps): Logger {
  const streams: { level?: Level; stream: DestinationStream }[] = [{ stream: deps.stdout }];
  if (deps.otelLogger) streams.push({ stream: createOtelLogStream(deps.otelLogger) });

  return pino(
    {
      level: deps.config.logLevel,
      base: { service: deps.serviceName },
      timestamp: pino.stdTimeFunctions.isoTime,
      redact: { paths: REDACT_PATHS, censor: "[REDACTED]" },
    },
    pino.multistream(streams),
  );
}
