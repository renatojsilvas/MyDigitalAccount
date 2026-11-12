import fastifySwagger from "@fastify/swagger";
import fp from "fastify-plugin";
import { jsonSchemaTransform } from "fastify-type-provider-zod";

export interface OpenApiOptions {
  readonly version: string;
}

/**
 * OpenAPI gerado dos schemas Zod das rotas (ADR-0005): o documento nunca é escrito à mão.
 * Tem de ser registrado ANTES das rotas, porque o swagger as coleta no hook `onRoute`.
 * O documento é consumido em build (`app.swagger()`, ver scripts/generate-openapi.ts) e dele
 * saem os tipos do front; não há rota pública de documentação.
 */
export const openapiPlugin = fp<OpenApiOptions>(
  async (app, opts) => {
    await app.register(fastifySwagger, {
      openapi: {
        info: {
          title: "Conta Digital: BFF",
          description:
            "API da tela. Dinheiro é sempre inteiro em centavos. Gerado dos schemas Zod do BFF; o login de demonstração só existe fora de production.",
          version: opts.version,
        },
        tags: [
          { name: "auth", description: "Autenticação" },
          { name: "home", description: "Tela inicial" },
          { name: "diagnostics", description: "Diagnóstico" },
        ],
        components: {
          securitySchemes: {
            bearerAuth: { type: "http", scheme: "bearer", bearerFormat: "JWT" },
          },
        },
      },
      transform: jsonSchemaTransform,
    });
  },
  { name: "openapi" },
);
