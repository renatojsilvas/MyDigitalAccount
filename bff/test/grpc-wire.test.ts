import http2 from "node:http2";
import type { AddressInfo } from "node:net";
import { connectNodeAdapter } from "@connectrpc/connect-node";
import type { HandlerContext } from "@connectrpc/connect";
import {
  PingService,
  type PingRequest,
  type PingResponse,
} from "@conta/contracts/conta/ping/v1/ping_pb";
import { afterEach, describe, expect, it } from "vitest";
import { createHarness, testConfig, type Harness } from "./harness.js";

// Estes testes usam gRPC de verdade (HTTP/2 em texto, como o servidor Go do ledger), não o
// transporte em memória: é o que prova o que realmente vai para a rede.

interface FakeLedger {
  url: string;
  requests: Headers[];
  close(): Promise<void>;
}

async function startFakeLedger(
  behavior: (req: PingRequest, ctx: HandlerContext) => PingResponse | Promise<PingResponse>,
): Promise<FakeLedger> {
  const requests: Headers[] = [];
  const handler = connectNodeAdapter({
    routes: (router) => {
      router.service(PingService, {
        ping: (req, ctx) => {
          requests.push(new Headers(ctx.requestHeader));
          return behavior(req, ctx);
        },
      });
    },
  });
  const server = http2.createServer(handler);
  // O cliente mantém a sessão HTTP/2 aberta; sem destruí-la, server.close() nunca termina.
  const sessions = new Set<http2.ServerHttp2Session>();
  server.on("session", (session) => {
    sessions.add(session);
    session.on("close", () => sessions.delete(session));
  });
  await new Promise<void>((resolve) => {
    server.listen(0, "127.0.0.1", resolve);
  });
  const { port } = server.address() as AddressInfo;
  return {
    url: `http://127.0.0.1:${String(port)}`,
    requests,
    close: () =>
      new Promise<void>((resolve) => {
        server.close(() => {
          resolve();
        });
        for (const session of sessions) session.destroy();
      }),
  };
}

let h: Harness;
let ledger: FakeLedger | undefined;
afterEach(async () => {
  await h.close();
  await ledger?.close();
  ledger = undefined;
});

describe("BFF -> ledger por gRPC sobre HTTP/2", () => {
  it("chega ao ledger com o protocolo gRPC, o prazo (grpc-timeout) e o traceparent", async () => {
    ledger = await startFakeLedger(
      (req) => ({ message: req.message, service: "ledger" }) as PingResponse,
    );
    h = createHarness(
      undefined,
      testConfig({ LEDGER_GRPC_URL: ledger.url, LEDGER_TIMEOUT_MS: "150" }),
      {
        realTransport: true,
      },
    );

    const res = await h.app.inject({ url: "/ping?message=oi" });
    expect(res.statusCode).toBe(200);
    expect(res.json()).toEqual({ echo: "oi", servedBy: "ledger" });

    const headers = ledger.requests[0];
    expect(headers?.get("content-type")).toContain("application/grpc");
    // O timeout do BFF vira deadline no ledger: o Go o enxerga como prazo do contexto.
    expect(headers?.get("grpc-timeout")).toBe("150m");

    const client = h.spanNamed("conta.ping.v1.PingService/Ping");
    expect(headers?.get("traceparent")).toBe(
      `00-${client.spanContext().traceId}-${client.spanContext().spanId}-01`,
    );
  });

  it("ledger fora do ar vira 503, com o span de cliente marcado como UNAVAILABLE", async () => {
    // Porta que acabou de ser liberada: ninguém escuta nela (conexão recusada).
    const dead = await startFakeLedger(() => ({ message: "", service: "" }) as PingResponse);
    const url = dead.url;
    await dead.close();

    h = createHarness(undefined, testConfig({ LEDGER_GRPC_URL: url }), {
      realTransport: true,
    });
    const res = await h.app.inject({ url: "/ping" });

    expect(res.statusCode).toBe(503);
    expect(res.json()).toMatchObject({ code: "dependency_unavailable" });
    expect(h.spanNamed("conta.ping.v1.PingService/Ping").attributes["rpc.grpc.status_code"]).toBe(
      14,
    );
  });

  it("ledger lento: o prazo é respeitado e a resposta é 504 sem esperar o ledger", async () => {
    ledger = await startFakeLedger(async (req) => {
      await new Promise((r) => setTimeout(r, 1_000));
      return { message: req.message, service: "ledger" } as PingResponse;
    });
    h = createHarness(
      undefined,
      testConfig({ LEDGER_GRPC_URL: ledger.url, LEDGER_TIMEOUT_MS: "100" }),
      {
        realTransport: true,
      },
    );

    const started = performance.now();
    const res = await h.app.inject({ url: "/ping" });
    const elapsed = performance.now() - started;

    expect(res.statusCode).toBe(504);
    expect(res.json()).toMatchObject({ code: "dependency_timeout" });
    expect(elapsed).toBeLessThan(700); // bem antes do 1s do ledger
    expect(h.spanNamed("conta.ping.v1.PingService/Ping").attributes["rpc.grpc.status_code"]).toBe(
      4,
    );
  });
});
