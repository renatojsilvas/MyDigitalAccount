import { ROOT_CONTEXT } from "@opentelemetry/api";
import type { FastifyPluginCallbackZod } from "fastify-type-provider-zod";
import { replyWithProblem } from "../../plugins/errors.js";
import { pingQuerySchema, pingResponseSchema } from "./schema.js";
import type { PingService } from "./service.js";

export interface PingRouteOptions {
  readonly service: PingService;
}

export const pingRoutes: FastifyPluginCallbackZod<PingRouteOptions> = (app, opts, done) => {
  app.get(
    "/ping",
    { schema: { querystring: pingQuerySchema, response: { 200: pingResponseSchema } } },
    async (request, reply) => {
      const result = await opts.service.ping(
        request.query.message,
        request.telemetry?.context ?? ROOT_CONTEXT,
      );
      if (!result.ok) return replyWithProblem(request, reply, result.error);
      return result.value;
    },
  );
  done();
};
