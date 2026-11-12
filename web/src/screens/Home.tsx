import { useEffect, useState } from "react";
import type { Api, HomeView, Problem } from "../api/client";
import { formatBRL } from "../money";

interface HomeProps {
  readonly api: Api;
  /** O token não vale mais (expirou, ou a conta sumiu): volta ao login. */
  readonly onUnauthorized: () => void;
  readonly onLogout: () => void;
}

type State =
  | { readonly kind: "loading" }
  | { readonly kind: "ready"; readonly home: HomeView }
  | { readonly kind: "failed"; readonly message: string; readonly traceId?: string };

/** A tela só exibe o que o BFF devolve; nenhuma regra de negócio mora aqui. */
export function Home({ api, onUnauthorized, onLogout }: HomeProps) {
  const [state, setState] = useState<State>({ kind: "loading" });
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    async function load() {
      setState({ kind: "loading" });
      try {
        const { data, error, response } = await api.GET("/home", { signal: controller.signal });
        if (controller.signal.aborted) return;
        if (data) {
          setState({ kind: "ready", home: data });
        } else if (response.status === 401) {
          onUnauthorized();
        } else {
          const problem = error as Problem | undefined;
          setState({
            kind: "failed",
            message: "Não foi possível carregar a sua conta agora.",
            ...(problem?.trace_id ? { traceId: problem.trace_id } : {}),
          });
        }
      } catch {
        if (!controller.signal.aborted) {
          setState({ kind: "failed", message: "Sem conexão com o servidor." });
        }
      }
    }
    void load();
    return () => {
      controller.abort();
    };
  }, [api, attempt, onUnauthorized]);

  const retry = (
    <button
      type="button"
      onClick={() => {
        setAttempt((n) => n + 1);
      }}
    >
      Tentar novamente
    </button>
  );

  return (
    <main>
      <header>
        <h1>Conta Digital</h1>
        <button type="button" onClick={onLogout}>
          Sair
        </button>
      </header>

      {state.kind === "loading" ? <p>Carregando…</p> : null}

      {state.kind === "failed" ? (
        <section role="alert">
          <p>{state.message}</p>
          {state.traceId ? <p className="hint">Trace: {state.traceId}</p> : null}
          {retry}
        </section>
      ) : null}

      {state.kind === "ready" ? (
        <>
          <section aria-label="Saldo">
            <h2>Saldo</h2>
            <p className="balance">{formatBRL(state.home.balanceCents)}</p>
          </section>

          <section aria-label="Limite de Pix">
            <h2>Limite diário de Pix</h2>
            {state.home.limits.status === "unavailable" ? (
              <p role="status">O limite está indisponível no momento.</p>
            ) : (
              <dl>
                <dt>Disponível hoje</dt>
                <dd className="limit-available">
                  {formatBRL(state.home.limits.availableTodayCents)}
                </dd>
                <dt>Usado hoje</dt>
                <dd>
                  {formatBRL(state.home.limits.usedTodayCents)} de{" "}
                  {formatBRL(state.home.limits.dailyLimitCents)}
                </dd>
              </dl>
            )}
          </section>

          <section aria-label="Extrato">
            <h2>Últimas transações</h2>
            {state.home.statement.status === "unavailable" ? (
              <div role="alert">
                <p>O extrato está indisponível no momento. O seu saldo acima está correto.</p>
                {retry}
              </div>
            ) : state.home.statement.items.length === 0 ? (
              <p>Nenhuma transação ainda.</p>
            ) : (
              <ul>
                {state.home.statement.items.map((item) => (
                  <li key={item.id}>
                    <span>{item.description}</span>
                    <span className={item.direction}>
                      {item.direction === "credit" ? "+" : "-"}
                      {formatBRL(item.amountCents)}
                    </span>
                    <time dateTime={item.postedAt}>
                      {new Date(item.postedAt).toLocaleString("pt-BR")}
                    </time>
                  </li>
                ))}
              </ul>
            )}
          </section>
        </>
      ) : null}
    </main>
  );
}
