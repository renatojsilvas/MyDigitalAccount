import { afterEach, describe, expect, it } from "vitest";
import { pixAcceptedSchema, pixViewSchema } from "../src/modules/pix/schema.js";
import { createHarness, testConfig, type Harness } from "./harness.js";
import { hang } from "./fake-ledger.js";
import {
  Code,
  ConnectError,
  ErrorReason,
  okLimits,
  PixReason,
  PixState,
  PixStatus,
  pixMessage,
  transfersRejection,
  type TransfersImpl,
} from "./fake-transfers.js";

let h: Harness;
afterEach(async () => {
  await h.close();
});

const PIX_ID = "01a1d0a3-fccc-7c3b-a643-8f3eaf603028";
const CPF = "98765432100";
const body = {
  destinationKey: { type: "cpf", value: CPF },
  amountCents: 12_345,
  description: "aluguel",
};

interface Seen {
  req?: unknown;
  traceparent?: string | null;
}

function setup(transfers: TransfersImpl, config = testConfig()) {
  h = createHarness(undefined, config, { transfers: { getLimits: okLimits, ...transfers } });
}

async function post(headers: Record<string, string> = {}, payload: unknown = body) {
  return h.app.inject({
    method: "POST",
    url: "/pix",
    headers: {
      authorization: `Bearer ${await h.token("acc-1")}`,
      "idempotency-key": "chave-1",
      ...headers,
    },
    payload: payload as Record<string, unknown>,
  });
}

describe("POST /pix", () => {
  it("aceita: 202 com o ID e Location, repassando conta do token, chave e traceparent", async () => {
    const seen: Seen = {};
    setup({
      startPix: (req, ctx) => {
        seen.req = req;
        seen.traceparent = ctx.requestHeader.get("traceparent");
        return { pix: pixMessage() };
      },
    });
    const res = await post();
    expect(res.statusCode).toBe(202);
    expect(res.headers.location).toBe(`/pix/${PIX_ID}`);
    expect(pixAcceptedSchema.parse(res.json())).toEqual({
      pixId: PIX_ID,
      status: "processing",
      state: "accepted",
    });

    const req = seen.req as Record<string, unknown>;
    expect(req.accountId).toBe("acc-1"); // do token, nunca do cliente
    expect(req.idempotencyKey).toBe("chave-1"); // intacta
    expect(req.amountCents).toBe(12_345n);
    expect((req.destinationKey as { value: string }).value).toBe(CPF);

    // Um trace só: o traceparent que chega ao transfers é do trace da requisição do BFF.
    const traceId = h.spanNamed("POST /pix").spanContext().traceId;
    expect(seen.traceparent).toContain(traceId);
    expect(res.headers["x-trace-id"]).toBe(traceId);
  });

  it("o CPF de destino não aparece em log nem em span", async () => {
    setup({ startPix: () => ({ pix: pixMessage() }) });
    await post();
    const logs =
      JSON.stringify(h.stdout()) +
      JSON.stringify(h.logs.getFinishedLogRecords().map((l) => l.body));
    const spans = JSON.stringify(
      h.spans.getFinishedSpans().map((s) => [s.name, s.attributes, s.events]),
    );
    expect(logs).not.toContain(CPF);
    expect(spans).not.toContain(CPF);
  });

  it.each([
    ["sem Idempotency-Key", { "idempotency-key": "" }, body],
    ["chave com espaço", { "idempotency-key": "a b" }, body],
    ["CPF com 10 dígitos", {}, { ...body, destinationKey: { type: "cpf", value: "1234567890" } }],
    ["valor com centavos fracionários", {}, { ...body, amountCents: 1.5 }],
    ["valor zero", {}, { ...body, amountCents: 0 }],
    ["tipo de chave desconhecido", {}, { ...body, destinationKey: { type: "email", value: CPF } }],
  ])("%s: 400 sem chamar o transfers", async (_name, headers, payload) => {
    let called = false;
    setup({
      startPix: () => {
        called = true;
        return { pix: pixMessage() };
      },
    });
    const res = await post(headers, payload);
    expect(res.statusCode).toBe(400);
    expect(res.headers["content-type"]).toContain("application/problem+json");
    expect(called).toBe(false);
  });

  it("mesma chave com outro pedido: 409", async () => {
    setup({
      startPix: () => {
        throw transfersRejection(Code.AlreadyExists, ErrorReason.IDEMPOTENCY_KEY_REUSED);
      },
    });
    const res = await post();
    expect(res.statusCode).toBe(409);
    expect(res.json()).toMatchObject({ code: "idempotency_key_reused" });
  });

  it("recusa de formato do transfers (CPF com dígito errado): 400 com o campo, sem o valor", async () => {
    setup({
      startPix: () => {
        throw transfersRejection(Code.InvalidArgument, ErrorReason.INVALID_REQUEST, {
          field: "destination_key.value",
        });
      },
    });
    const res = await post();
    expect(res.statusCode).toBe(400);
    expect(res.json()).toMatchObject({
      code: "invalid_argument",
      detail: "campo inválido: destination_key.value",
    });
    expect(res.body).not.toContain(CPF);
  });

  it("transfers lento: 504, e o aceite pode ser repetido com a mesma chave", async () => {
    setup({ startPix: (_req, ctx) => hang(ctx) }, testConfig({ TRANSFERS_TIMEOUT_MS: "20" }));
    const res = await post();
    expect(res.statusCode).toBe(504);
  });

  it("sem token: 401", async () => {
    setup({});
    const res = await h.app.inject({ method: "POST", url: "/pix", payload: body });
    expect(res.statusCode).toBe(401);
  });
});

