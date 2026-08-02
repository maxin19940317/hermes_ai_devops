# Hermes DevOps Runtime deployment

日常更新/回滚/排障命令见 [RUNBOOK.md](RUNBOOK.md)。

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

Artifact downloads support three auth modes via `ARTIFACT_AUTH_TYPE`: `basic`
(recommended, design principle 5 — a read-only GitLab Deploy Token in
`ARTIFACT_AUTH_USERNAME` / `ARTIFACT_AUTH_TOKEN`, sent as HTTP Basic auth),
`bearer` (a GitLab PAT in `ARTIFACT_AUTH_TOKEN`), and `job_token` (CI job tokens
only; sending a PAT in a `JOB-TOKEN` header fails with 401). GitLab Deploy Tokens
support HTTP Basic auth only — they cannot be used with `bearer`.

Feishu notifications are dual-mode: when `FEISHU_APP_ID` / `FEISHU_APP_SECRET` /
`FEISHU_RECEIVE_ID` are all set, the worker sends via the enterprise custom app bot
(`im/v1/messages`, tenant token cached with a 5-minute refresh margin and one
forced refresh on expiry errors); otherwise it falls back to the group custom
bot webhook (`FEISHU_WEBHOOK_URL`). `FEISHU_RECEIVE_ID` accepts an `open_id`
(personal DM, set `FEISHU_RECEIVE_ID_TYPE=open_id`) or a `chat_id` (group chat,
the default when the type is unset). All empty means notifications are silently
skipped (dev mode). Terminal-state notifications render as an interactive card
(2026-07-30): the header background is green/red/orange by verdict — red whenever
any task has a non-`INFRA_ERROR` failure (business failure takes priority even if
`INFRA_ERROR` is also present), orange when every failure is `INFRA_ERROR`, green
when nothing is judged a failure. If the card send fails, or the configured sender
doesn't support cards, the worker falls back to the same plain-text message this
version used to send unconditionally. This milestone ships display only — the
card has no buttons or other interactive components. The worker also runs an
optional command listener over the
app's WebSocket event subscription: when `FEISHU_CMD_WHITELIST` (comma-separated
open_ids) is set, whitelisted users can send the bot DM commands (`status`,
`devices`, `rerun <source_workflow_id> [variant]`, `unquarantine [device_id]`);
messages from anyone else are silently ignored.

`rerun` accepts only an authoritative, closed source recorded in `workflow_runs`.
Without a variant it retries only source-output entries satisfying
`verdict != PASSED && verdict != SKIPPED`; an explicit variant remains allowed when it
belongs to the source run, including PASSED or SKIPPED. Legacy rows returned by
`RecentRuns` are display-only and cannot be rerun. Each direct text command allocates a
fresh attempt and workflow ID. Temporal duplicate rejection is therefore not an
idempotency mechanism for repeated commands; persistent action claims belong to the
subsequent interactive-button round.

### 飞书指令自然语言翻译(可选)

`FEISHU_CMD_NL=true` 后,不在 `status|devices|rerun|unquarantine` 里的输入会经
hermes-agent 翻译成一条指令再执行。启用前置条件(三者合取):

1. `FEISHU_CMD_WHITELIST` 非空(指令 listener 本身已启用)
2. `HERMES_ENDPOINT` 非空
3. **analyze_bridge 已部署 `/translate` 路由** —— 它不在本 compose 内,由
   hermes-agent 实例内的 `start-analyze-bridge` 启停。先
   `curl -X POST -H "Authorization: Bearer $HERMES_AUTH_TOKEN" .../translate`
   确认路由存在,否则全部自然语言请求 502(手打指令不受影响)。

行为要点:只读指令(status/devices)直接执行;副作用指令(rerun/unquarantine)
先回执待确认,回复 `y` 执行、`n` 取消,120 秒过期。每次翻译在
`command_translations` 表留痕。

## MinIO evidence uploads

The `minio` service stores run evidence (result.json, junit.xml, logcat.txt,
stdout.log, stderr.log) uploaded directly by Clients against presigned PUT URLs the
worker signs at dispatch time (design §3.7). `minio-init` is a one-shot container that
creates the bucket (`MINIO_BUCKET`, default `hermes-evidence`) once `minio` is healthy
and installs the bucket's lifecycle rules.

