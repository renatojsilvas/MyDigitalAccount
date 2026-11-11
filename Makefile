# Tarefas do projeto. Rodam DENTRO do contêiner tools:
#   docker compose run --rm tools make <alvo>
# Subir, parar e apagar o ambiente são comandos do docker compose (ver README).

.PHONY: generate proto-lint go-lint go-test ts-lint ts-test vuln lint test

# Recria gen/ do zero a partir de proto/ (ADR-0001). Os modelos de gen/ vêm de
# tools/templates; o package.json de gen/ts precisa existir antes do pnpm install.
generate:
	rm -rf gen
	mkdir -p gen/go gen/ts/src
	cp tools/templates/gen-go.mod gen/go/go.mod
	cp tools/templates/gen-ts-package.json gen/ts/package.json
	cp tools/templates/gen-ts-tsconfig.json gen/ts/tsconfig.json
	pnpm install --frozen-lockfile
	buf generate
	cd gen/go && go mod tidy && go build ./...
	pnpm --filter @conta/contracts build

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

# Vulnerabilidades conhecidas nas dependências (consulta o banco vuln.go.dev).
vuln:
	@for d in $(GO_MODULES); do echo "==> govulncheck $$d"; (cd $$d && govulncheck ./...) || exit 1; done

lint: generate proto-lint go-lint ts-lint

test: generate go-test ts-test
