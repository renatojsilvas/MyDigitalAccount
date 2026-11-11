import { describe, expect, it } from "vitest";
import { ConfigError, loadConfig } from "./config.js";

describe("loadConfig", () => {
  it("aplica os padrões", () => {
    expect(loadConfig({})).toEqual({
      environment: "development",
      logLevel: "info",
      serviceVersion: "dev",
      host: "0.0.0.0",
      port: 8088,
      ledgerUrl: "http://ledger:50051",
      ledgerTimeoutMs: 1000,
      otlpEndpoint: undefined,
      shutdownTimeoutMs: 10_000,
    });
  });

  it("lê valores explícitos e converte números", () => {
    const cfg = loadConfig({
      ENVIRONMENT: "production",
      LOG_LEVEL: "debug",
      BFF_PORT: "9000",
      LEDGER_GRPC_URL: "http://localhost:50051",
      LEDGER_TIMEOUT_MS: "250",
      OTEL_EXPORTER_OTLP_ENDPOINT: "http://otel:4317",
    });
    expect(cfg).toMatchObject({
      environment: "production",
      logLevel: "debug",
      port: 9000,
      ledgerUrl: "http://localhost:50051",
      ledgerTimeoutMs: 250,
      otlpEndpoint: "http://otel:4317",
    });
  });

  it("trata variável vazia como ausente (é o que o compose entrega)", () => {
    expect(loadConfig({ BFF_PORT: "", OTEL_EXPORTER_OTLP_ENDPOINT: "" })).toMatchObject({
      port: 8088,
      otlpEndpoint: undefined,
    });
  });

  it.each([
    ["ENVIRONMENT", { ENVIRONMENT: "staging" }],
    ["LOG_LEVEL", { LOG_LEVEL: "barulhento" }],
    ["BFF_PORT", { BFF_PORT: "70000" }],
    ["LEDGER_GRPC_URL", { LEDGER_GRPC_URL: "não é url" }],
    ["LEDGER_TIMEOUT_MS", { LEDGER_TIMEOUT_MS: "0" }],
    ["SHUTDOWN_TIMEOUT_MS", { SHUTDOWN_TIMEOUT_MS: "abc" }],
  ])("rejeita %s inválida", (name, env) => {
    expect(() => loadConfig(env)).toThrow(name);
  });

  it("reporta todos os problemas de uma vez", () => {
    try {
      loadConfig({ ENVIRONMENT: "x", LOG_LEVEL: "y", BFF_PORT: "z" });
      expect.unreachable("deveria ter falhado");
    } catch (err) {
      expect(err).toBeInstanceOf(ConfigError);
      expect((err as ConfigError).issues).toHaveLength(3);
    }
  });
});