describe("GET /pix/{pixId}", () => {
  it("devolve o Pix com histórico, status e motivo, só adaptando a forma", async () => {
    let asked: Record<string, unknown> | undefined;
    setup({
      getPix: (req) => {
        asked = req;
        return {
          pix: pixMessage({
            state: PixState.REVERSED,
            status: PixStatus.FAILED,
            reason: PixReason.SETTLEMENT_REJECTED,
          }),
        };
      },
    });
    const res = await h.app.inject({
      url: `/pix/${PIX_ID}`,
      headers: { authorization: `Bearer ${await h.token("acc-1")}` },
    });
    expect(res.statusCode).toBe(200);
    const view = pixViewSchema.parse(res.json());
    expect(view).toMatchObject({
      pixId: PIX_ID,
      status: "failed",
      state: "reversed",
      reason: "settlement_rejected",
      amountCents: 12_345,
      destinationKeyMasked: "***.654.321-**",
      history: [{ state: "accepted", at: "2026-11-12T15:00:00.000Z" }],
    });
    expect(asked).toMatchObject({ accountId: "acc-1", pixId: PIX_ID });
  });

  it("Pix de outra conta (ou inexistente): 404", async () => {
    setup({
      getPix: () => {
        throw transfersRejection(Code.NotFound, ErrorReason.PIX_NOT_FOUND);
      },
    });
    const res = await h.app.inject({
      url: `/pix/${PIX_ID}`,
      headers: { authorization: `Bearer ${await h.token("acc-1")}` },
    });
    expect(res.statusCode).toBe(404);
  });

  it("ID que não é UUID: 400", async () => {
    setup({});
    const res = await h.app.inject({
      url: "/pix/abc",
      headers: { authorization: `Bearer ${await h.token("acc-1")}` },
    });
    expect(res.statusCode).toBe(400);
  });

  it("transfers fora do ar: 503 com Retry-After", async () => {
    setup({
      getPix: () => {
        throw new ConnectError("down", Code.Unavailable);
      },
    });
    const res = await h.app.inject({
      url: `/pix/${PIX_ID}`,
      headers: { authorization: `Bearer ${await h.token("acc-1")}` },
    });
    expect(res.statusCode).toBe(503);
    expect(res.headers["retry-after"]).toBe("1");
  });
});