### Retention (lifecycle rules)

The bucket holds two object classes with deliberately different retention, so the
rules are keyed on prefix:

| Prefix | Contents | Retention |
|---|---|---|
| `runs/{task_id}/` | raw attachments (logcat, stdout/stderr, junit, dumps) — the bulky ones | expire after `MINIO_RUNS_RETAIN_DAYS` (default 90) |
| `evidence/{task_id}/` | evidence snapshots (≤96KB) | **no expiry** — they are the replay record behind a `decisions` row |

`minio-init` rebuilds the whole lifecycle configuration on every run (`mc ilm rule rm
--all` then `mc ilm rule add`), so it is idempotent and changing
`MINIO_RUNS_RETAIN_DAYS` takes effect by re-running the container:

```bash
docker compose --env-file .env run --rm minio-init
mc ilm rule ls hermes/hermes-evidence   # or read minio-init's own log line
```

Deliberately not implemented: the finer "PASSED logs 7 days, failures 90 days" split
named in CLAUDE.md §12 Phase 3. Verdict is not part of the object key and is unknown
at upload time (attachments are uploaded before the rule engine runs), so an
S3 lifecycle rule cannot see it. Getting the 7-day half requires tagging each object
with its verdict once the task reaches a terminal state and adding a tag-filtered
rule — a runtime change, not a bucket-config change. Until then `runs/` uses the
conservative bound (90 days), which keeps failure evidence for its full window at the
cost of keeping passing runs' logs longer than necessary.

Key environment variables (see `.env.example`):

- `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD` — root credentials; the password is a
  required secret in `deploy/.env` (validated by `validate-env.sh`). The worker reuses
  them as `MINIO_ACCESS_KEY` / `MINIO_SECRET_KEY`.
- `MINIO_ENDPOINT=minio:9000` (compose-internal) and `MINIO_PUBLIC_ENDPOINT` — the host
  embedded in presigned URLs; it must be Client-reachable (LAN address) because the
  signature covers the Host header and cannot be rewritten afterwards. If the endpoint
  or credentials are empty the worker degrades gracefully: `presigned_uploads` is empty
  and dispatch still succeeds.
- `MINIO_PRESIGN_TTL` (default `1h`) — presigned URL lifetime. Since gap #8 the normal path
  signs URLs **after** collection finishes, seconds before upload, so this no longer needs to
  exceed the longest task. It still bounds the dispatch-time fallback set, which is used when
  the agent is older than the endpoint or the endpoint is unreachable mid-run.
- `UPLOAD_REQUEST_MAX_FILES` (default `64`) — per-request file cap for `POST /callbacks/v1/upload-requests`.
  Over the cap the whole request is rejected rather than truncated, so a client never believes it
  uploaded everything when it did not.
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

## Database migrations

Runtime services apply `runtime/internal/store/schema.sql` at startup (`OpenPG`),
but only as `CREATE TABLE IF NOT EXISTS` — new columns on existing tables are NOT
added. When a release adds columns, apply the matching script in
`deploy/postgres/migrations/` before recreating services:

```bash
docker exec -i hermes-runtime-postgres-1 psql -U hermes_runtime -d hermes_runtime \
  -v ON_ERROR_STOP=1 < deploy/postgres/migrations/<file>.sql
```

### workflow_runs migration gate

`workflow_runs` is an immutable registry for new workflow inputs and is not backfilled
from legacy artifacts or tasks. Its migration changes the artifact unique key from
`(commit_sha, pipeline_id, variant)` to
`(project, commit_sha, pipeline_id, variant)`, so it is not rolling-compatible with old
artifact writers.

The already-merged presign/evidence-v3/attribution batch must be deployed and observed stable first.
Merging the workflow_runs branch does not authorize the production migration.
Stop all old artifact writers and Feishu command listeners before removing the old
unique constraint or restarting `analyze_bridge` on v2.

The mandatory production order is:

```text
prior batch stable -> stop all old artifact writers and Feishu command listeners -> migrate -> update and restart analyze_bridge on every hermes-agent host -> deploy all new binaries -> resume
```

