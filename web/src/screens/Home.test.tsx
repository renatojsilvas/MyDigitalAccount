import { render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { fakeApi, problem } from "../test-utils";
import { Home } from "./Home";

const item = {
  id: "e1",
  direction: "credit",
  amountCents: 500_000,
  balanceAfterCents: 500_000,
  description: "Saldo inicial",
  postedAt: "2026-11-12T10:00:00.000Z",
} as const;

const limits = {
  status: "ok",
  dailyLimitCents: 200_000,
  usedTodayCents: 12_345,
  availableTodayCents: 187_655,
  resetsAt: "2026-11-13T03:00:00.000Z",
} as const;

function renderHome(handler: (request: Request) => Response, overrides = {}) {
  const props = { onUnauthorized: vi.fn(), onLogout: vi.fn(), ...overrides };
  render(<Home api={fakeApi(handler)} {...props} />);
  return props;
}

describe("Home", () => {
  it("mostra o saldo e as últimas transações", async () => {
    let authorization: string | null = null;
    renderHome((request) => {
      authorization = request.headers.get("authorization");
      return Response.json({
        balanceCents: 500_000,
        statement: { status: "ok", items: [item] },
        limits,
      });
    });

    expect(await screen.findByText("R$ 5.000,00", { selector: ".balance" })).toBeTruthy();
    const extrato = screen.getByRole("region", { name: "Extrato" });
    expect(within(extrato).getByText("Saldo inicial")).toBeTruthy();
    expect(within(extrato).getByText("+R$ 5.000,00")).toBeTruthy();
    expect(authorization).toBe("Bearer token-de-teste");
  });

  // Critério de pronto da fase: extrato indisponível não derruba a tela.
  it("com o extrato indisponível, mostra o saldo e um aviso no lugar do extrato", async () => {
    renderHome(() =>
      Response.json({
        balanceCents: 123_456,
        statement: { status: "unavailable", reason: "timeout" },
        limits,
      }),
    );

    expect(await screen.findByText("R$ 1.234,56", { selector: ".balance" })).toBeTruthy();
    const extrato = screen.getByRole("region", { name: "Extrato" });
    expect(within(extrato).getByRole("alert").textContent).toContain("extrato está indisponível");
    expect(within(extrato).queryByRole("list")).toBeNull();
    expect(within(extrato).getByRole("button", { name: "Tentar novamente" })).toBeTruthy();
  });

  it("mostra o limite diário de Pix vindo do BFF", async () => {
    renderHome(() =>
      Response.json({ balanceCents: 500_000, statement: { status: "ok", items: [] }, limits }),
    );
    const secao = await screen.findByRole("region", { name: "Limite de Pix" });
    expect(within(secao).getByText("R$ 1.876,55")).toBeTruthy();
    expect(within(secao).getByText(/R\$ 123,45 de R\$ 2\.000,00/)).toBeTruthy();
  });

  // Degradação parcial (Fase 3): sem os limites, a home segue com saldo e extrato.
  it("com os limites indisponíveis, mostra saldo e extrato e um aviso no lugar dos limites", async () => {
    renderHome(() =>
      Response.json({
        balanceCents: 500_000,
        statement: { status: "ok", items: [item] },
        limits: { status: "unavailable", reason: "unavailable" },
      }),
    );
    expect(await screen.findByText("R$ 5.000,00", { selector: ".balance" })).toBeTruthy();
    const secao = screen.getByRole("region", { name: "Limite de Pix" });
    expect(within(secao).getByRole("status").textContent).toContain("indisponível");
    expect(
      within(screen.getByRole("region", { name: "Extrato" })).getByText("Saldo inicial"),
    ).toBeTruthy();
  });

  it("volta ao login quando o BFF responde 401", async () => {
    const props = renderHome(() => problem(401, "unauthenticated"));
    await vi.waitFor(() => {
      expect(props.onUnauthorized).toHaveBeenCalledOnce();
    });
  });

  it("sem saldo (erro do BFF), mostra a falha com o trace e não mostra saldo nenhum", async () => {
    renderHome(() => problem(503, "dependency_unavailable", "abc123"));
    expect((await screen.findByRole("alert")).textContent).toContain("Não foi possível carregar");
    expect(screen.getByText("Trace: abc123")).toBeTruthy();
    expect(screen.queryByText(/R\$/)).toBeNull();
  });
});
