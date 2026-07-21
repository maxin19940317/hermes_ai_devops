#!/bin/sh
set -eu

env_file=${1:-deploy/.env}
lock_file=${2:-deploy/images.lock.env}
project_id=${PROJECT_ID:-651}
pipeline_id=${PIPELINE_GLOBAL_ID:-656}
trigger_url=${TRIGGER_URL:-http://127.0.0.1:18090}

set -a
. "$env_file"
. "$lock_file"
set +a

for cmd in curl jq docker; do
  command -v "$cmd" >/dev/null || { echo "ERROR: missing $cmd" >&2; exit 1; }
done

compose() {
  docker compose --env-file "$env_file" --env-file "$lock_file" \
    -f deploy/docker-compose.yml "$@"
}

curl -fsS "$trigger_url/healthz" | grep -qx ok
curl -fsS "http://127.0.0.1:${WORKER_CALLBACKS_HOST_PORT:-18091}/healthz" | grep -qx ok

project=$(curl -fsS \
  --header "$GITLAB_TOKEN_HEADER: $GITLAB_TOKEN" \
  "$GITLAB_BASE_URL/api/v4/projects/$project_id")
pipeline=$(curl -fsS \
  --header "$GITLAB_TOKEN_HEADER: $GITLAB_TOKEN" \
  "$GITLAB_BASE_URL/api/v4/projects/$project_id/pipelines/$pipeline_id")

project_path=$(printf '%s' "$project" | jq -er '.path_with_namespace')
sha=$(printf '%s' "$pipeline" | jq -er '.sha')
ref=$(printf '%s' "$pipeline" | jq -er '.ref')
status=$(printf '%s' "$pipeline" | jq -er '.status')
test "$status" = success || { echo "ERROR: pipeline $pipeline_id is $status" >&2; exit 1; }

payload=$(jq -n \
  --argjson project_id "$project_id" \
  --arg project_path "$project_path" \
  --argjson pipeline_id "$pipeline_id" \
  --arg sha "$sha" \
  --arg ref "$ref" \
  '{object_kind:"pipeline",object_attributes:{id:$pipeline_id,ref:$ref,tag:false,sha:$sha,status:"success"},project:{id:$project_id,path_with_namespace:$project_path}}')

response_file=$(mktemp)
trap 'rm -f "$response_file"' EXIT
http_status=$(curl -sS -o "$response_file" -w '%{http_code}' \
  -X POST -H 'Content-Type: application/json' \
  -H "X-Gitlab-Token: $TRIGGER_WEBHOOK_SECRET" \
  --data-binary "$payload" "$trigger_url/webhooks/gitlab")
test "$http_status" = 202 || {
  echo "ERROR: webhook returned HTTP $http_status" >&2
  sed -n '1,5p' "$response_file" >&2
  exit 1
}
workflow_id=$(jq -er '.workflow_id' "$response_file")

short_sha=$(printf '%.8s' "$sha")
artifact_count=$(compose exec -T postgres psql -At \
  -U "$POSTGRES_ADMIN_USER" -d hermes_runtime \
  -c "SELECT count(*) FROM artifacts WHERE commit_sha='$short_sha';")
test "$artifact_count" = 8 || {
  echo "ERROR: artifact count is $artifact_count, want 8" >&2
  exit 1
}

compose exec -T temporal tctl --address temporal:7233 \
  workflow describe --workflow_id "$workflow_id" >/dev/null
compose exec -T temporal tctl --address temporal:7233 \
  taskqueue describe --taskqueue device-test >/dev/null

second_status=$(curl -sS -o "$response_file" -w '%{http_code}' \
  -X POST -H 'Content-Type: application/json' \
  -H "X-Gitlab-Token: $TRIGGER_WEBHOOK_SECRET" \
  --data-binary "$payload" "$trigger_url/webhooks/gitlab")
test "$second_status" = 202
jq -e '.started == false' "$response_file" >/dev/null

echo "PASS: pipeline $pipeline_id -> $workflow_id, 8 artifacts, duplicate suppressed"
