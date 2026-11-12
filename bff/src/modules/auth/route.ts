import { ROOT_CONTEXT } from "@opentelemetry/api";
import type { FastifyPluginCallbackZod } from "fastify-type-provider-zod";
import { problemResponses } from "../../problem-schema.js";
import { replyWithProblem } from "../../plugins/errors.js";
import { demoLoginBodySchema, tokenResponseSchema } from "./schema.js";
import type { AuthService } from "./service.js";

export interface AuthRouteOptions {
  readonly service: AuthService;
  readonly ttlSeconds: number;
}

/** Só é registrada quando o login de demonstração está ligado (nunca em production). */
export const authRoutes: FastifyPluginCallbackZod<AuthRouteOptions> = (app, opts, done) => {
  app.post(
    "/auth/demo-login",
    {
      schema: {
        summary: "Login de demonstração (só desenvolvimento)",
        description:
          "Emite um token para um CPF que tenha conta no ledger. Não há senha: existe apenas para a demonstração e não é registrado em production.",
        tags: ["auth"],
        body: demoLoginBodySchema,
        response: { 200: tokenResponseSchema, ...problemResponses },
      },
    },
    async (request, reply) => {
      const result = await opts.service.demoLogin(
        request.body.document,
        request.telemetry?.context ?? ROOT_CONTEXT,
      );
      if (!result.ok) return replyWithProblem(request, reply, result.error);
      return {
        accessToken: app.jwt.sign({ sub: result.value.accountId }),
        tokenType: "Bearer" as const,
        expiresIn: opts.ttlSeconds,
      };
    },
  );
  done();
};
