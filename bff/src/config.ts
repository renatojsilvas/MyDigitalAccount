import { z } from "zod";

const LOG_LEVELS = ["fatal", "error", "warn", "info", "debug", "trace", "silent"] as const;

const envSchema = z.object({
  ENVIRONMENT: z.enum(["development", "test", "production"]).default("development"),
  LOG_LEVEL: z.enum(LOG_LEVELS).default("info"),
  SERVICE_VERSION: z.string().min(1).default("dev"),
  BFF_HOST: z.string().min(1).default("0.0.0.0"),
  // Porta interna do contêiner; a porta publicada no host é BFF_HOST_PORT, só no compose.
  BFF_PORT: z.coerce.number().int().min(0).max(65535).default(8088),
  LEDGER_GRPC_URL: z.url().default("http://ledger:50051"),
  LEDGER_TIMEOUT_MS: z.coerce.number().int().positive().default(1000),
  OTEL_EXPORTER_OTLP_ENDPOINT: z.url().optional(),
  SHUTDOWN_TIMEOUT_MS: z.coerce.number().int().positive().default(10_000),
});

export interface Config {
  readonly environment: "development" | "test" | "production";
  readonly logLevel: (typeof LOG_LEVELS)[number];
  readonly serviceVersion: string;
  readonly host: string;
  readonly port: number;
  readonly ledgerUrl: string;
  readonly ledgerTimeoutMs: number;
  /** Ausente: nada é exportado por OTLP; os logs seguem no stdout. */
  readonly otlpEndpoint: string | undefined;
  readonly shutdownTimeoutMs: number;
}

export class ConfigError extends Error {
  constructor(public readonly issues: readonly string[]) {
    super(`configuração inválida:\n${issues.map((i) => `  - ${i}`).join("\n")}`);
    this.name = "ConfigError";
  }
}

/**
 * Valida o ambiente no boot e falha com todos os problemas de uma vez.
 * Recebe o ambiente por parâmetro (sem ler process.env aqui) para os testes não
 * dependerem de estado global. Variável vazia conta como ausente: é o que o compose
 * entrega quando uma `${VAR}` não está definida.
 */
export function loadConfig(env: Readonly<Record<string, string | undefined>>): Config {
  const present = Object.fromEntries(
    Object.entries(env).filter(([, v]) => v !== undefined && v !== ""),
  );
  const parsed = envSchema.safeParse(present);
  if (!parsed.success) {
    throw new ConfigError(parsed.error.issues.map((i) => `${i.path.join(".")}: ${i.message}`));
  }
  const e = parsed.data;
  return {
    environment: e.ENVIRONMENT,
    logLevel: e.LOG_LEVEL,
    serviceVersion: e.SERVICE_VERSION,
    host: e.BFF_HOST,
    port: e.BFF_PORT,
    ledgerUrl: e.LEDGER_GRPC_URL,
    ledgerTimeoutMs: e.LEDGER_TIMEOUT_MS,
    otlpEndpoint: e.OTEL_EXPORTER_OTLP_ENDPOINT,
    shutdownTimeoutMs: e.SHUTDOWN_TIMEOUT_MS,
  };
}
