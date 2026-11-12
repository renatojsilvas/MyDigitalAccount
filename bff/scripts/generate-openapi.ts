import { mkdir, writeFile } from "node:fs/promises";
import { dirname } from "node:path";
import { buildApp } from "../src/app.js";
import { loadConfig } from "../src/config.js";

// Gera o OpenAPI a partir das rotas, sem abrir porta nem falar com o ledger.
// Uso: tsx scripts/generate-openapi.ts <arquivo-de-saída>
const out = process.argv[2];
if (out === undefined) {
  process.stderr.write("uso: generate-openapi <arquivo-de-saída>\n");
  process.exit(2);
}

// Em development, para o documento incluir o login de demonstração. O segredo é só um
// preenchimento: nada é assinado nem validado aqui.
const config = loadConfig({
  ENVIRONMENT: "development",
  LOG_LEVEL: "silent",
  BFF_JWT_SECRET: "x".repeat(32),
});
const { app } = buildApp(config);
await app.ready();
const document = app.swagger();
await app.close();

await mkdir(dirname(out), { recursive: true });
await writeFile(out, `${JSON.stringify(document, null, 2)}\n`);
process.stdout.write(`OpenAPI gravado em ${out}\n`);
