# syntax=docker/dockerfile:1
#
# Contexto de build: a RAIZ do repositório. Pré-requisito: `make generate` (gen/ts).
# O estágio "tools" vem de um contexto adicional (a imagem tools, fonte única de versões):
#   compose:  additional_contexts: tools: service:tools
#   manual:   docker build --build-context tools=docker-image://conta-digital/tools:dev ...
#
# Alvos:  dev   -> ferramentas + tsx watch (recarga automática); o código entra por volume
#         final -> Node mínimo, só dependências de produção, sem root

FROM tools AS dev
WORKDIR /workspace/bff
HEALTHCHECK --interval=5s --timeout=3s --start-period=40s --retries=12 \
  CMD curl -fsS http://127.0.0.1:8088/readyz || exit 1
CMD ["pnpm", "dev"]

FROM tools AS build
WORKDIR /workspace
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY gen/ts gen/ts
COPY bff bff
RUN --mount=type=cache,target=/cache \
    pnpm install --frozen-lockfile \
 && pnpm --filter @conta/bff build \
 # deploy monta um diretório autocontido só com o BFF e suas dependências de produção
 # (inclui o pacote @conta/contracts copiado, não linkado).
 && pnpm --filter @conta/bff deploy --prod --legacy /out

FROM node:24.22.2-bookworm-slim@sha256:01d566e8d94d8c619591d78652babfccd3c6992a3dffa9ed5ab39e11cf6c70e9 AS final
ENV NODE_ENV=production
WORKDIR /app
COPY --from=build --chown=node:node /out/ ./
# A imagem oficial já traz o usuário "node" (uid 1000), sem privilégios.
USER node
EXPOSE 8088
# A imagem não tem curl: a sonda usa o fetch do próprio Node.
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=3 \
  CMD ["node", "-e", "fetch('http://127.0.0.1:'+(process.env.BFF_PORT||8088)+'/readyz').then(r=>process.exit(r.ok?0:1),()=>process.exit(1))"]
CMD ["node", "dist/main.js"]
