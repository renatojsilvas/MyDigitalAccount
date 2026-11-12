import type { Result } from "./result.js";

/**
 * Falhas esperadas ao falar com uma dependência. São dados (união discriminada por `kind`),
 * não exceções: quem chama é obrigado a tratá-las, e o compilador acusa um caso esquecido.
 * Quem traduz para HTTP é o `problemOf`, num lugar só.
 */
export type DependencyError =
  | { readonly kind: "timeout"; readonly dependency: string }
  | { readonly kind: "unavailable"; readonly dependency: string }
  | { readonly kind: "not_found"; readonly dependency: string }
  | { readonly kind: "invalid_argument"; readonly detail: string }
  | { readonly kind: "unexpected"; readonly dependency: string };

export type DependencyResult<T> = Result<T, DependencyError>;

/** Falha de identidade: o token (ou a conta a que ele aponta) não vale mais. Sempre 401. */
export interface UnauthenticatedError {
  readonly kind: "unauthenticated";
}

/** Tudo o que uma rota pode devolver como falha esperada; `problemOf` traduz num lugar só. */
export type BffError = DependencyError | UnauthenticatedError;

/** Corpo de erro no formato RFC 9457 (problem details), sem detalhes internos. */
export interface Problem {
  readonly type: string;
  readonly title: string;
  readonly status: number;
  readonly code: string;
  readonly detail?: string;
  readonly trace_id?: string;
}

export interface ProblemResponse {
  readonly status: number;
  readonly body: Problem;
  readonly headers: Readonly<Record<string, string>>;
}

export function problemOf(error: BffError): ProblemResponse {
  switch (error.kind) {
    case "timeout":
      return problem(504, "dependency_timeout", "A dependência não respondeu a tempo");
    case "unavailable":
      return {
        ...problem(503, "dependency_unavailable", "A dependência está indisponível"),
        headers: { "retry-after": "1" },
      };
    case "not_found":
      return problem(404, "not_found", "Recurso não encontrado");
    case "unauthenticated":
      return problem(401, "unauthenticated", "Autenticação necessária");
    case "invalid_argument":
      return problem(400, "invalid_argument", "Requisição inválida", error.detail);
    case "unexpected":
      // Sem detalhe: erro inesperado de dependência não vaza para o cliente.
      return problem(502, "dependency_error", "Falha ao consultar a dependência");
    default:
      return error satisfies never;
  }
}

export function problem(
  status: number,
  code: string,
  title: string,
  detail?: string,
): ProblemResponse {
  return {
    status,
    headers: {},
    body: {
      type: `urn:conta:problem:${code}`,
      title,
      status,
      code,
      ...(detail !== undefined ? { detail } : {}),
    },
  };
}
