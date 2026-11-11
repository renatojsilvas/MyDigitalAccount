import type { AddressInfo } from "node:net";
import { buildApp, type AppOverrides } from "./app.js";
import type { Config } from "./config.js";

export interface RunningServer {
  readonly address: AddressInfo;
  /** Desligamento gracioso: para de se anunciar pronto, termina o que está em andamento e descarrega a telemetria. */
  stop(): Promise<void>;
}

export async function start(config: Config, overrides: AppOverrides = {}): Promise<RunningServer> {
  const { app, lifecycle } = buildApp(config, overrides);
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
