import { z } from "zod";

// Contrato da home com o front (ADR-0005). Fonte única: dele saem o tipo da resposta
// (z.infer), a validação da saída e o OpenAPI de onde o front gera os tipos.
//
// Dinheiro é inteiro em centavos; z.int() só aceita inteiros seguros do JS (até 2^53).
const cents = z.int().describe("Valor em centavos (inteiro)");

export const statementItemSchema = z.object({
  id: z.string().describe("Identificador do lançamento"),
  direction: z.enum(["credit", "debit"]),
  amountCents: cents.describe("Valor do lançamento em centavos, sempre positivo"),
  balanceAfterCents: cents.describe("Saldo da conta logo depois do lançamento"),
  description: z.string(),
  postedAt: z.iso.datetime().describe("Momento do lançamento, ISO 8601 em UTC"),
});

/** Por que uma seção opcional não veio. Conjunto fixo: também é rótulo de métrica. */
export const degradedReasonSchema = z.enum(["timeout", "unavailable", "error"]);

// União discriminada por `status`: o front distingue "sem movimentos" de "falhou".
export const statementSectionSchema = z.discriminatedUnion("status", [
  z.object({ status: z.literal("ok"), items: z.array(statementItemSchema) }),
  z.object({ status: z.literal("unavailable"), reason: degradedReasonSchema }),
]);

export const homeResponseSchema = z.object({
  balanceCents: cents.describe("Saldo atual em centavos"),
  statement: statementSectionSchema,
});

export type HomeView = z.infer<typeof homeResponseSchema>;
export type StatementItemView = z.infer<typeof statementItemSchema>;
export type DegradedReason = z.infer<typeof degradedReasonSchema>;
