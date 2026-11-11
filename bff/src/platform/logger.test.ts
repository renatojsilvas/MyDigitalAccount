import {
  InMemoryLogRecordExporter,
  LoggerProvider,
  SimpleLogRecordProcessor,
} from "@opentelemetry/sdk-logs";
import { describe, expect, it } from "vitest";
import { createLogger } from "./logger.js";

function setup() {
  const lines: string[] = [];
  const exporter = new InMemoryLogRecordExporter();
  const provider = new LoggerProvider({
    processors: [new SimpleLogRecordProcessor({ exporter })],
  });
  const logger = createLogger({
    config: { logLevel: "debug" },
    serviceName: "bff",
    stdout: { write: (l: string) => void lines.push(l) },
    otelLogger: provider.getLogger("test"),
  });
  return { logger, lines, exporter };
}

describe("createLogger", () => {
  it("mascara dados sensíveis na origem, no stdout e no export OTLP", () => {
    const { logger, lines, exporter } = setup();
    logger.info({ cpf: "123.456.789-09", token: "abc", pix_key: "a@b.com", conta: "ok" }, "teste");

    const out = JSON.parse(lines[0] ?? "{}") as Record<string, unknown>;
    expect(out).toMatchObject({
      cpf: "[REDACTED]",
      token: "[REDACTED]",
      pix_key: "[REDACTED]",
      conta: "ok",
    });
    expect(lines[0]).not.toContain("123.456.789-09");

    const exported = exporter.getFinishedLogRecords()[0];
    expect(JSON.stringify(exported?.attributes)).not.toContain("123.456.789-09");
    expect(exported?.attributes.conta).toBe("ok");
  });

  it("exporta severidade, corpo e o contexto de trace do registro", () => {
    const { logger, exporter } = setup();
    const traceId = "0102030405060708090a0b0c0d0e0f10";
    const spanId = "0102030405060708";
    logger.child({ trace_id: traceId, span_id: spanId }).warn({ route: "/x" }, "atenção");

    const rec = exporter.getFinishedLogRecords()[0];
    expect(rec?.severityText).toBe("WARN");
    expect(rec?.body).toBe("atenção");
    expect(rec?.spanContext?.traceId).toBe(traceId);
    expect(rec?.spanContext?.spanId).toBe(spanId);
    expect(rec?.attributes.route).toBe("/x");
  });

  it("respeita o nível configurado", () => {
    const lines: string[] = [];
    const logger = createLogger({
      config: { logLevel: "warn" },
      serviceName: "bff",
      stdout: { write: (l: string) => void lines.push(l) },
    });
    logger.info("ignorado");
    expect(lines).toHaveLength(0);
  });
});
