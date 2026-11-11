import { z } from "zod";

// Só a forma da entrada. Regras de negócio (como o tamanho máximo útil) são do ledger:
// o limite aqui existe apenas para o BFF não aceitar corpo arbitrariamente grande.
export const pingQuerySchema = z.object({
  message: z.string().max(1024).default("ping"),
});

export const pingResponseSchema = z.object({
  echo: z.string(),
  servedBy: z.string(),
});

export type PingView = z.infer<typeof pingResponseSchema>;
