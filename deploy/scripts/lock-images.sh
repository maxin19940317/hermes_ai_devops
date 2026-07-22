#!/bin/sh
set -eu

env_file=${1:-deploy/.env}
lock_file=${2:-deploy/images.lock.env}

test -f "$env_file" || {
  echo "ERROR: missing $env_file; copy deploy/.env.example and fill secrets" >&2
  exit 1
}

read_value() {
  sed -n "s/^$1=//p" "$env_file" | tail -n 1
}

: >"$lock_file"
for key in POSTGRES_IMAGE TEMPORAL_IMAGE TEMPORAL_UI_IMAGE GO_IMAGE RUNTIME_BASE_IMAGE MINIO_IMAGE MINIO_MC_IMAGE; do
  ref=$(read_value "$key")
  test -n "$ref" || {
    echo "ERROR: $key is empty" >&2
    exit 1
  }
  docker pull "$ref" >/dev/null
  digest=$(docker image inspect --format '{{index .RepoDigests 0}}' "$ref")
  case "$digest" in
    *@sha256:*) printf '%s=%s\n' "$key" "$digest" >>"$lock_file" ;;
    *) echo "ERROR: no repo digest for $ref" >&2; exit 1 ;;
  esac
done
chmod 0600 "$lock_file"
echo "Wrote immutable image references to $lock_file"
