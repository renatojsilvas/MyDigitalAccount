import { ROOT_CONTEXT } from "@opentelemetry/api";
import type { FastifyPluginCallbackZod } from "fastify-type-provider-zod";
import { problemResponses } from "../../problem-schema.js";
import { replyWithProblem } from "../../plugins/errors.js";
import { toHomeView } from "./mapper.js";
import { homeResponseSchema } from "./schema.js";
import type { HomeService } from "./service.js";

export interface HomeRouteOptions {
  readonly service: HomeService;
}

export const homeRoutes: FastifyPluginCallbackZod<HomeRouteOptions> = (app, opts, done) => {
  app.get(
    "/home",
    {
      // onRequest: a identidade é checada antes de validar ou processar qualquer coisa.
      onRequest: (request, reply) => app.authenticate(request, reply),
      schema: {
        summary: "Home: saldo, últimas transações e limite diário de Pix",
        description:
          "O saldo é essencial: se faltar, a resposta é um erro. O extrato e os limites são opcionais: se faltarem, a resposta é 200 com `status = unavailable` na seção.",
        tags: ["home"],
        security: [{ bearerAuth: [] }],
        response: { 200: homeResponseSchema, ...problemResponses },
      },
    },
    async (request, reply) => {
      // A conta vem SEMPRE do token; o cliente não informa account_id em lugar nenhum.
      const result = await opts.service.home(
        request.user.sub,
        request.telemetry?.context ?? ROOT_CONTEXT,
      );
      if (!result.ok) return replyWithProblem(request, reply, result.error);
      const view = toHomeView(result.value);
      const degraded = [
        ...(view.statement.status === "unavailable" ? ["statement"] : []),
        ...(view.limits.status === "unavailable" ? ["limits"] : []),
      ];
      if (degraded.length > 0) request.log = request.log.child({ degraded });
      return view;
    },
  );
  done();
};
