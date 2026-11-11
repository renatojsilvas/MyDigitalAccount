import { ConfigError, loadConfig } from "./config.js";
import { start } from "./server.js";

async function main(): Promise<void> {
  const config = loadConfig(process.env);
  const server = await start(config);

  let stopping = false;
  const shutdown = (signal: string): void => {
    if (stopping) return;
    stopping = true;
    process.stdout.write(`${JSON.stringify({ level: 30, msg: "encerrando", signal })}\n`);
    const deadline = setTimeout(() => {
      process.stderr.write("bff: o desligamento passou do prazo; saindo à força\n");
      process.exit(1);
    }, config.shutdownTimeoutMs);
    deadline.unref();
    server.stop().then(
      () => process.exit(0),
      (err: unknown) => {
        process.stderr.write(`bff: erro ao encerrar: ${String(err)}\n`);
        process.exit(1);
      },
    );
  };
  process.once("SIGINT", () => {
    shutdown("SIGINT");
  });
  process.once("SIGTERM", () => {
    shutdown("SIGTERM");
  });
}

main().catch((err: unknown) => {
  process.stderr.write(`bff: ${err instanceof ConfigError ? err.message : String(err)}\n`);
  process.exit(1);
});
