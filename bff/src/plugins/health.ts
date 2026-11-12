import fp from "fastify-plugin";
import type { Lifecycle } from "../lifecycle.js";

export interface HealthOptions {
  readonly lifecycle: Lifecycle;
}

/**
 * /healthz: o processo está vivo. /readyz: pode receber tráfego.
 *
 * O /readyz NÃO consulta o ledger: o BFF degrada de forma parcial quando uma dependência
 * falha (fase 2). Se a prontidão dependesse dela, o balanceador tiraria o BFF do ar
 * justamente quando ele ainda consegue responder algo útil.
 */
export const healthPlugin = fp<HealthOptions>(
  (app, opts, done) => {
    // Sondas de infraestrutura: fora do contrato com o front (hide: true).
    const hidden = { schema: { hide: true } };
    app.get("/healthz", hidden, () => ({ status: "ok" }));
    app.get("/readyz", hidden, (_request, reply) => {
      if (!opts.lifecycle.isReady()) {
        return reply.status(503).send({ status: "shutting_down" });
      }
      return { status: "ready" };
    });
    done();
  },
  { name: "health" },
);
