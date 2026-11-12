import { afterEach, describe, expect, it } from "vitest";
import { createHarness, testConfig, type Harness } from "./harness.js";

let h: Harness;
afterEach(async () => {
  await h.close();
});

type Json = Record<string, unknown>;

async function openapi(config = testConfig()): Promise<Json> {
  h = createHarness(undefined, config);
  await h.app.ready();
  return h.app.swagger() as unknown as Json;
}

/** Todas as propriedades (nome → schema) de qualquer objeto do documento, em qualquer profundidade. */
function* properties(node: unknown): Generator<[string, Json]> {
  if (Array.isArray(node)) {
    for (const item of node) yield* properties(item);
  } else if (node !== null && typeof node === "object") {
    for (const [key, value] of Object.entries(node)) {
      if (key === "properties" && value !== null && typeof value === "object") {
        for (const [name, schema] of Object.entries(value as Json)) yield [name, schema as Json];
      }
      yield* properties(value);
    }
  }
}

describe("OpenAPI gerado dos schemas", () => {
  it("descreve as rotas da API e esconde as sondas de saúde", async () => {
    const doc = await openapi();
    const paths = doc.paths as Record<string, Json>;
    expect(Object.keys(paths).sort()).toEqual(["/auth/demo-login", "/home", "/ping"]);
    expect(paths["/home"]).toHaveProperty("get");
    expect(paths["/auth/demo-login"]).toHaveProperty("post");
  });

  it("declara o esquema Bearer e exige-o só na /home", async () => {
    const doc = await openapi();
    const paths = doc.paths as Record<string, Record<string, Json>>;
    expect((doc.components as Json).securitySchemes).toMatchObject({
      bearerAuth: { type: "http", scheme: "bearer", bearerFormat: "JWT" },
    });
    expect(paths["/home"]?.get?.security).toEqual([{ bearerAuth: [] }]);
    expect(paths["/auth/demo-login"]?.post?.security).toBeUndefined();
  });

  it("documenta os erros esperados da /home", async () => {
    const doc = await openapi();
    const responses = (doc.paths as Record<string, Record<string, Json>>)["/home"]?.get
      ?.responses as Json;
    expect(Object.keys(responses).sort()).toEqual(["200", "400", "401", "502", "503", "504"]);
  });

  it("o extrato é uma união: ok com itens, ou indisponível com motivo", async () => {
    const doc = await openapi();
    const ok200 = JSON.stringify(
      (doc.paths as Record<string, Record<string, Json>>)["/home"]?.get?.responses,
    );
    expect(ok200).toContain('"oneOf"');
    expect(ok200).toContain('"unavailable"');
    expect(ok200).toContain('"timeout"');
  });

  it("dinheiro é sempre inteiro: nenhum campo *Cents é number nem string", async () => {
    const doc = await openapi();
    const cents = [...properties(doc)].filter(([name]) => /cents$/i.test(name));
    expect(cents.length).toBeGreaterThan(0);
    for (const [name, schema] of cents) {
      expect(schema.type, `campo ${name}`).toBe("integer");
    }
  });

  it("sem o login de demonstração (production), a rota some do contrato", async () => {
    const doc = await openapi(testConfig({ ENVIRONMENT: "production" }));
    expect(Object.keys(doc.paths as Json)).not.toContain("/auth/demo-login");
  });

  it("os erros são descritos como application/problem+json; os sucessos, como application/json", async () => {
    const doc = await openapi();
    const paths = doc.paths as Record<string, Record<string, { responses: Record<string, Json> }>>;
    let errors = 0;
    for (const [path, methods] of Object.entries(paths)) {
      for (const [method, op] of Object.entries(methods)) {
        for (const [status, response] of Object.entries(op.responses)) {
          const types = Object.keys(response.content ?? {});
          const expected = status.startsWith("2")
            ? ["application/json"]
            : ["application/problem+json"];
          expect(types, `${method.toUpperCase()} ${path} ${status}`).toEqual(expected);
          if (!status.startsWith("2")) errors++;
        }
      }
    }
    expect(errors).toBeGreaterThan(0);
  });

  it("o content-type documentado é o que o BFF de fato devolve", async () => {
    h = createHarness(undefined, testConfig(), { ledger: {} });
    const res = await h.app.inject({ url: "/home" }); // sem token: 401
    const doc = h.app.swagger() as unknown as {
      paths: Record<string, Record<string, { responses: Record<string, { content: Json }> }>>;
    };
    const documented = Object.keys(doc.paths["/home"]?.get?.responses["401"]?.content ?? {});
    expect(res.statusCode).toBe(401);
    expect(documented).toEqual(["application/problem+json"]);
    expect(res.headers["content-type"]).toContain(documented[0]);
  });
});