During the stopped-ingress window, stop every old Trigger process that can insert
artifacts and every old Worker process that hosts a Feishu command listener. This stops
both artifact writes and command ingress before any v2 component starts. Then apply
`deploy/postgres/migrations/2026-07-30-workflow-runs.sql`, synchronize
`hermes/analyze_bridge` including `command.schema.json v2` to every hermes-agent host and
restart each `analyze_bridge`, then deploy all new Trigger/Worker/Relay binaries as one
release. A forward or reverse version mismatch makes the bridge reject the other side's
translation payload.
A forward or reverse v1/v2 mismatch breaks all natural-language commands.
Only resume command and artifact ingress after analyze_bridge and all new binaries are on v2.
Do not combine this window with deployment of the prerequisite batch.

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
  -f deploy/docker-compose.yml logs -f --tail 100 trigger worker relay
```

## Outbox backlog monitoring

Results reach Temporal through a transactional outbox: the worker writes the result row
and an outbox row in one transaction, and `relay` delivers the signal. A row that cannot
be delivered stays put with `attempts` and `last_error` recorded, so a growing backlog is
the signal that deliveries are failing.

`relay` reports the backlog on a timer (`RELAY_BACKLOG_INTERVAL`, default `1m`) under the
message `outbox backlog`, and picks the level so the log line can *be* the alert
condition rather than a stream someone has to watch:

| Level | When |
|---|---|
| `warn` | any row with `attempts >= RELAY_STUCK_ATTEMPTS` (default 3), **or** oldest pending row older than `RELAY_BACKLOG_WARN_AGE` (default `5m`) |
| `info` | backlog exists but nothing is stuck — delivery is in progress |
| `debug` | no backlog (healthy runs quiet) |

Fields: `pending`, `stuck`, `oldest_age`, `oldest_id`, `sample_error` (the `last_error` of
the most-retried pending row — the diagnostic entry point).

```bash
# alert on it
docker compose ... logs relay | grep '"message":"outbox backlog"' | grep '"level":"warn"'

# ad-hoc check against the database
psql "$DATABASE_URL" -c 'SELECT * FROM outbox_backlog;'
psql "$DATABASE_URL" -c "SELECT id, event_key, attempts, last_error, created_at
                         FROM outbox WHERE published_at IS NULL ORDER BY attempts DESC LIMIT 20;"
```

The `outbox_backlog` view uses a fixed `attempts >= 3` for its `stuck` column; it is the
human-facing coarse filter, while `RELAY_STUCK_ATTEMPTS` is the configurable threshold the
relay's own log uses. Set `RELAY_BACKLOG_INTERVAL=0` to turn the periodic report off.

## Upgrade

**Deployment order when a release adds a dispatch payload field** (e.g. `lease_id`,
`upload_request_url`): upgrade the Windows Client Agent first, or roll out Runtime and
Agent together. Do not upgrade Runtime alone and leave old Agents running. Agent
releases prior to the root-level `additionalProperties` relaxation in
`agent/internal/server/dispatch.schema.json` reject any dispatch payload carrying a
field they don't recognize with `400 schema_violation` — the whole dispatch fails as
INFRA, it does not degrade to ignoring the new field. The schema relaxation only
protects Agents built from it onward; already-deployed older Agents are still exposed.
See `docs/superpowers/specs/2026-07-29-on-demand-presign-design.md` §7 for the full
compatibility matrix.

Record the current image ID, update source, rebuild, and recreate only Runtime services
(if the release adds database columns, apply the migration first — see Database
migrations):

```bash
docker image inspect hermes-runtime:${RUNTIME_IMAGE_TAG:-dev} --format '{{.Id}}'
deploy/scripts/lock-images.sh deploy/.env deploy/images.lock.env
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml build trigger
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml up -d --no-deps trigger worker relay
```

## Roll back

Retag the recorded Runtime image ID and recreate Trigger/Worker/Relay. Never use `down -v` in
normal rollback because `hermes-runtime-postgres` contains Temporal and Runtime state.

```bash
test -n "${ROLLBACK_IMAGE_ID:-}" || { echo "Set ROLLBACK_IMAGE_ID first" >&2; exit 1; }
docker tag "$ROLLBACK_IMAGE_ID" hermes-runtime:${RUNTIME_IMAGE_TAG:-dev}
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml up -d --no-deps trigger worker relay
```

## Stop without deleting data

```bash
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml down
```
