import { describe, expect, it } from "vitest";
import { ConfigError, loadConfig } from "./config.js";

const SECRET = "segredo-de-teste-com-32-caracteres-no-minimo";
const base = { BFF_JWT_SECRET: SECRET };

describe("loadConfig", () => {
  it("aplica os padrões", () => {
    expect(loadConfig(base)).toEqual({
      environment: "development",
      logLevel: "info",
      serviceVersion: "dev",
      host: "0.0.0.0",
      port: 8088,
      ledgerUrl: "http://ledger:50051",
      ledgerTimeoutMs: 1000,
      ledgerBalanceTimeoutMs: 250,
      ledgerStatementTimeoutMs: 200,
      jwtSecret: SECRET,
      jwtTtlSeconds: 900,
      demoLogin: true,
      otlpEndpoint: undefined,
      shutdownTimeoutMs: 10_000,
    });
  });

  it("lê valores explícitos e converte números", () => {
    const cfg = loadConfig({
      ...base,
      ENVIRONMENT: "production",
      LOG_LEVEL: "debug",
      BFF_PORT: "9000",
      LEDGER_GRPC_URL: "http://localhost:50051",
      LEDGER_TIMEOUT_MS: "250",
      LEDGER_BALANCE_TIMEOUT_MS: "300",
      LEDGER_STATEMENT_TIMEOUT_MS: "120",
      OTEL_EXPORTER_OTLP_ENDPOINT: "http://otel:4317",
    });
    expect(cfg).toMatchObject({
      environment: "production",
      logLevel: "debug",
      port: 9000,
      ledgerUrl: "http://localhost:50051",
      ledgerTimeoutMs: 250,
      ledgerBalanceTimeoutMs: 300,
      ledgerStatementTimeoutMs: 120,
      otlpEndpoint: "http://otel:4317",
    });
  });

  it("trata variável vazia como ausente (é o que o compose entrega)", () => {
    expect(loadConfig({ ...base, BFF_PORT: "", OTEL_EXPORTER_OTLP_ENDPOINT: "" })).toMatchObject({
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
    ["LEDGER_BALANCE_TIMEOUT_MS", { LEDGER_BALANCE_TIMEOUT_MS: "0" }],
    ["LEDGER_STATEMENT_TIMEOUT_MS", { LEDGER_STATEMENT_TIMEOUT_MS: "-5" }],
    ["SHUTDOWN_TIMEOUT_MS", { SHUTDOWN_TIMEOUT_MS: "abc" }],
    ["BFF_JWT_SECRET", { BFF_JWT_SECRET: "curto" }],
    ["BFF_JWT_TTL_SECONDS", { BFF_JWT_TTL_SECONDS: "0" }],
  ])("rejeita %s inválida", (name, env) => {
    expect(() => loadConfig({ ...base, ...env })).toThrow(name);
  });

  it("exige o segredo do JWT, sem valor padrão", () => {
    expect(() => loadConfig({})).toThrow("BFF_JWT_SECRET");
  });

  describe("login de demonstração", () => {
    it("fica ligado em development e test, desligado em production", () => {
      expect(loadConfig({ ...base, ENVIRONMENT: "development" }).demoLogin).toBe(true);
      expect(loadConfig({ ...base, ENVIRONMENT: "test" }).demoLogin).toBe(true);
      expect(loadConfig({ ...base, ENVIRONMENT: "production" }).demoLogin).toBe(false);
    });

    it("pode ser desligado fora de production", () => {
      expect(loadConfig({ ...base, BFF_DEMO_LOGIN: "false" }).demoLogin).toBe(false);
    });

    it("o boot falha se ele for ligado em production", () => {
      expect(() =>
        loadConfig({ ...base, ENVIRONMENT: "production", BFF_DEMO_LOGIN: "true" }),
      ).toThrow("BFF_DEMO_LOGIN");
    });
  });

  it("reporta todos os problemas de uma vez", () => {
    try {
      loadConfig({ ...base, ENVIRONMENT: "x", LOG_LEVEL: "y", BFF_PORT: "z" });
      expect.unreachable("deveria ter falhado");
    } catch (err) {
      expect(err).toBeInstanceOf(ConfigError);
      expect((err as ConfigError).issues).toHaveLength(3);
    }
  });
});
