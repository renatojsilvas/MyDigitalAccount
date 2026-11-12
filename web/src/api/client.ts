import createClient from "openapi-fetch";
import type { paths } from "./schema";

export interface ApiOptions {
  readonly baseUrl: string;
  /** Lido a cada requisição: o token pode mudar (login, logout) sem recriar o cliente. */
  readonly getToken: () => string | null;
  /** Só para os testes (um servidor falso no lugar da rede). */
  readonly fetch?: (request: Request) => Promise<Response>;
}

/** Cliente HTTP tipado, gerado do OpenAPI do BFF: caminhos, corpos e respostas vêm de `schema.d.ts`. */
export function createApi(options: ApiOptions) {
  const client = createClient<paths>({
    baseUrl: options.baseUrl,
    ...(options.fetch ? { fetch: options.fetch } : {}),
  });
  client.use({
    onRequest({ request }) {
      const token = options.getToken();
      if (token !== null) request.headers.set("authorization", `Bearer ${token}`);
      return request;
    },
  });
  return client;
}

export type Api = ReturnType<typeof createApi>;

/** Tipos de resposta tirados do contrato gerado, nunca escritos à mão. */
export type HomeView = paths["/home"]["get"]["responses"]["200"]["content"]["application/json"];
export type Problem = paths["/home"]["get"]["responses"]["503"]["content"]["application/json"];
