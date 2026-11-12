import fastifyJwt from "@fastify/jwt";
import type { FastifyReply, FastifyRequest } from "fastify";
import fp from "fastify-plugin";
import { replyWithProblem } from "./errors.js";

declare module "@fastify/jwt" {
  interface FastifyJWT {
    // O token carrega só o account_id (`sub`). Nada de CPF (ADR-0006).
    payload: { sub: string };
    user: { sub: string };
  }
}

declare module "fastify" {
  interface FastifyInstance {
    /** Hook `onRequest` das rotas protegidas: valida o Bearer e preenche `request.user`. */
    authenticate(request: FastifyRequest, reply: FastifyReply): Promise<FastifyReply | undefined>;
  }
}

export const JWT_ALGORITHM = "HS256";

export interface AuthPluginOptions {
  readonly secret: string;
  readonly issuer: string;
  readonly audience: string;
  readonly ttlSeconds: number;
}

/**
 * Autenticação por JWT (ADR-0006). O algoritmo é fixado dos dois lados: o token não escolhe
 * como será verificado (sem isso, `alg: none` e a confusão HS/RS seriam possíveis).
 */
export const authPlugin = fp<AuthPluginOptions>(
  async (app, opts) => {
    await app.register(fastifyJwt, {
      secret: opts.secret,
      sign: {
        algorithm: JWT_ALGORITHM,
        iss: opts.issuer,
        aud: opts.audience,
        expiresIn: opts.ttlSeconds, // número = segundos (o teste confere exp - iat)
      },
      verify: {
        algorithms: [JWT_ALGORITHM],
        allowedIss: opts.issuer,
        allowedAud: opts.audience,
      },
    });

    app.decorate("authenticate", async (request: FastifyRequest, reply: FastifyReply) => {
      try {
        await request.jwtVerify();
        // O tipo diz que `sub` é string, mas um token assinado por nós sem `sub` também passa.
        const sub: unknown = request.user.sub;
        if (typeof sub !== "string" || sub === "") throw new Error("token sem sub");
      } catch (err) {
        // O motivo vai ao log (nunca o token); o cliente recebe sempre o mesmo 401 genérico.
        const reason = (err as { code?: unknown }).code;
        request.log.info(
          { reason: typeof reason === "string" ? reason : "invalid_token" },
          "autenticação recusada",
        );
        return replyWithProblem(request, reply, { kind: "unauthenticated" });
      }
      // O account_id é identificador, não dado pessoal: pode ir ao log e ao trace (nunca a métrica).
      const accountId = request.user.sub;
      request.telemetry?.span.setAttribute("enduser.id", accountId);
      request.log = request.log.child({ account_id: accountId });
      return undefined;
    });
  },
  { name: "auth" },
);
