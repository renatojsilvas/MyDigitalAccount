import { z } from "zod";

// Contrato do Pix com o front (ADR-0005). Fonte única: dele saem os tipos, a validação de
// entrada e saída e o OpenAPI de onde o front gera o cliente. O BFF só confere a forma; as
// regras (CPF válido, saldo, limite, antifraude) são do transfers (regra 1).

const cents = z.int().describe("Valor em centavos (inteiro)");

export const pixStatusSchema = z
  .enum(["processing", "completed", "failed", "rejected"])
  .describe(
    "Situação para o cliente: processing (em andamento, inclusive pendente de confirmação do SPI), completed, failed (saiu e voltou: estornado) ou rejected (não saiu)",
  );

export const pixStateSchema = z
  .enum([
    "accepted",
    "validated",
    "debited",
    "settling",
    "pending_confirmation",
    "settled",
    "completed",
    "reversing",
    "reversed",
    "rejected",
  ])
  .describe("Estado detalhado da saga (ADR-0007). Para exibir ao cliente, use `status`.");

export const pixReasonSchema = z
  .enum([
    "antifraud_denied",
    "limit_exceeded",
    "destination_invalid",
    "same_account",
    "insufficient_funds",
    "dependency_unavailable",
    "settlement_rejected",
    "settlement_not_sent",
    "source_account_invalid",
  ])
  .describe("Motivo da recusa (rejected) ou do estorno (failed)");

/** Header de idempotência: de 1 a 128 caracteres ASCII visíveis, como o transfers aceita. */
export const startPixHeadersSchema = z.object({
  "idempotency-key": z
    .string()
    .regex(/^[\x21-\x7E]{1,128}$/, "de 1 a 128 caracteres ASCII visíveis")
    .describe(
      "Chave de idempotência gerada pelo cliente (um UUID, por exemplo). Repetir o pedido com a mesma chave devolve o mesmo Pix; outro pedido com a mesma chave é recusado (409).",
    ),
});

export const startPixBodySchema = z.object({
  destinationKey: z.object({
    type: z.literal("cpf").describe("Tipo da chave Pix; na demonstração, só CPF"),
    value: z
      .string()
      .regex(/^\d{11}$/, "11 dígitos")
      .describe(
        "CPF de destino, só dígitos. Os dígitos verificadores são conferidos pelo transfers.",
      ),
  }),
  amountCents: cents.positive().describe("Valor do Pix em centavos, maior que zero"),
  description: z.string().max(140).optional().describe("Texto opcional que aparece no extrato"),
});

export const pixAcceptedSchema = z.object({
  pixId: z.string().describe("Identificador do Pix; acompanhe por GET /pix/{pixId}"),
  status: pixStatusSchema,
  state: pixStateSchema,
});

export const pixParamsSchema = z.object({ pixId: z.uuid() });

export const pixChangeSchema = z.object({
  state: pixStateSchema,
  reason: pixReasonSchema.optional(),
  at: z.iso.datetime(),
});

export const pixViewSchema = z.object({
  pixId: z.string(),
  status: pixStatusSchema,
  state: pixStateSchema,
  reason: pixReasonSchema.optional(),
  amountCents: cents.describe("Valor do Pix em centavos"),
  description: z.string(),
  destinationKeyMasked: z.string().describe("Chave de destino mascarada (***.456.789-**)"),
  confirmationOverdue: z
    .boolean()
    .describe("Pendente de confirmação do SPI além do prazo; o cliente continua vendo processing"),
  createdAt: z.iso.datetime(),
  updatedAt: z.iso.datetime(),
  history: z
    .array(pixChangeSchema)
    .describe("Histórico de estados, do mais antigo ao mais recente"),
});

export type StartPixBody = z.infer<typeof startPixBodySchema>;
export type PixAcceptedView = z.infer<typeof pixAcceptedSchema>;
export type PixView = z.infer<typeof pixViewSchema>;
