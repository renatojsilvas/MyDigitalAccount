import { z } from "zod";

/** Corpo de erro RFC 9457 (ver `Problem` em errors.ts), descrito para o OpenAPI e a serialização. */
export const problemSchema = z.object({
  type: z.string(),
  title: z.string(),
  status: z.int(),
  code: z.string().describe("Código estável para o cliente tratar o erro"),
  detail: z.string().optional(),
  trace_id: z.string().optional().describe("Mesmo valor do header x-trace-id"),
});

/** O BFF responde erros como RFC 9457: o content-type é application/problem+json, não application/json. */
export const PROBLEM_CONTENT_TYPE = "application/problem+json";

const problemResponse = { content: { [PROBLEM_CONTENT_TYPE]: { schema: problemSchema } } };

/** Respostas de erro que toda rota que chama o ledger pode devolver, descritas com o content-type real. */
export const problemResponses = {
  400: problemResponse,
  401: problemResponse,
  502: problemResponse,
  503: problemResponse,
  504: problemResponse,
};
