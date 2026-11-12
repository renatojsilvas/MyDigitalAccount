import type { AddressInfo } from "node:net";
import { buildApp, type AppOverrides } from "./app.js";
import type { Config } from "./config.js";

export interface RunningServer {
  readonly address: AddressInfo;
  /** Desligamento gracioso: para de se anunciar pronto, termina o que está em andamento e descarrega a telemetria. */
  stop(): Promise<void>;
}

export async function start(config: Config, overrides: AppOverrides = {}): Promise<RunningServer> {
  const { app, lifecycle, warmUp } = buildApp(config, overrides);
  // Antes de aceitar tráfego, mas sem poder derrubar o boot nem entrar na prontidão: o prazo é o
  // da própria chamada ao ledger (LEDGER_TIMEOUT_MS) e a falha só vira um aviso no log.
  await warmUp();
  await app.listen({ host: config.host, port: config.port });
  const address = app.server.address() as AddressInfo;
  app.log.info(
    {
      host: address.address,
      port: address.port,
      environment: config.environment,
      otlp_export: config.otlpEndpoint !== undefined,
    },
    "serviço iniciado",
  );

  return {
    address,
    async stop() {
      lifecycle.markShuttingDown();
      await app.close();
    },
  };
}
