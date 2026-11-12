import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { fakeApi, problem } from "../test-utils";
import { Login } from "./Login";

function typeCpfAndSubmit(value: string) {
  fireEvent.change(screen.getByLabelText("CPF"), { target: { value } });
  fireEvent.click(screen.getByRole("button", { name: "Entrar" }));
}

describe("Login", () => {
  it("envia só os dígitos do CPF e entrega o token ao entrar", async () => {
    const onLoggedIn = vi.fn();
    let body: unknown;
    render(
      <Login
        api={fakeApi(async (request) => {
          body = await request.json();
          return Response.json({ accessToken: "jwt-abc", tokenType: "Bearer", expiresIn: 900 });
        })}
        onLoggedIn={onLoggedIn}
      />,
    );

    typeCpfAndSubmit("123.456.789-09");

    await vi.waitFor(() => {
      expect(onLoggedIn).toHaveBeenCalledWith("jwt-abc");
    });
    expect(body).toEqual({ document: "12345678909" });
  });

  it("CPF sem conta: mostra o erro e não entra", async () => {
    const onLoggedIn = vi.fn();
    render(<Login api={fakeApi(() => problem(401, "unauthenticated"))} onLoggedIn={onLoggedIn} />);

    typeCpfAndSubmit("52998224725");

    expect((await screen.findByRole("alert")).textContent).toContain("Não há conta");
    expect(onLoggedIn).not.toHaveBeenCalled();
  });
});
