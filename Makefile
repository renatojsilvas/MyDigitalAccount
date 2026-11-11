# Tarefas do projeto. Rodam DENTRO do contêiner tools:
#   docker compose run --rm tools make <alvo>
# Subir, parar e apagar o ambiente são comandos do docker compose (ver README).

.PHONY: generate proto-lint lint test

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

lint: generate proto-lint

test: generate
