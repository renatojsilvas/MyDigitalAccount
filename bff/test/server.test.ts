import { afterEach, describe, expect, it } from "vitest";
import { start, type RunningServer } from "../src/server.js";
import { testConfig } from "./harness.js";

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
});
