import { ROOT_CONTEXT } from "@opentelemetry/api";
import { describe, expect, it } from "vitest";
import { createLedgerClient } from "../src/clients/ledger.js";
import { toHomeView } from "../src/modules/home/mapper.js";
import type { HomeObserver, HomeSection } from "../src/modules/home/observe.js";
import { homeResponseSchema, type DegradedReason } from "../src/modules/home/schema.js";
import { createHomeService } from "../src/modules/home/service.js";
import {
  accountNotFound,
  Code,
  ConnectError,
  Direction,
  fakeLedgerTransport,
  posting,
  sleep,
  type LedgerImpl,
} from "./fake-ledger.js";

interface Degradation {
  section: HomeSection;
  reason: DegradedReason;
}

function setup(impl: LedgerImpl) {
  const degradations: Degradation[] = [];
  const observer: HomeObserver = {
    degraded: (section, reason) => void degradations.push({ section, reason }),
  };
  const ledger = createLedgerClient(fakeLedgerTransport(impl), {
    timeoutMs: 100,
    balanceTimeoutMs: 100,
    statementTimeoutMs: 50,
  });
  return { service: createHomeService({ ledger, observer }), degradations };
}

const okBalance: LedgerImpl["getBalance"] = () => ({ accountId: "a", balanceCents: 123_456n });
const okStatement: LedgerImpl["listStatement"] = () => ({
  postings: [posting(2, Direction.DEBIT), posting(1)],
  nextPageToken: "",
});

describe("serviço da home", () => {
  it("caminho feliz: saldo e extrato, sem degradação", async () => {
    const { service, degradations } = setup({ getBalance: okBalance, listStatement: okStatement });
    const res = await service.home("a", ROOT_CONTEXT);
    expect(res.ok).toBe(true);
    if (!res.ok) return;
    expect(res.value.balanceCents).toBe(123_456n);
    expect(res.value.statement.status).toBe("ok");
    expect(degradations).toEqual([]);
  });

  it("as duas chamadas saem em paralelo", async () => {
    let inFlight = 0;
    let maxInFlight = 0;
    const track =
      <T>(value: T) =>
      async () => {
        maxInFlight = Math.max(maxInFlight, ++inFlight);
        await sleep(20);
        inFlight--;
        return value;
      };
    const { service } = setup({
      getBalance: track({ accountId: "a", balanceCents: 1n }),
      listStatement: track({ postings: [], nextPageToken: "" }),
    });
    await service.home("a", ROOT_CONTEXT);
    expect(maxInFlight).toBe(2);
  });

  const statementFailures: {
    name: string;
    run: LedgerImpl["listStatement"];
    reason: DegradedReason;
  }[] = [
    {
      name: "lento (estoura o prazo do extrato)",
      run: async () => {
        await sleep(120);
        return { postings: [], nextPageToken: "" };
      },
      reason: "timeout",
    },
    {
      name: "fora do ar",
      run: () => {
        throw new ConnectError("down", Code.Unavailable);
      },
      reason: "unavailable",
    },
    {
      name: "erro interno",
      run: () => {
        throw new ConnectError("boom", Code.Internal);
      },
      reason: "error",
    },
  ];
  it.each(statementFailures)("extrato $name: home com saldo e extrato indisponível", async (f) => {
    const { service, degradations } = setup({ getBalance: okBalance, listStatement: f.run });
    const res = await service.home("a", ROOT_CONTEXT);
    expect(res.ok).toBe(true);
    if (!res.ok) return;
    expect(res.value.balanceCents).toBe(123_456n);
    expect(res.value.statement).toEqual({ status: "unavailable", reason: f.reason });
    expect(degradations).toEqual([{ section: "statement", reason: f.reason }]);
  });

  const balanceFailures: { name: string; run: LedgerImpl["getBalance"]; kind: string }[] = [
    {
      name: "lento",
      run: async () => {
        await sleep(150);
        return { accountId: "a", balanceCents: 1n };
      },
      kind: "timeout",
    },
    {
      name: "fora do ar",
      run: () => {
        throw new ConnectError("down", Code.Unavailable);
      },
      kind: "unavailable",
    },
    {
      name: "conta do token inexistente",
      run: () => {
        throw accountNotFound();
      },
      kind: "unauthenticated",
    },
  ];
  it.each(balanceFailures)("saldo $name: a home falha (não há saldo aproximado)", async (f) => {
    const { service, degradations } = setup({ getBalance: f.run, listStatement: okStatement });
    const res = await service.home("a", ROOT_CONTEXT);
    expect(res).toMatchObject({ ok: false, error: { kind: f.kind } });
    expect(degradations).toEqual([]); // falha do essencial não é "degradação"
  });

  it("conta inexistente nos dois: prevalece o 401 do saldo", async () => {
    const gone = () => {
      throw accountNotFound();
    };
    const { service } = setup({ getBalance: gone, listStatement: gone });
    expect(await service.home("a", ROOT_CONTEXT)).toEqual({
      ok: false,
      error: { kind: "unauthenticated" },
    });
  });
});

describe("mapper da home", () => {
  it("monta o view model e ele cumpre o schema de saída", () => {
    const view = toHomeView({
      balanceCents: 123_456n,
      statement: {
        status: "ok",
        postings: [
          {
            entryId: "e1",
            direction: "debit",
            amountCents: 250n,
            balanceAfterCents: 123_456n,
            description: "Pix",
            postedAt: new Date("2026-11-12T10:00:00Z"),
          },
        ],
      },
    });
    expect(view).toEqual({
      balanceCents: 123_456,
      statement: {
        status: "ok",
        items: [
          {
            id: "e1",
            direction: "debit",
            amountCents: 250,
            balanceAfterCents: 123_456,
            description: "Pix",
            postedAt: "2026-11-12T10:00:00.000Z",
          },
        ],
      },
    });
    expect(homeResponseSchema.safeParse(view).success).toBe(true);
  });

  it("extrato indisponível carrega o motivo", () => {
    expect(
      toHomeView({
        balanceCents: 0n,
        statement: { status: "unavailable", reason: "timeout" },
      }),
    ).toEqual({ balanceCents: 0, statement: { status: "unavailable", reason: "timeout" } });
  });

  it("valor acima do inteiro seguro do JSON falha em vez de arredondar", () => {
    expect(() =>
      toHomeView({
        balanceCents: BigInt(Number.MAX_SAFE_INTEGER) + 1n,
        statement: { status: "ok", postings: [] },
      }),
    ).toThrow(RangeError);
  });
});
