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

/** Respostas de erro que toda rota autenticada que chama o ledger pode devolver. */
export const problemResponses = {
  400: problemSchema,
  401: problemSchema,
  502: problemSchema,
  503: problemSchema,
  504: problemSchema,
};
