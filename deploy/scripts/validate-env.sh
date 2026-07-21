#!/bin/sh
set -eu

env_file=${1:-deploy/.env}
lock_file=${2:-deploy/images.lock.env}
test -f "$env_file" || { echo "ERROR: missing $env_file" >&2; exit 1; }
test -f "$lock_file" || { echo "ERROR: run deploy/scripts/lock-images.sh first" >&2; exit 1; }

set -a
. "$env_file"
. "$lock_file"
set +a

for key in POSTGRES_ADMIN_PASSWORD RUNTIME_DB_PASSWORD GITLAB_TOKEN TRIGGER_WEBHOOK_SECRET; do
  eval "value=\${$key:-}"
  test -n "$value" || { echo "ERROR: $key is empty" >&2; exit 1; }
done

for key in POSTGRES_IMAGE TEMPORAL_IMAGE TEMPORAL_UI_IMAGE GO_IMAGE RUNTIME_BASE_IMAGE; do
  eval "value=\${$key:-}"
  case "$value" in
    *@sha256:*) ;;
    *) echo "ERROR: $key is not digest locked" >&2; exit 1 ;;
  esac
done

for spec in "TRIGGER_HOST_PORT 18090" "WORKER_CALLBACKS_HOST_PORT 18091" "TEMPORAL_UI_HOST_PORT 18080"; do
  var=${spec% *}
  def=${spec#* }
  eval "port=\${$var:-$def}"
  if ss -ltnH "sport = :$port" | grep -q .; then
    echo "ERROR: $var port $port is occupied" >&2
    exit 1
  fi
done

echo "Deployment environment is valid"
