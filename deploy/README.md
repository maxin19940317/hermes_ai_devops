# Hermes DevOps Runtime deployment

This Compose project is independent from `/opt/hermes`. It must not stop, rename,
or reconfigure the existing Hermes Agent containers or the process using host port 8090.

## Security boundary

This is a q-uat integration deployment. Trigger is plain HTTP protected by the GitLab
Webhook Secret Token. Per design decision 2 (UAT LAN exposure), the worker callbacks
port (18091) and the MinIO API port (9000) bind to the test subnet
(`${WORKER_CALLBACKS_BIND_IP:-0.0.0.0}` / `${MINIO_BIND_IP:-0.0.0.0}`) so the Windows
Client on the same subnet can reach them. Both are plain HTTP without mTLS — this
exposure is test-subnet-only and must not extend beyond it until Phase 3 lands mTLS.
The MinIO console (9001) and Temporal UI (18080) stay localhost-pinned.

`CALLBACK_BASE_URL` now points at the server LAN address (`http://10.88.118.251:18091`
in `.env.example`): the Runtime hands this URL to Clients as `callback_base_url`, and
Clients POST callbacks to it. Keep it aligned with the actual bind address in
`deploy/.env`.

## Networking

The `hermes-runtime` bridge network uses an explicit subnet
(`RUNTIME_SUBNET`, default `172.31.240.0/24`). Do not leave it to Docker's
auto-assignment: an auto-assigned range once hijacked the host route for real
`172.22.0.0/16` devices on the corporate network. If the pinned range ever
conflicts, change `RUNTIME_SUBNET` in `deploy/.env` and recreate the stack
(`down` then `up -d`; named volumes persist).

Artifact downloads use `ARTIFACT_AUTH_TYPE=bearer` by default (a GitLab PAT in
`ARTIFACT_AUTH_TOKEN`). `job_token` is only for CI job tokens; sending a PAT in a
`JOB-TOKEN` header fails with 401.

## MinIO evidence uploads

The `minio` service stores run evidence (result.json, junit.xml, logcat.txt,
stdout.log, stderr.log) uploaded directly by Clients against presigned PUT URLs the
worker signs at dispatch time (design §3.7). `minio-init` is a one-shot container that
creates the bucket (`MINIO_BUCKET`, default `hermes-evidence`) once `minio` is healthy.

Key environment variables (see `.env.example`):

- `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD` — root credentials; the password is a
  required secret in `deploy/.env` (validated by `validate-env.sh`). The worker reuses
  them as `MINIO_ACCESS_KEY` / `MINIO_SECRET_KEY`.
- `MINIO_ENDPOINT=minio:9000` (compose-internal) and `MINIO_PUBLIC_ENDPOINT` — the host
  embedded in presigned URLs; it must be Client-reachable (LAN address) because the
  signature covers the Host header and cannot be rewritten afterwards. If the endpoint
  or credentials are empty the worker degrades gracefully: `presigned_uploads` is empty
  and dispatch still succeeds.
- `MINIO_PRESIGN_TTL` (default `1h`) — presigned URL lifetime.
- `MINIO_BIND_IP` / `MINIO_HOST_PORT` (default `0.0.0.0:9000`) — API exposure;
  `MINIO_CONSOLE_PORT` (default `9001`) is published on `127.0.0.1` only.

Presigned URLs carry signatures — the worker logs object keys only, never URLs.

## Configure

```bash
cp deploy/.env.example deploy/.env
chmod 0600 deploy/.env
# Fill secrets locally; never print or commit this file.
deploy/scripts/lock-images.sh deploy/.env deploy/images.lock.env
deploy/scripts/validate-env.sh deploy/.env deploy/images.lock.env
```

Note: `deploy/postgres/init/10-runtime-db.sh` runs only on the first initialization of the
`hermes-runtime-postgres` volume. Changing `RUNTIME_DB_PASSWORD` afterwards does not update
the existing role; rotate it manually with `ALTER ROLE hermes_runtime PASSWORD ...` via
`docker compose exec postgres psql`, or recreate the volume if no state is worth keeping.

## Start

```bash
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml up -d --build
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml ps
```

## Verify

```bash
curl -fsS http://127.0.0.1:18090/healthz
curl -fsS http://127.0.0.1:18091/healthz
deploy/scripts/verify-pipeline.sh deploy/.env deploy/images.lock.env
```

Temporal UI is localhost-only at `http://127.0.0.1:18080`. Access it remotely with:

```bash
test -n "${Q_UAT_HOST:-}" || { echo "Set Q_UAT_HOST first" >&2; exit 1; }
ssh -L 18080:127.0.0.1:18080 "$Q_UAT_HOST"
```

## Logs

```bash
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml logs -f --tail 100 trigger worker
```

## Upgrade

Record the current image ID, update source, rebuild, and recreate only Runtime services:

```bash
docker image inspect hermes-runtime:${RUNTIME_IMAGE_TAG:-dev} --format '{{.Id}}'
deploy/scripts/lock-images.sh deploy/.env deploy/images.lock.env
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml build trigger
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml up -d --no-deps trigger worker
```

## Roll back

Retag the recorded Runtime image ID and recreate Trigger/Worker. Never use `down -v` in
normal rollback because `hermes-runtime-postgres` contains Temporal and Runtime state.

```bash
test -n "${ROLLBACK_IMAGE_ID:-}" || { echo "Set ROLLBACK_IMAGE_ID first" >&2; exit 1; }
docker tag "$ROLLBACK_IMAGE_ID" hermes-runtime:${RUNTIME_IMAGE_TAG:-dev}
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml up -d --no-deps trigger worker
```

## Stop without deleting data

```bash
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml down
```
