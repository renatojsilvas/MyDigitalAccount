#!/bin/sh
# Provisiona, de forma idempotente, um banco e dois papéis por serviço (ADR-0004, princípio do
# menor privilégio). Roda como job do compose (postgres-bootstrap) a cada `up`, antes dos jobs
# de migration, e é o único lugar com a credencial de superusuário além do próprio Postgres.
#
#   <svc>_owner  dono do banco e do schema; roda as migrations (DDL)
#   <svc>_app    usado pelo serviço; só os privilégios que cada migration concede
#
# Entrada (ambiente):
#   BOOTSTRAP_SERVICES                 lista de serviços, ex.: "ledger spi"
#   <SVC>_DB_OWNER_PASSWORD            senha do <svc>_owner (ex.: LEDGER_DB_OWNER_PASSWORD)
#   <SVC>_DB_APP_PASSWORD              senha do <svc>_app
#   PGHOST, PGPORT, PGUSER, PGPASSWORD superusuário, lido pelo psql
#
# Idempotência: papéis e bancos só são criados se faltarem no catálogo. CREATE DATABASE não
# roda dentro de transação nem de bloco DO, então o comando é montado por SELECT e executado
# com \gexec só quando a consulta devolve linha. A senha é reaplicada a cada execução (ALTER
# ROLE): trocar a senha no .env passa a valer no próximo `up`, sem recriar o volume.
set -eu

: "${BOOTSTRAP_SERVICES:?defina BOOTSTRAP_SERVICES (ex.: \"ledger spi\")}"

for svc in $BOOTSTRAP_SERVICES; do
  case "$svc" in
    *[!a-z0-9_]* | "") echo "bootstrap: nome de serviço inválido: '$svc'" >&2; exit 1 ;;
  esac
  upper=$(echo "$svc" | tr '[:lower:]' '[:upper:]')
  owner_password=$(printenv "${upper}_DB_OWNER_PASSWORD" || true)
  app_password=$(printenv "${upper}_DB_APP_PASSWORD" || true)
  if [ -z "$owner_password" ] || [ -z "$app_password" ]; then
    echo "bootstrap: defina ${upper}_DB_OWNER_PASSWORD e ${upper}_DB_APP_PASSWORD" >&2
    exit 1
  fi

  # Nomes e senhas entram como variáveis do psql: :"x" escapa como identificador, :'x' como
  # literal, e format(%I) escapa dentro do SQL montado. Nada é interpolado pelo shell.
  psql -v ON_ERROR_STOP=1 --no-psqlrc --quiet --dbname postgres \
    -v db="$svc" -v owner="${svc}_owner" -v app="${svc}_app" \
    -v owner_password="$owner_password" -v app_password="$app_password" <<'SQL'
SELECT format('CREATE ROLE %I LOGIN', :'owner')
 WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = :'owner') \gexec
SELECT format('CREATE ROLE %I LOGIN', :'app')
 WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = :'app') \gexec
ALTER ROLE :"owner" WITH LOGIN PASSWORD :'owner_password';
ALTER ROLE :"app" WITH LOGIN PASSWORD :'app_password';

SELECT format('CREATE DATABASE %I OWNER %I', :'db', :'owner')
 WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = :'db') \gexec
-- Por padrão qualquer papel conecta em qualquer banco; aqui só os dois do serviço. REVOKE e
-- GRANT repetidos não mudam nada.
REVOKE ALL ON DATABASE :"db" FROM PUBLIC;
GRANT CONNECT ON DATABASE :"db" TO :"app";
SQL
  # No Postgres 15+ o schema public pertence ao dono do banco (pg_database_owner) e PUBLIC não
  # tem CREATE nele; o <svc>_app herda só USAGE. Nada mais a ajustar no schema.
  echo "bootstrap: $svc pronto (banco $svc, papéis ${svc}_owner e ${svc}_app)"
done
