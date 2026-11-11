#!/bin/sh
# Roda uma única vez, na primeira inicialização do volume (docker-entrypoint-initdb.d).
# Um banco por serviço: nenhum serviço acessa o banco de outro.
# Os bancos de transfers e reconciler são criados nas fases em que esses serviços nascem.
set -eu

for db in ledger; do
  psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres \
    -c "CREATE DATABASE \"$db\""
done
