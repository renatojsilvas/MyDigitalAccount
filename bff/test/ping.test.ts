import { SpanKind, SpanStatusCode } from "@opentelemetry/api";
import { afterEach, describe, expect, it } from "vitest";
import { Code, ConnectError, createHarness, type Harness, type LedgerBehavior } from "./harness.js";

let h: Harness;
afterEach(async () => {
  await h.close();
});

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

describe("GET /ping", () => {
  it("devolve o view model montado a partir do ledger", async () => {
    h = createHarness();
    const res = await h.app.inject({ url: "/ping?message=oi" });
    expect(res.statusCode).toBe(200);
    expect(res.json()).toEqual({ echo: "oi", servedBy: "ledger" });
  });

  it("usa a mensagem padrão quando não vem nenhuma", async () => {
    h = createHarness();
    const res = await h.app.inject({ url: "/ping" });
    expect(res.json()).toEqual({ echo: "ping", servedBy: "ledger" });
  });

  it("recusa mensagem grande demais na borda, com problem details", async () => {
    h = createHarness();
    const res = await h.app.inject({ url: `/ping?message=${"a".repeat(1025)}` });
    expect(res.statusCode).toBe(400);
    expect(res.headers["content-type"]).toContain("application/problem+json");
    expect(res.json()).toMatchObject({ code: "validation_error", status: 400 });
    expect(h.ledgerRequests).toHaveLength(0); // nem chegou ao ledger
  });
});

describe("falhas do ledger viram respostas HTTP previsíveis", () => {
  const cases: { name: string; behavior: LedgerBehavior; status: number; code: string }[] = [
    {
      name: "indisponível",
      behavior: () => {
        throw new ConnectError("down", Code.Unavailable);
      },
      status: 503,
      code: "dependency_unavailable",
    },
    {
      name: "lento demais",
      behavior: async () => {
        await sleep(400);
        return { message: "x", service: "ledger" } as never;
      },
      status: 504,
      code: "dependency_timeout",
    },
    {
      name: "argumento inválido",
      behavior: () => {
        throw new ConnectError("message excede 256 caracteres", Code.InvalidArgument);
      },
      status: 400,
      code: "invalid_argument",
    },
    {
      name: "erro interno do ledger",
      behavior: () => {
        throw new ConnectError("stack secreta do ledger", Code.Internal);
      },
      status: 502,
      code: "dependency_error",
    },
    {
      name: "exceção qualquer",
      behavior: () => {
        throw new Error("segredo interno");
      },
      status: 502,
      code: "dependency_error",
    },
  ];

  it.each(cases)("$name -> $status $code", async ({ behavior, status, code }) => {
    h = createHarness(behavior);
    const res = await h.app.inject({ url: "/ping?message=x" });

    expect(res.statusCode).toBe(status);
    expect(res.json()).toMatchObject({ code, status });
    // Nada de interno vaza ao cliente.
    expect(res.body).not.toContain("secreta");
    expect(res.body).not.toContain("segredo");
    // O problem details carrega o trace_id, que liga a resposta ao trace no Grafana.
    const serverSpan = h.spanNamed("GET /ping");
    expect(res.json<{ trace_id: string }>().trace_id).toBe(serverSpan.spanContext().traceId);
  });

  it("503 manda Retry-After", async () => {
    h = createHarness(() => {
      throw new ConnectError("down", Code.Unavailable);
    });
    const res = await h.app.inject({ url: "/ping" });
    expect(res.headers["retry-after"]).toBe("1");
  });

  it("repassa o motivo da recusa só quando é erro do cliente (400)", async () => {
    h = createHarness(() => {
      throw new ConnectError("message excede 256 caracteres", Code.InvalidArgument);
    });
    const res = await h.app.inject({ url: "/ping" });
    expect(res.json<{ detail: string }>().detail).toContain("excede 256");
  });
});

describe("traces", () => {
  it("encadeia span de servidor e de cliente e propaga o traceparent ao ledger", async () => {
    h = createHarness();
    await h.app.inject({ url: "/ping?message=oi" });

    const server = h.spanNamed("GET /ping");
    const client = h.spanNamed("conta.ping.v1.PingService/Ping");

    expect(server.kind).toBe(SpanKind.SERVER);
    expect(server.attributes).toMatchObject({
      "http.request.method": "GET",
      "http.route": "/ping",
      "http.response.status_code": 200,
    });
    expect(client.kind).toBe(SpanKind.CLIENT);
    expect(client.attributes).toMatchObject({
      "rpc.system.name": "grpc",
      "rpc.method": "conta.ping.v1.PingService/Ping",
      "peer.service": "ledger",
      "rpc.response.status_code": "OK",
    });
    // Mesmo trace; o cliente é filho do servidor.
    expect(client.spanContext().traceId).toBe(server.spanContext().traceId);
    expect(client.parentSpanContext?.spanId).toBe(server.spanContext().spanId);

    // O ledger recebeu o traceparent, com o span de cliente como pai.
    const traceparent = h.ledgerRequests[0]?.get("traceparent");
    expect(traceparent).toBe(
      `00-${client.spanContext().traceId}-${client.spanContext().spanId}-01`,
    );
  });

  it("não continua o trace de quem chamou: abre um trace novo e guarda o contexto como link", async () => {
    h = createHarness();
    const forgedTrace = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
    const forgedSpan = "bbbbbbbbbbbbbbbb";
    const res = await h.app.inject({
      url: "/ping",
      headers: { traceparent: `00-${forgedTrace}-${forgedSpan}-01` },
    });

    const server = h.spanNamed("GET /ping");
    expect(server.spanContext().traceId).not.toBe(forgedTrace);
    expect(server.parentSpanContext).toBeUndefined(); // raiz
    expect(server.links).toHaveLength(1);
    expect(server.links[0]?.context).toMatchObject({ traceId: forgedTrace, spanId: forgedSpan });
    // O cliente acha o trace pelo header, sem precisar enviar um traceparent.
    expect(res.headers["x-trace-id"]).toBe(server.spanContext().traceId);
  });

  it("marca o span de cliente com erro e o status gRPC quando o ledger falha", async () => {
    h = createHarness(() => {
      throw new ConnectError("down", Code.Unavailable);
    });
    await h.app.inject({ url: "/ping" });

    const client = h.spanNamed("conta.ping.v1.PingService/Ping");
    expect(client.status.code).toBe(SpanStatusCode.ERROR);
    expect(client.attributes["rpc.response.status_code"]).toBe("UNAVAILABLE");
    expect(client.events.some((e) => e.name === "exception")).toBe(true);
  });

  it("identifica a instância no recurso (réplicas não colidem no Prometheus)", async () => {
    h = createHarness();
    await h.app.inject({ url: "/ping" });
    expect(h.spanNamed("GET /ping").resource.attributes).toMatchObject({
      "service.name": "bff",
      "service.instance.id": "bff-test-1",
    });
  });

  it("a sonda de saúde não gera span", async () => {
    h = createHarness();
    await h.app.inject({ url: "/healthz" });
    await h.app.inject({ url: "/readyz" });
    expect(h.spans.getFinishedSpans()).toHaveLength(0);
  });
});

