import { SpanStatusCode } from "@opentelemetry/api";
import { afterEach, describe, expect, it } from "vitest";
import { homeResponseSchema } from "../src/modules/home/schema.js";
import { Code, ConnectError, createHarness, testConfig, type Harness } from "./harness.js";
import { accountNotFound, Direction, hang, posting, type LedgerImpl } from "./fake-ledger.js";

let h: Harness;
afterEach(async () => {
  await h.close();
});

const okLedger: LedgerImpl = {
  getBalance: () => ({ accountId: "acc-1", balanceCents: 123_456n }),
  listStatement: () => ({
    postings: [posting(2, Direction.DEBIT), posting(1)],
    nextPageToken: "",
  }),
};

async function getHome(ledger: LedgerImpl, config = testConfig()) {
  h = createHarness(undefined, config, { ledger });
  return h.app.inject({
    url: "/home",
    headers: { authorization: `Bearer ${await h.token("acc-1")}` },
  });
}

describe("GET /home", () => {
  it("responde com saldo e extrato, e a resposta cumpre o schema", async () => {
    const res = await getHome(okLedger);
    expect(res.statusCode).toBe(200);
    const body = homeResponseSchema.parse(res.json());
    expect(body.balanceCents).toBe(123_456);
    expect(body.statement.status).toBe("ok");
    expect(body.statement.status === "ok" && body.statement.items.map((i) => i.direction)).toEqual([
      "debit",
      "credit",
    ]);
  });

  it("devolve o x-trace-id do trace da requisição", async () => {
    const res = await getHome(okLedger);
    const traceId = h.spanNamed("GET /home").spanContext().traceId;
    expect(res.headers["x-trace-id"]).toBe(traceId);
  });
});

describe("degradação parcial (critério de pronto da Fase 2)", () => {
  // Extrato que nunca responde + prazo curto: o timeout dispara sem depender da velocidade da máquina.
  const slowStatement: LedgerImpl = { ...okLedger, listStatement: (_req, ctx) => hang(ctx) };
  const shortStatement = testConfig({ LEDGER_STATEMENT_TIMEOUT_MS: "20" });
  const downStatement: LedgerImpl = {
    ...okLedger,
    listStatement: () => {
      throw new ConnectError("down", Code.Unavailable);
    },
  };

  it.each([
    ["lento", slowStatement, shortStatement, "timeout"],
    ["fora do ar", downStatement, testConfig(), "unavailable"],
  ])("extrato %s: 200 com saldo e extrato indisponível", async (_name, ledger, config, reason) => {
    const res = await getHome(ledger, config);
    expect(res.statusCode).toBe(200);
    expect(res.json()).toEqual({
      balanceCents: 123_456,
      statement: { status: "unavailable", reason },
    });
  });

  it("aparece no contador de respostas degradadas", async () => {
    await getHome(slowStatement, shortStatement);
    await getHome(downStatement);
    const metric = await h.metric("bff.home.degraded");
    expect(metric).toBeDefined();
    const points = metric?.dataPoints.map((p) => ({
      attributes: p.attributes,
      value: p.value,
    }));
    // h é o segundo harness; ele só viu a segunda chamada.
    expect(points).toEqual([
      {
        attributes: { "home.section": "statement", "home.degraded.reason": "unavailable" },
        value: 1,
      },
    ]);
  });

  it("aparece no trace: span do servidor marcado e span do cliente com erro", async () => {
    await getHome(slowStatement, shortStatement);
    const server = h.spanNamed("GET /home");
    expect(server.attributes).toMatchObject({
      "home.degraded": true,
      "http.response.status_code": 200,
    });
    expect(server.events.map((e) => e.name)).toContain("home.degraded");
    expect(server.status.code).not.toBe(SpanStatusCode.ERROR); // degradar não é erro do BFF

    const statementSpan = h.spanNamed("conta.ledger.v1.LedgerService/ListStatement");
    expect(statementSpan.status.code).toBe(SpanStatusCode.ERROR);
    expect(statementSpan.attributes["rpc.response.status_code"]).toBe("DEADLINE_EXCEEDED");
    expect(statementSpan.parentSpanContext?.spanId).toBe(server.spanContext().spanId);
    // O saldo, na mesma requisição, deu certo.
    expect(h.spanNamed("conta.ledger.v1.LedgerService/GetBalance").status.code).not.toBe(
      SpanStatusCode.ERROR,
    );
  });

  it("registra a degradação no log da requisição, sem dado pessoal", async () => {
    await getHome(slowStatement, shortStatement);
    const line = h.stdout().find((l) => l.msg === "request completed");
    expect(line).toMatchObject({ route: "/home", status: 200, degraded: ["statement"] });
  });
});

describe("falha do saldo (essencial): a home falha", () => {
  const cases: {
    name: string;
    getBalance: LedgerImpl["getBalance"];
    status: number;
    code: string;
    config?: ReturnType<typeof testConfig>;
  }[] = [
    {
      name: "lento",
      getBalance: (_req, ctx) => hang(ctx),
      config: testConfig({ LEDGER_BALANCE_TIMEOUT_MS: "20" }),
      status: 504,
      code: "dependency_timeout",
    },
    {
      name: "fora do ar",
      getBalance: () => {
        throw new ConnectError("down", Code.Unavailable);
      },
      status: 503,
      code: "dependency_unavailable",
    },
    {
      name: "erro interno",
      getBalance: () => {
        throw new ConnectError("boom: detalhe interno", Code.Internal);
      },
      status: 502,
      code: "dependency_error",
    },
    {
      // O token aponta para uma conta que não existe mais (ex.: banco recriado): o front volta ao login.
      name: "conta do token inexistente",
      getBalance: () => {
        throw accountNotFound();
      },
      status: 401,
      code: "unauthenticated",
    },
  ];

  it.each(cases)("saldo $name: $status $code", async ({ getBalance, status, code, config }) => {
    const res = await getHome({ ...okLedger, getBalance }, config);
    expect(res.statusCode).toBe(status);
    expect(res.json()).toMatchObject({ code, status });
    expect(res.json()).toHaveProperty("trace_id");
    expect(res.body).not.toContain("detalhe interno"); // erro interno não vaza
  });

  it("não conta como resposta degradada", async () => {
    await getHome({
      ...okLedger,
      getBalance: () => {
        throw new ConnectError("down", Code.Unavailable);
      },
    });
    expect(await h.metric("bff.home.degraded")).toBeUndefined();
  });
});
