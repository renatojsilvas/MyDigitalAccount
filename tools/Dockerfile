# Imagem `tools`: única fonte das versões de ferramenta do projeto (ADR-0001/0003).
# Uso: docker compose run --rm tools <comando>

ARG GO_VERSION=1.27.4
ARG NODE_VERSION=24.22.2

# Fonte do Node: copiamos só o runtime da imagem oficial, em vez de instalar via apt
# (que traria uma versão antiga e não fixável).
FROM node:${NODE_VERSION}-bookworm-slim AS node

# Estágio de compilação das ferramentas Go. `go install` com versão fixa é
# independente de arquitetura (arm64 no Mac, amd64 no CI) e não deixa o compilador
# de fora da imagem final por acidente: só os binários são copiados adiante.
FROM golang:${GO_VERSION}-bookworm AS gotools
ARG BUF_VERSION=v1.75.0
ARG SQLC_VERSION=v1.31.1
ARG GOOSE_VERSION=v3.30.0
ARG GRPCURL_VERSION=v1.9.4
ARG AIR_VERSION=v1.67.4
ARG GOVULNCHECK_VERSION=v1.9.0
ARG PROTOC_GEN_GO_VERSION=v1.36.12
ARG PROTOC_GEN_GO_GRPC_VERSION=v1.6.2
ARG GOLANGCI_LINT_VERSION=v2.15.0
ENV GOBIN=/out CGO_ENABLED=0
RUN go install github.com/bufbuild/buf/cmd/buf@${BUF_VERSION} \
 && go install github.com/sqlc-dev/sqlc/cmd/sqlc@${SQLC_VERSION} \
 && go install github.com/pressly/goose/v3/cmd/goose@${GOOSE_VERSION} \
 && go install github.com/fullstorydev/grpcurl/cmd/grpcurl@${GRPCURL_VERSION} \
 && go install github.com/air-verse/air@${AIR_VERSION} \
 && go install golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION} \
 && go install google.golang.org/protobuf/cmd/protoc-gen-go@${PROTOC_GEN_GO_VERSION} \
 && go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@${PROTOC_GEN_GO_GRPC_VERSION}
# golangci-lint: o projeto recomenda o binário oficial, não `go install`.
RUN curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/${GOLANGCI_LINT_VERSION}/install.sh \
    | sh -s -- -b /out ${GOLANGCI_LINT_VERSION}

FROM golang:${GO_VERSION}-bookworm AS tools
ARG PNPM_VERSION=12.18.0

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      make git jq curl ca-certificates postgresql-client redis-tools \
 && rm -rf /var/lib/apt/lists/*

COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
 && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx \
 && npm install -g pnpm@${PNPM_VERSION} \
 && npm cache clean --force

COPY --from=gotools /out/ /usr/local/bin/

# O contêiner roda com o UID/GID do usuário do host (ver compose.yaml), que não tem
# entrada em /etc/passwd. Por isso HOME e todos os caches ficam em /cache, um volume
# nomeado; o diretório é criado com 0777 porque o volume herda as permissões dele
# na primeira montagem.
RUN mkdir -m 0777 /cache /workspace \
 && git config --system --add safe.directory /workspace
ENV HOME=/cache/home \
    GOCACHE=/cache/go-build \
    GOMODCACHE=/cache/go-mod \
    GOFLAGS=-buildvcs=false \
    GOTOOLCHAIN=local \
    XDG_CACHE_HOME=/cache/xdg \
    npm_config_cache=/cache/npm \
    PNPM_HOME=/cache/pnpm \
    npm_config_store_dir=/cache/pnpm-store \
    PATH=/cache/pnpm:$PATH

WORKDIR /workspace
