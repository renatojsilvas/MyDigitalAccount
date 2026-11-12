import { ROOT_CONTEXT } from "@opentelemetry/api";
import type { FastifyPluginCallbackZod } from "fastify-type-provider-zod";
import { PROBLEM_CONTENT_TYPE, problemResponses, problemSchema } from "../../problem-schema.js";
import { replyWithProblem } from "../../plugins/errors.js";
import { toPixAccepted, toPixView } from "./mapper.js";
import {
  pixAcceptedSchema,
  pixParamsSchema,
  pixViewSchema,
  startPixBodySchema,
  startPixHeadersSchema,
} from "./schema.js";
import type { PixService } from "./service.js";

export interface PixRouteOptions {
  readonly service: PixService;
}

const problem = (description: string) => ({
  description,
  content: { [PROBLEM_CONTENT_TYPE]: { schema: problemSchema } },
});

export const pixRoutes: FastifyPluginCallbackZod<PixRouteOptions> = (app, opts, done) => {
  app.post(
    "/pix",
    {
      onRequest: (request, reply) => app.authenticate(request, reply),
      schema: {
        summary: "Envia um Pix",
        description:
          "Aceita o Pix e responde 202 com o ID, sem esperar a liquidação: o resultado sai depois (ADR-0007). Acompanhe por GET /pix/{pixId}. Recusas de negócio (saldo, limite, antifraude, chave inexistente) aparecem no status do Pix, não aqui.",
        tags: ["pix"],
        security: [{ bearerAuth: [] }],
        headers: startPixHeadersSchema,
        body: startPixBodySchema,
        response: {
          202: pixAcceptedSchema,
          ...problemResponses,
          409: problem("A chave de idempotência já foi usada com outro pedido"),
        },
      },
    },
    async (request, reply) => {
      const result = await opts.service.start(
        request.user.sub,
        request.headers["idempotency-key"],
        request.body,
        request.telemetry?.context ?? ROOT_CONTEXT,
      );
      if (!result.ok) return replyWithProblem(request, reply, result.error);
      request.telemetry?.span.setAttribute("pix.id", result.value.pixId);
      return reply
        .status(202)
        .header("location", `/pix/${result.value.pixId}`)
        .send(toPixAccepted(result.value));
    },
  );

  app.get(
    "/pix/:pixId",
    {
      onRequest: (request, reply) => app.authenticate(request, reply),
      schema: {
        summary: "Consulta um Pix, com o histórico de estados",
        description:
          "Só o Pix da própria conta; o de outra conta responde 404, sem revelar que existe.",
        tags: ["pix"],
        security: [{ bearerAuth: [] }],
        params: pixParamsSchema,
        response: { 200: pixViewSchema, ...problemResponses, 404: problem("Pix não encontrado") },
      },
    },
    async (request, reply) => {
      const result = await opts.service.get(
        request.user.sub,
        request.params.pixId,
        request.telemetry?.context ?? ROOT_CONTEXT,
      );
      if (!result.ok) return replyWithProblem(request, reply, result.error);
      request.telemetry?.span.setAttribute("pix.id", result.value.pixId);
      return toPixView(result.value);
    },
  );
  done();
};
