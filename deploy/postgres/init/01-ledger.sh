#!/bin/sh
# Roda uma única vez, na primeira inicialização do volume (docker-entrypoint-initdb.d).
# Mudou este arquivo? Recrie só o volume do banco (o -v do down apagaria também o cache do Go):
#   docker compose --profile core down && docker volume rm conta-digital_postgres-data
#
# Um banco por serviço, com dois papéis (ADR-0004, princípio do menor privilégio):
#   ledger_owner  dono do banco e do schema; roda as migrations (DDL)
#   ledger_app    usado pelo serviço; só os privilégios que cada migration concede
# O superusuário (POSTGRES_USER) fica só para administração.
set -eu

: "${LEDGER_DB_OWNER_PASSWORD:?defina LEDGER_DB_OWNER_PASSWORD no .env}"
: "${LEDGER_DB_APP_PASSWORD:?defina LEDGER_DB_APP_PASSWORD no .env}"

# As senhas entram como variáveis do psql (:'nome'), que o psql escapa como literal SQL.
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres \
  -v owner_password="$LEDGER_DB_OWNER_PASSWORD" \
  -v app_password="$LEDGER_DB_APP_PASSWORD" <<'SQL'
CREATE ROLE ledger_owner LOGIN PASSWORD :'owner_password';
CREATE ROLE ledger_app LOGIN PASSWORD :'app_password';

CREATE DATABASE ledger OWNER ledger_owner;
-- Por padrão qualquer papel conecta em qualquer banco; aqui só os dois do ledger.
REVOKE ALL ON DATABASE ledger FROM PUBLIC;
GRANT CONNECT ON DATABASE ledger TO ledger_app;
SQL

# No Postgres 15+ o schema public pertence ao dono do banco (pg_database_owner) e PUBLIC não
# tem CREATE nele; ledger_app herda só USAGE. Nada mais a ajustar no schema.
