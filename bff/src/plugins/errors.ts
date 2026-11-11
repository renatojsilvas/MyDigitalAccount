import type { FastifyReply, FastifyRequest } from "fastify";
import fp from "fastify-plugin";
import { hasZodFastifySchemaValidationErrors } from "fastify-type-provider-zod";
import { problem, problemOf, type DependencyError, type ProblemResponse } from "../errors.js";

function send(request: FastifyRequest, reply: FastifyReply, p: ProblemResponse): FastifyReply {
  const traceId = request.telemetry?.span.spanContext().traceId;
  return reply
    .status(p.status)
    .headers(p.headers)
    .type("application/problem+json")
    .send(traceId === undefined ? p.body : { ...p.body, trace_id: traceId });
}

/** Traduz uma falha esperada de dependência para a resposta HTTP. Usado pelas rotas. */
export function replyWithProblem(
  request: FastifyRequest,
  reply: FastifyReply,
  error: DependencyError,
): FastifyReply {
  return send(request, reply, problemOf(error));
}

/**
 * Handler central: tudo o que não foi tratado numa rota (validação, 404, erro inesperado)
 * sai no mesmo formato, sem vazar detalhes internos.
 */
export const errorsPlugin = fp(
  (app, _opts, done) => {
    app.setNotFoundHandler((request, reply) =>
      send(request, reply, problem(404, "not_found", "Recurso não encontrado")),
    );

    app.setErrorHandler((error, request, reply) => {
      if (hasZodFastifySchemaValidationErrors(error)) {
        const detail = error.validation
          .map((v) => `${v.instancePath || "(raiz)"}: ${v.message ?? "inválido"}`)
          .join("; ");
        return send(
          request,
          reply,
          problem(400, "validation_error", "Requisição inválida", detail),
        );
      }

      const status =
        typeof (error as { statusCode?: unknown }).statusCode === "number"
          ? (error as { statusCode: number }).statusCode
          : 500;
      if (status >= 400 && status < 500) {
        return send(request, reply, problem(status, "bad_request", "Requisição inválida"));
      }

      // Erro inesperado: o detalhe vai para o log e o trace, nunca para o cliente.
      request.log.error({ err: error }, "erro não tratado");
      return send(request, reply, problem(500, "internal_error", "Erro interno"));
    });

    done();
  },
  { name: "errors" },
);
