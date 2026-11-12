# syntax=docker/dockerfile:1
#
# Contexto de build: a RAIZ do repositório. Pré-requisito: `make generate` (gen/go).
# O estágio "tools" vem de um contexto adicional (a imagem tools, fonte única de versões):
#   compose:  additional_contexts: tools: docker-image://conta-digital/tools:dev
#   manual:   docker build --build-context tools=docker-image://conta-digital/tools:dev ...
#
# Alvos:  dev   -> ferramentas + air (recarga automática); o código entra por volume
#         final -> binário estático em distroless, sem root

FROM tools AS dev
WORKDIR /workspace/services/antifraude
HEALTHCHECK --interval=5s --timeout=3s --start-period=30s --retries=12 \
  CMD curl -fsS http://127.0.0.1:8082/readyz || exit 1
CMD ["air", "-c", ".air.toml"]

FROM tools AS build
ARG VERSION=dev
WORKDIR /workspace
COPY gen/go gen/go
COPY platform platform
COPY services/antifraude services/antifraude
WORKDIR /workspace/services/antifraude
RUN --mount=type=cache,target=/cache \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/antifraude ./cmd/antifraude

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329 AS final
COPY --from=build /out/antifraude /antifraude
USER nonroot:nonroot
EXPOSE 50061 8082
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=3 \
  CMD ["/antifraude", "healthcheck"]
ENTRYPOINT ["/antifraude"]
