import { useState, type SyntheticEvent } from "react";
import type { Api } from "../api/client";

interface LoginProps {
  readonly api: Api;
  readonly onLoggedIn: (token: string) => void;
  /** Aviso vindo de fora, por exemplo "sessão expirada". */
  readonly notice?: string;
}

/** Login de demonstração: o usuário digita o CPF de uma conta do seed. Não há senha. */
export function Login({ api, onLoggedIn, notice }: LoginProps) {
  const [document, setDocument] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function submit(event: SyntheticEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const { data, response } = await api.POST("/auth/demo-login", {
        body: { document: document.replace(/\D/g, "") },
      });
      if (data) {
        onLoggedIn(data.accessToken);
      } else if (response.status === 400) {
        setError("CPF inválido.");
      } else if (response.status === 401) {
        setError("Não há conta de demonstração para esse CPF.");
      } else {
        setError("Não foi possível entrar agora. Tente de novo.");
      }
    } catch {
      setError("Sem conexão com o servidor.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <main>
      <h1>Conta Digital</h1>
      <p className="hint">Login de demonstração: informe o CPF de uma conta criada pelo seed.</p>
      {notice ? <p role="status">{notice}</p> : null}
      <form onSubmit={(e) => void submit(e)}>
        <label>
          CPF
          <input
            inputMode="numeric"
            autoComplete="off"
            value={document}
            onChange={(e) => {
              setDocument(e.target.value);
            }}
          />
        </label>
        <button type="submit" disabled={busy || document.trim() === ""}>
          Entrar
        </button>
      </form>
      {error ? <p role="alert">{error}</p> : null}
    </main>
  );
}
