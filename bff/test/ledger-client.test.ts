import { ROOT_CONTEXT } from "@opentelemetry/api";
import { describe, expect, it } from "vitest";
import { createLedgerClient, type LedgerClient } from "../src/clients/ledger.js";
import {
  accountNotFound,
  Code,
  ConnectError,
  Direction,
  ErrorReason,
  fakeLedgerTransport,
  posting,
  rejection,
  sleep,
  type LedgerImpl,
} from "./fake-ledger.js";

const call = { context: ROOT_CONTEXT };

function clientFor(impl: LedgerImpl): LedgerClient {
  return createLedgerClient(fakeLedgerTransport(impl), {
    timeoutMs: 100,
    balanceTimeoutMs: 100,
    statementTimeoutMs: 50,
  });
}

describe("adaptador do ledger", () => {
  it("devolve saldo em bigint e extrato como tipos de domínio", async () => {
    const c = clientFor({
      getBalance: () => ({ accountId: "a", balanceCents: 9_007_199_254_740_993n }),
      listStatement: () => ({
        postings: [posting(2, Direction.DEBIT), posting(1)],
        nextPageToken: "",
      }),
    });
    expect(await c.getBalance("a", call)).toEqual({ ok: true, value: 9_007_199_254_740_993n });

    const statement = await c.listStatement("a", 10, call);
    expect(
      statement.ok && statement.value.map((p) => [p.entryId, p.direction, p.amountCents]),
    ).toEqual([
      ["entry-2", "debit", 200n],
      ["entry-1", "credit", 100n],
    ]);
    expect(statement.ok && statement.value[0]?.postedAt.toISOString()).toBe(
      "2026-11-12T10:02:00.000Z",
    );
  });

  it("acha a conta pelo CPF e não expõe o CPF no tipo de domínio", async () => {
    const seen: string[] = [];
    const c = clientFor({
      getAccountByDocument: (req) => {
        seen.push(req.holderDocument);
        return {
          account: { accountId: "acc-9", holderName: "Ana", holderDocument: req.holderDocument },
        };
      },
    });
    const res = await c.findAccountByDocument("12345678909", call);
    expect(res).toEqual({ ok: true, value: { accountId: "acc-9", holderName: "Ana" } });
    expect(seen).toEqual(["12345678909"]);
  });

  it("traduz ACCOUNT_NOT_FOUND (lido do ErrorInfo) para not_found", async () => {
    const c = clientFor({
      getBalance: () => {
        throw accountNotFound();
      },
    });
    expect(await c.getBalance("a", call)).toEqual({
      ok: false,
      error: { kind: "not_found", dependency: "ledger" },
    });
  });

  it("NOT_FOUND sem o motivo esperado é erro inesperado, não conta inexistente", async () => {
    const c = clientFor({
      getBalance: () => {
        throw new ConnectError("rota?", Code.NotFound);
      },
    });
    expect(await c.getBalance("a", call)).toMatchObject({
      ok: false,
      error: { kind: "unexpected" },
    });
  });

  it.each([
    [Code.Unavailable, "unavailable"],
    [Code.Internal, "unexpected"],
    [Code.PermissionDenied, "unexpected"],
  ])("código %s vira %s", async (code, kind) => {
    const c = clientFor({
      getBalance: () => {
        throw new ConnectError("x", code);
      },
    });
    expect(await c.getBalance("a", call)).toMatchObject({ ok: false, error: { kind } });
  });

  it("CPF inválido volta como invalid_argument, com o detalhe do ledger", async () => {
    const c = clientFor({
      getAccountByDocument: () => {
        throw rejection(Code.InvalidArgument, ErrorReason.INVALID_REQUEST, "is not a valid CPF");
      },
    });
    expect(await c.findAccountByDocument("123", call)).toEqual({
      ok: false,
      error: { kind: "invalid_argument", detail: "is not a valid CPF" },
    });
  });

  it("cada chamada tem o seu prazo: o extrato estoura antes do saldo", async () => {
    const c = clientFor({
      getBalance: async () => {
        await sleep(70); // acima do prazo do extrato (50), abaixo do do saldo (100)
        return { accountId: "a", balanceCents: 1n };
      },
      listStatement: async () => {
        await sleep(70);
        return { postings: [], nextPageToken: "" };
      },
    });
    const [balance, statement] = await Promise.all([
      c.getBalance("a", call),
      c.listStatement("a", 10, call),
    ]);
    expect(balance.ok).toBe(true);
    expect(statement).toEqual({ ok: false, error: { kind: "timeout", dependency: "ledger" } });
  });
});
