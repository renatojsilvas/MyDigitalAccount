import { ROOT_CONTEXT } from "@opentelemetry/api";
import { describe, expect, it } from "vitest";
import { createLedgerClient } from "../src/clients/ledger.js";
import { createTransfersClient } from "../src/clients/transfers.js";
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
  gate,
  hang,
  posting,
  type LedgerImpl,
} from "./fake-ledger.js";
import { fakeTransfersTransport, okLimits, type TransfersImpl } from "./fake-transfers.js";

interface Degradation {
  section: HomeSection;
  reason: DegradedReason;
}

/** Prazos generosos por padrão; os testes de timeout os encurtam e usam um handler que nunca responde. */
interface Deadlines {
  balanceMs?: number;
  statementMs?: number;
  limitsMs?: number;
}

function setup(
  impl: LedgerImpl,
  deadlines: Deadlines = {},
  transfersImpl: TransfersImpl = { getLimits: okLimits },
) {
  const degradations: Degradation[] = [];
  const observer: HomeObserver = {
    degraded: (section, reason) => void degradations.push({ section, reason }),
  };
  const ledger = createLedgerClient(fakeLedgerTransport(impl), {
    timeoutMs: 10_000,
    balanceTimeoutMs: deadlines.balanceMs ?? 10_000,
    statementTimeoutMs: deadlines.statementMs ?? 10_000,
  });
  const transfers = createTransfersClient(fakeTransfersTransport(transfersImpl), {
    timeoutMs: 10_000,
    limitsTimeoutMs: deadlines.limitsMs ?? 10_000,
  });
  return { service: createHomeService({ ledger, transfers, observer }), degradations };
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
    // Barreira: cada handler só responde quando os DOIS já chegaram ao ledger. Se as chamadas
    // fossem em sequência, o primeiro ficaria esperando para sempre (e estouraria o prazo).
    const bothArrived = gate();
    let arrived = 0;
    const arrive = async () => {
      if (++arrived === 2) bothArrived.open();
      await bothArrived.wait();
    };
    const { service } = setup(
      {
        getBalance: async () => {
          await arrive();
          return { accountId: "a", balanceCents: 1n };
        },
        listStatement: async () => {
          await arrive();
          return { postings: [], nextPageToken: "" };
        },
      },
      { balanceMs: 2_000, statementMs: 2_000 },
    );
    const res = await service.home("a", ROOT_CONTEXT);
    expect(res.ok).toBe(true);
    expect(arrived).toBe(2);
  });

  const statementFailures: {
    name: string;
    run: LedgerImpl["listStatement"];
    reason: DegradedReason;
    deadlines?: Deadlines;
  }[] = [
    {
      name: "lento (estoura o prazo do extrato)",
      run: (_req, ctx) => hang(ctx),
      deadlines: { statementMs: 20 },
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
    const { service, degradations } = setup(
      { getBalance: okBalance, listStatement: f.run },
      f.deadlines,
    );
    const res = await service.home("a", ROOT_CONTEXT);
    expect(res.ok).toBe(true);
    if (!res.ok) return;
    expect(res.value.balanceCents).toBe(123_456n);
    expect(res.value.statement).toEqual({ status: "unavailable", reason: f.reason });
    expect(degradations).toEqual([{ section: "statement", reason: f.reason }]);
  });

  const balanceFailures: {
    name: string;
    run: LedgerImpl["getBalance"];
    kind: string;
    deadlines?: Deadlines;
  }[] = [
    {
      name: "lento",
      run: (_req, ctx) => hang(ctx),
      deadlines: { balanceMs: 20 },
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
    const { service, degradations } = setup(
      { getBalance: f.run, listStatement: okStatement },
      f.deadlines,
    );
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

describe("limites na home (Fase 3)", () => {
  it("vêm do transfers, em paralelo com o ledger", async () => {
    const { service, degradations } = setup({ getBalance: okBalance, listStatement: okStatement });
    const res = await service.home("a", ROOT_CONTEXT);
    expect(res.ok && res.value.limits).toEqual({
      status: "ok",
      limits: {
        dailyLimitCents: 200_000n,
        usedTodayCents: 12_345n,
        availableTodayCents: 187_655n,
        resetsAt: new Date("2026-11-13T03:00:00.000Z"),
      },
    });
    expect(degradations).toEqual([]);
  });

  it.each([
    [
      "lento",
      { getLimits: (_r: unknown, ctx: { signal: AbortSignal }) => hang(ctx) },
      { limitsMs: 20 },
      "timeout",
    ],
    [
      "fora do ar",
      {
        getLimits: () => {
          throw new ConnectError("down", Code.Unavailable);
        },
      },
      {},
      "unavailable",
    ],
  ] as const)(
    "transfers %s: home com saldo e extrato, limites indisponíveis",
    async (_n, impl, dl, reason) => {
      const { service, degradations } = setup(
        { getBalance: okBalance, listStatement: okStatement },
        dl,
        impl,
      );
      const res = await service.home("a", ROOT_CONTEXT);
      expect(res.ok).toBe(true);
      if (!res.ok) return;
      expect(res.value.balanceCents).toBe(123_456n);
      expect(res.value.statement.status).toBe("ok");
      expect(res.value.limits).toEqual({ status: "unavailable", reason });
      expect(degradations).toEqual([{ section: "limits", reason }]);
    },
  );
});

describe("mapper da home", () => {
  it("monta o view model e ele cumpre o schema de saída", () => {
    const view = toHomeView({
      balanceCents: 123_456n,
      limits: {
        status: "ok",
        limits: {
          dailyLimitCents: 200_000n,
          usedTodayCents: 12_345n,
          availableTodayCents: 187_655n,
          resetsAt: new Date("2026-11-13T03:00:00.000Z"),
        },
      },
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
      limits: {
        status: "ok",
        dailyLimitCents: 200_000,
        usedTodayCents: 12_345,
        availableTodayCents: 187_655,
        resetsAt: "2026-11-13T03:00:00.000Z",
      },
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
        limits: { status: "unavailable", reason: "unavailable" },
      }),
    ).toEqual({
      balanceCents: 0,
      statement: { status: "unavailable", reason: "timeout" },
      limits: { status: "unavailable", reason: "unavailable" },
    });
  });

  it("valor acima do inteiro seguro do JSON falha em vez de arredondar", () => {
    expect(() =>
      toHomeView({
        balanceCents: BigInt(Number.MAX_SAFE_INTEGER) + 1n,
        statement: { status: "ok", postings: [] },
        limits: { status: "unavailable", reason: "error" },
      }),
    ).toThrow(RangeError);
  });
});
