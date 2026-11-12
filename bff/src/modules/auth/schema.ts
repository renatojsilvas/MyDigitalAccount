import { z } from "zod";

// Só a forma. Se o CPF é válido (dígitos verificadores) quem decide é o ledger.
export const demoLoginBodySchema = z.object({
  document: z
    .string()
    .regex(/^\d{11}$/, "deve ter 11 dígitos, sem pontuação")
    .describe("CPF do usuário de demonstração, só dígitos"),
});

export const tokenResponseSchema = z.object({
  accessToken: z.string(),
  tokenType: z.literal("Bearer"),
  expiresIn: z.int().describe("Validade do token, em segundos"),
});

export type TokenView = z.infer<typeof tokenResponseSchema>;
