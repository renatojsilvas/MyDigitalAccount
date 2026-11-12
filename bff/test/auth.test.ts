import { afterEach, describe, expect, it } from "vitest";
import {
  Code,
  ConnectError,
  createHarness,
  forgeToken,
  testConfig,
  validClaims,
  type Harness,
} from "./harness.js";
import { accountNotFound, rejection, ErrorReason } from "./fake-ledger.js";

let h: Harness;
afterEach(async () => {
  await h.close();
});

const balanceOnlyLedger = { getBalance: () => ({ accountId: "a", balanceCents: 1n }) };

describe("autenticação do GET /home", () => {
  const rejected: { name: string; auth: () => string | undefined }[] = [
    { name: "sem Authorization", auth: () => undefined },
    { name: "esquema que não é Bearer", auth: () => "Basic abc" },
    { name: "token malformado", auth: () => "Bearer isto-nao-e-um-jwt" },
    {
      name: "assinatura com outro segredo",
      auth: () =>
        `Bearer ${forgeToken(validClaims("acc-1"), { secret: "outro-segredo-com-mais-de-32-caracteres!!" })}`,
    },
    {
      name: "token expirado",
      auth: () =>
        `Bearer ${forgeToken(validClaims("acc-1", { exp: Math.floor(Date.now() / 1000) - 60 }))}`,
    },
    {
      name: "outro emissor",
      auth: () => `Bearer ${forgeToken(validClaims("acc-1", { iss: "outro" }))}`,
    },
    {
      name: "outro destinatário",
      auth: () => `Bearer ${forgeToken(validClaims("acc-1", { aud: "outro" }))}`,
    },
    {
      name: "alg none (sem assinatura)",
      auth: () => `Bearer ${forgeToken(validClaims("acc-1"), { alg: "none" })}`,
    },
    {
      name: "sem sub",
      auth: () => {
        const claims = Object.fromEntries(
          Object.entries(validClaims("x")).filter(([k]) => k !== "sub"),
        );
        return `Bearer ${forgeToken(claims)}`;
      },
    },
  ];

  it.each(rejected)("recusa com 401 genérico: $name", async ({ auth }) => {
    h = createHarness(undefined, testConfig(), { ledger: balanceOnlyLedger });
    const authorization = auth();
    const res = await h.app.inject({
      url: "/home",
      ...(authorization ? { headers: { authorization } } : {}),
    });
    expect(res.statusCode).toBe(401);
    expect(res.headers["content-type"]).toContain("application/problem+json");
    expect(res.headers["www-authenticate"]).toBe("Bearer");
    // Sempre o mesmo corpo: não diz se expirou, se a assinatura falhou, etc.
    expect(res.json()).toMatchObject({ code: "unauthenticated", status: 401 });
    expect(h.ledgerRequests).toHaveLength(0);
  });

  it("não vaza o token nem o motivo no log", async () => {
    h = createHarness(undefined, testConfig(), { ledger: balanceOnlyLedger });
    const token = forgeToken(validClaims("acc-1"), {
      secret: "outro-segredo-com-mais-de-32-caracteres!!",
    });
    await h.app.inject({ url: "/home", headers: { authorization: `Bearer ${token}` } });
    const text = JSON.stringify(h.stdout());
    expect(text).not.toContain(token);
    expect(h.stdout().some((l) => l.msg === "autenticação recusada")).toBe(true);
  });

  it("a identidade da conta vem do token: é o sub que vai ao ledger", async () => {
    const seen: string[] = [];
    h = createHarness(undefined, testConfig(), {
      ledger: {
        getBalance: (req) => {
          seen.push(req.accountId);
          return { accountId: req.accountId, balanceCents: 1n };
        },
        listStatement: () => ({ postings: [], nextPageToken: "" }),
      },
    });
    const res = await h.app.inject({
      url: "/home?account_id=outra-conta",
      headers: { authorization: `Bearer ${await h.token("acc-do-token")}` },
    });
    expect(res.statusCode).toBe(200);
    expect(seen).toEqual(["acc-do-token"]);
  });

  it("o token emitido pelo BFF expira no prazo configurado", async () => {
    h = createHarness(undefined, testConfig({ BFF_JWT_TTL_SECONDS: "600" }), {
      ledger: balanceOnlyLedger,
    });
    const [, payload] = (await h.token("acc-1")).split(".");
    const claims = JSON.parse(Buffer.from(payload ?? "", "base64url").toString()) as {
      iat: number;
      exp: number;
      iss: string;
      aud: string;
      sub: string;
    };
    expect(claims.exp - claims.iat).toBe(600);
    expect(claims).toMatchObject({ iss: "conta-bff", aud: "conta-web", sub: "acc-1" });
    expect(Object.keys(claims).sort()).toEqual(["aud", "exp", "iat", "iss", "sub"]); // sem CPF
  });
});

