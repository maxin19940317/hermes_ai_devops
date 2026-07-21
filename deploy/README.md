# Hermes DevOps Runtime deployment

This Compose project is independent from `/opt/hermes`. It must not stop, rename,
or reconfigure the existing Hermes Agent containers or the process using host port 8090.

## Security boundary

This is a q-uat integration deployment. Trigger is plain HTTP protected by the GitLab
Webhook Secret Token. Worker callbacks bind to localhost because the callback handler does
not yet enforce the mTLS declared by the OpenAPI contract. Do not expose port 18091 until
HTTPS/mTLS or a test-subnet firewall rule is in place.

`CALLBACK_BASE_URL` defaults to `http://127.0.0.1:18091`, which is only valid while no
Client Agent exists: the Runtime hands this URL to Clients as `callback_base_url`, and a
real Client would POST callbacks to itself. When a Windows Client joins, set it to the
server LAN address in `deploy/.env` — only after the exposure conditions above are met.

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
