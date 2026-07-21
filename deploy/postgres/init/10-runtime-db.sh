#!/bin/sh
set -eu

: "${RUNTIME_DB_USER:?RUNTIME_DB_USER is required}"
: "${RUNTIME_DB_PASSWORD:?RUNTIME_DB_PASSWORD is required}"

psql -v ON_ERROR_STOP=1 \
  --username "$POSTGRES_USER" \
  --dbname "$POSTGRES_DB" \
  --set=runtime_user="$RUNTIME_DB_USER" \
  --set=runtime_password="$RUNTIME_DB_PASSWORD" <<'SQL'
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', :'runtime_user', :'runtime_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'runtime_user')\gexec

SELECT format('CREATE DATABASE hermes_runtime OWNER %I', :'runtime_user')
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'hermes_runtime')\gexec
SQL
