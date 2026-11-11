# Sonda de saúde para imagens sem shell (Collector, Tempo e Loki).
#
# Essas imagens são mínimas: sem sh, wget nem curl, então um HEALTHCHECK comum é impossível.
# Copiamos um busybox estático (musl, ~1,3 MB) e o healthcheck chama `/busybox wget`. A imagem
# continua sendo a do fornecedor, com um binário a mais. BASE vem do compose (tag + digest).
ARG BASE
FROM busybox:1.37.0-musl@sha256:f6a95d611e0a8780fabdb2aec43e31275b2720dfefa3e41b16835bfc42454478 AS probe

FROM ${BASE}
COPY --from=probe /bin/busybox /busybox
