import { createApi } from "./api/client";

/** Cliente da API apontando para um servidor falso: `handler` responde no lugar do BFF. */
export function fakeApi(handler: (request: Request) => Response | Promise<Response>) {
  return createApi({
    baseUrl: "http://bff.test",
    getToken: () => "token-de-teste",
    fetch: async (request) => handler(request),
  });
}

export const problem = (status: number, code: string, traceId = "trace-123"): Response =>
  Response.json(
    { type: `urn:conta:problem:${code}`, title: code, status, code, trace_id: traceId },
    { status, headers: { "content-type": "application/problem+json" } },
  );
