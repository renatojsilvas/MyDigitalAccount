import { Code, ConnectError, createRouterTransport, type Interceptor } from "@connectrpc/connect";
import { PingService, type PingResponse } from "@conta/contracts/conta/ping/v1/ping_pb";
import { afterEach, describe, expect, it } from "vitest";
import { start, type RunningServer } from "../src/server.js";
import { hang } from "./fake-ledger.js";
import { testConfig } from "./harness.js";

/** Ledger falso (só o Ping) em memória; `behavior` decide o que ele responde. */
function pingTransport(
  behavior: (
    message: string,
    ctx: { readonly signal: AbortSignal },
  ) => Promise<PingResponse> | PingResponse,
) {
  return (interceptors: Interceptor[]) =>
    createRouterTransport(
      ({ service }) => {
        service(PingService, { ping: (req, ctx) => behavior(req.message, ctx) });
      },
      { transport: { interceptors } },
    );
}

let server: RunningServer | undefined;
afterEach(async () => {
  await server?.stop().catch(() => undefined);
  server = undefined;
});

describe("start", () => {
  it("sobe em porta efêmera, responde às sondas e encerra com graça", async () => {
    const lines: string[] = [];
    server = await start(testConfig({ BFF_HOST: "127.0.0.1", BFF_PORT: "0" }), {
      stdout: { write: (l: string) => void lines.push(l) },
    });
    const base = `http://127.0.0.1:${String(server.address.port)}`;

    expect((await fetch(`${base}/healthz`)).status).toBe(200);
    expect((await fetch(`${base}/readyz`)).status).toBe(200);
    expect(lines.some((l) => l.includes("serviço iniciado"))).toBe(true);

    await server.stop();
    server = undefined;
    await expect(fetch(`${base}/healthz`)).rejects.toThrow(); // porta fechada
  });

  describe("aquecimento da conexão com o ledger", () => {
    const config = () => testConfig({ BFF_HOST: "127.0.0.1", BFF_PORT: "0" });

    it("chama o ledger uma vez, antes de aceitar tráfego", async () => {
      const messages: string[] = [];
      const lines: string[] = [];
      server = await start(config(), {
        stdout: { write: (l: string) => void lines.push(l) },
        ledgerTransport: pingTransport((message) => {
          messages.push(message);
          return { message, service: "ledger" } as PingResponse;
        }),
      });
      expect(messages).toEqual(["warmup"]); // já tinha acontecido quando start() devolveu
      expect(lines.some((l) => l.includes("conexão com o ledger aquecida"))).toBe(true);
    });

    it("ledger fora do ar: o boot segue, só avisa, e o /readyz continua 200", async () => {
      const lines: string[] = [];
      server = await start(config(), {
        stdout: { write: (l: string) => void lines.push(l) },
        ledgerTransport: pingTransport(() => {
          throw new ConnectError("down", Code.Unavailable);
        }),
      });
      const base = `http://127.0.0.1:${String(server.address.port)}`;
      expect((await fetch(`${base}/readyz`)).status).toBe(200);
      expect(lines.some((l) => l.includes("aquecimento do ledger falhou"))).toBe(true);
    });

    it("ledger que nunca responde: o boot espera no máximo o prazo da chamada", async () => {
      // O prazo (LEDGER_TIMEOUT_MS) é curto e o ledger não responde nunca: start() tem de terminar.
      server = await start(
        testConfig({ BFF_HOST: "127.0.0.1", BFF_PORT: "0", LEDGER_TIMEOUT_MS: "30" }),
        { ledgerTransport: pingTransport((_message, ctx) => hang(ctx)) },
      );
      const base = `http://127.0.0.1:${String(server.address.port)}`;
      expect((await fetch(`${base}/readyz`)).status).toBe(200);
    });
  });
});
