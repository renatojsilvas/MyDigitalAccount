import { useCallback, useMemo, useState } from "react";
import { createApi } from "./api/client";
import { Home } from "./screens/Home";
import { Login } from "./screens/Login";

const TOKEN_KEY = "conta.token";

// sessionStorage: some ao fechar a aba. É uma demonstração; um front de produção guardaria o
// token num cookie HttpOnly emitido pelo BFF, fora do alcance de JavaScript (XSS).
function readToken(): string | null {
  try {
    return sessionStorage.getItem(TOKEN_KEY);
  } catch {
    return null;
  }
}

function writeToken(token: string | null): void {
  try {
    if (token === null) sessionStorage.removeItem(TOKEN_KEY);
    else sessionStorage.setItem(TOKEN_KEY, token);
  } catch {
    // sem armazenamento (modo privado): a sessão vale só até recarregar a página
  }
}

export function App() {
  const [token, setToken] = useState<string | null>(readToken);
  const [notice, setNotice] = useState<string | undefined>();

  // Um cliente por token: no login ele é recriado, e a Home (que só existe com token) o recebe pronto.
  const api = useMemo(() => createApi({ baseUrl: "/api", getToken: () => token }), [token]);

  const signOut = useCallback((message?: string) => {
    writeToken(null);
    setToken(null);
    setNotice(message);
  }, []);
  const onUnauthorized = useCallback(() => {
    signOut("Sua sessão expirou. Entre novamente.");
  }, [signOut]);
  const onLogout = useCallback(() => {
    signOut();
  }, [signOut]);

  if (token === null) {
    return (
      <Login
        api={api}
        {...(notice ? { notice } : {})}
        onLoggedIn={(t) => {
          writeToken(t);
          setNotice(undefined);
          setToken(t);
        }}
      />
    );
  }
  return <Home api={api} onUnauthorized={onUnauthorized} onLogout={onLogout} />;
}
