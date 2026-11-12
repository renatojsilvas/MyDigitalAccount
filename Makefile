# Tarefas do projeto. Rodam DENTRO do contêiner tools:
#   docker compose run --rm tools make <alvo>
# Subir, parar e apagar o ambiente são comandos do docker compose (ver README).

# Serviços com persistência: o sqlc gera o código Go das queries (fora do Git).
SQLC_CONFIGS := $(wildcard services/*/sqlc.yaml)

.PHONY: generate clean-gen proto-lint go-lint go-test ts-lint ts-test vuln obs-smoke obs-e2e lint test

# Gera gen/ a partir de proto/ (ADR-0001), SOBRESCREVENDO os arquivos no lugar. Os modelos de
# gen/ vêm de tools/templates; o package.json de gen/ts precisa existir antes do pnpm install.
#
# Não apaga gen/ de propósito: com o ambiente de desenvolvimento no ar, o tsx (BFF) e o air
# (ledger) observam esses diretórios. Se eles forem apagados e recriados, os monitores perdem o
# diretório, os serviços caem por faltar o código gerado e não voltam. Sobrescrever gera
# eventos de escrita comuns e os dois se recompilam sozinhos. Para uma geração do zero (por
# exemplo, depois de remover um .proto), use `make clean-gen generate`. O CI parte de um
# checkout limpo, então já é do zero.
generate:
	mkdir -p gen/go gen/ts/src
	cp tools/templates/gen-go.mod gen/go/go.mod
	cp tools/templates/gen-ts-package.json gen/ts/package.json
	cp tools/templates/gen-ts-tsconfig.json gen/ts/tsconfig.json
	pnpm install --frozen-lockfile
	buf generate
	cd gen/go && go mod tidy && go build ./...
	pnpm --filter @conta/contracts build
	@for f in $(SQLC_CONFIGS); do echo "==> sqlc generate $$f"; sqlc generate -f $$f || exit 1; done

# Apaga o código gerado. Pare os serviços de desenvolvimento antes, ou reinicie-os depois.
clean-gen:
	rm -rf gen

proto-lint:
	buf lint
	buf format -d --exit-code

# Módulos Go escritos à mão: serviços e testkit (gen/go é código gerado e fica de fora).
GO_MODULES := $(patsubst %/go.mod,%,$(wildcard services/*/go.mod testkit/go.mod))

go-lint:
	@for d in $(GO_MODULES); do echo "==> golangci-lint $$d"; (cd $$d && golangci-lint run ./...) || exit 1; done

go-test:
	@for d in $(GO_MODULES); do echo "==> go test -race $$d"; (cd $$d && go test -race -count=1 ./...) || exit 1; done

# TypeScript: cada pacote do workspace com script `lint`/`test` (gen/ts não tem: não se faz
# lint de código gerado). O `lint` do BFF roda ESLint, Prettier e o typecheck.
ts-lint:
	pnpm -r --workspace-concurrency=1 run typecheck
	pnpm -r --workspace-concurrency=1 run lint

ts-test:
	pnpm -r --workspace-concurrency=1 run test

# Confere que traces, métricas e logs chegam a Tempo, Prometheus e Loki. Exige o profile obs
# no ar; por isso não faz parte de `make test`.
obs-smoke:
	sh deploy/obs/smoke.sh

# Critério de pronto da Fase 0: um /ping do BFF ao ledger é um único trace, ligado aos logs.
# Exige core e obs no ar, com os serviços exportando para o Collector.
obs-e2e:
	sh deploy/obs/e2e-trace.sh

# Vulnerabilidades conhecidas nas dependências (consulta o banco vuln.go.dev).
vuln:
	@for d in $(GO_MODULES); do echo "==> govulncheck $$d"; (cd $$d && govulncheck ./...) || exit 1; done

lint: generate proto-lint go-lint ts-lint

test: generate go-test ts-test