describe("logs", () => {
  it("um log por requisição, com trace_id e span_id do span do servidor", async () => {
    h = createHarness();
    await h.app.inject({ url: "/ping?message=oi" });

    const server = h.spanNamed("GET /ping");
    const done = h.stdout().filter((l) => l.msg === "request completed");
    expect(done).toHaveLength(1);
    expect(done[0]).toMatchObject({
      service: "bff",
      method: "GET",
      route: "/ping",
      status: 200,
      trace_id: server.spanContext().traceId,
      span_id: server.spanContext().spanId,
    });
  });

  it("o mesmo log é exportado ao OpenTelemetry, ligado ao trace", async () => {
    h = createHarness();
    await h.app.inject({ url: "/ping" });

    const server = h.spanNamed("GET /ping");
    const rec = h.logs.getFinishedLogRecords().find((r) => r.body === "request completed");
    expect(rec?.spanContext?.traceId).toBe(server.spanContext().traceId);
  });

  it("erro 5xx é logado como erro; erro de cliente não", async () => {
    h = createHarness(() => {
      throw new ConnectError("down", Code.Unavailable);
    });
    await h.app.inject({ url: "/ping" });
    expect(h.stdout().find((l) => l.msg === "request completed")).toMatchObject({
      level: 50,
      status: 503,
    });
  });
});

describe("métricas", () => {
  it("RED por rota: duração com método, rota e status", async () => {
    h = createHarness();
    await h.app.inject({ url: "/ping" });

    const m = await h.metric("http.server.request.duration");
    expect(m?.dataPoints[0]?.attributes).toEqual({
      "http.request.method": "GET",
      "http.route": "/ping",
      "http.response.status_code": 200,
    });
  });

  it("latência e erro por dependência", async () => {
    h = createHarness(() => {
      throw new ConnectError("down", Code.Unavailable);
    });
    await h.app.inject({ url: "/ping" });

    const m = await h.metric("rpc.client.call.duration");
    expect(m?.dataPoints[0]?.attributes).toMatchObject({
      dependency: "ledger",
      "rpc.system.name": "grpc",
      "rpc.method": "conta.ping.v1.PingService/Ping",
      "rpc.response.status_code": "UNAVAILABLE",
    });
  });

  it("rota inexistente usa o rótulo fixo 'unmatched', não o caminho cru", async () => {
    h = createHarness();
    const res = await h.app.inject({ url: "/nao/existe/123" });
    expect(res.statusCode).toBe(404);
    expect(res.json()).toMatchObject({ code: "not_found" });

    const m = await h.metric("http.server.request.duration");
    expect(m?.dataPoints[0]?.attributes["http.route"]).toBe("unmatched");
  });

  it("métricas de recurso (USE): atraso do event loop e heap", async () => {
    h = createHarness();
    expect(await h.metric("nodejs.heap.used")).toBeDefined();
    expect(await h.metric("nodejs.eventloop.delay.p99")).toBeDefined();
  });
});

describe("saúde e erros inesperados", () => {
  it("/healthz sempre 200; /readyz vira 503 no desligamento", async () => {
    h = createHarness();
    expect((await h.app.inject({ url: "/healthz" })).statusCode).toBe(200);
    expect((await h.app.inject({ url: "/readyz" })).statusCode).toBe(200);

    h.built.lifecycle.markShuttingDown();
    expect((await h.app.inject({ url: "/healthz" })).statusCode).toBe(200);
    const readyz = await h.app.inject({ url: "/readyz" });
    expect(readyz.statusCode).toBe(503);
    expect(readyz.json()).toEqual({ status: "shutting_down" });
  });

  it("erro não tratado vira 500 genérico, com o detalhe só no log", async () => {
    h = createHarness();
    h.app.get("/boom", () => {
      throw new Error("segredo interno");
    });
    const res = await h.app.inject({ url: "/boom" });

    expect(res.statusCode).toBe(500);
    expect(res.json()).toMatchObject({ code: "internal_error" });
    expect(res.body).not.toContain("segredo");
    expect(h.stdout().some((l) => l.msg === "erro não tratado")).toBe(true);
    expect(h.spanNamed("GET /boom").status.code).toBe(SpanStatusCode.ERROR);
  });
});
