# syntax=docker/dockerfile:1
#
# Front-end de demonstração: só o alvo de desenvolvimento (Vite com recarga automática, o código
# entra por volume). Não há imagem final: o front só existe para ver o sistema funcionando
# (plano, seção 6.1) e o proxy /api -> BFF é do servidor do Vite. Contexto adicional `tools`:
#   compose: additional_contexts: tools: docker-image://conta-digital/tools:dev
# Pré-requisito: `make generate` (tipos em web/src/api/schema.d.ts).

FROM tools AS dev
WORKDIR /workspace/web
EXPOSE 5173
HEALTHCHECK --interval=5s --timeout=3s --start-period=40s --retries=12 \
  CMD curl -fsS http://127.0.0.1:5173/ || exit 1
CMD ["pnpm", "dev"]