describe("POST /auth/demo-login", () => {
  const CPF = "12345678909";

  it("emite um token válido para o CPF que tem conta, e o /home o aceita", async () => {
    h = createHarness(undefined, testConfig(), {
      ledger: {
        getAccountByDocument: (req) => ({
          account: { accountId: "acc-ana", holderName: "Ana", holderDocument: req.holderDocument },
        }),
        getBalance: (req) => ({ accountId: req.accountId, balanceCents: 500_000n }),
        listStatement: () => ({ postings: [], nextPageToken: "" }),
      },
    });
    const login = await h.app.inject({
      method: "POST",
      url: "/auth/demo-login",
      payload: { document: CPF },
    });
    expect(login.statusCode).toBe(200);
    const body = login.json<{ accessToken: string; tokenType: string; expiresIn: number }>();
    expect(body).toMatchObject({ tokenType: "Bearer", expiresIn: 900 });

    const home = await h.app.inject({
      url: "/home",
      headers: { authorization: `Bearer ${body.accessToken}` },
    });
    expect(home.statusCode).toBe(200);
    expect(home.json<{ balanceCents: number }>().balanceCents).toBe(500_000);
  });

  it("CPF sem conta: 401, igual a qualquer credencial inválida", async () => {
    h = createHarness(undefined, testConfig(), {
      ledger: {
        getAccountByDocument: () => {
          throw accountNotFound();
        },
      },
    });
    const res = await h.app.inject({
      method: "POST",
      url: "/auth/demo-login",
      payload: { document: CPF },
    });
    expect(res.statusCode).toBe(401);
    expect(res.json()).toMatchObject({ code: "unauthenticated" });
  });

  it("CPF com forma inválida é recusado na borda, sem chamar o ledger", async () => {
    h = createHarness(undefined, testConfig(), { ledger: {} });
    const res = await h.app.inject({
      method: "POST",
      url: "/auth/demo-login",
      payload: { document: "123.456" },
    });
    expect(res.statusCode).toBe(400);
    expect(res.json()).toMatchObject({ code: "validation_error" });
    expect(JSON.stringify(res.json())).not.toContain("123.456"); // não ecoa o valor
  });

  it("CPF com dígitos verificadores errados: 400 vindo do ledger", async () => {
    h = createHarness(undefined, testConfig(), {
      ledger: {
        getAccountByDocument: () => {
          throw rejection(Code.InvalidArgument, ErrorReason.INVALID_REQUEST, "is not a valid CPF");
        },
      },
    });
    const res = await h.app.inject({
      method: "POST",
      url: "/auth/demo-login",
      payload: { document: "12345678900" },
    });
    expect(res.statusCode).toBe(400);
  });

  it("ledger fora do ar: 503", async () => {
    h = createHarness(undefined, testConfig(), {
      ledger: {
        getAccountByDocument: () => {
          throw new ConnectError("down", Code.Unavailable);
        },
      },
    });
    const res = await h.app.inject({
      method: "POST",
      url: "/auth/demo-login",
      payload: { document: CPF },
    });
    expect(res.statusCode).toBe(503);
  });

  it("nunca registra o CPF em log nem em span", async () => {
    h = createHarness(undefined, testConfig(), {
      ledger: {
        getAccountByDocument: () => {
          throw accountNotFound();
        },
      },
    });
    await h.app.inject({ method: "POST", url: "/auth/demo-login", payload: { document: CPF } });
    await h.built.telemetry.loggerProvider.forceFlush();
    const everything = JSON.stringify([
      h.stdout(),
      h.spans
        .getFinishedSpans()
        .map((s) => ({ name: s.name, attributes: s.attributes, events: s.events })),
      h.logs.getFinishedLogRecords().map((r) => ({ body: r.body, attributes: r.attributes })),
    ]);
    expect(everything).not.toContain(CPF);
  });

  it("em production a rota não existe (404)", async () => {
    h = createHarness(undefined, testConfig({ ENVIRONMENT: "production" }), { ledger: {} });
    const res = await h.app.inject({
      method: "POST",
      url: "/auth/demo-login",
      payload: { document: CPF },
    });
    expect(res.statusCode).toBe(404);
  });
});
