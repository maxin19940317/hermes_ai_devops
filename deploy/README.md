# Hermes DevOps Runtime deployment

日常更新/回滚/排障命令见 [RUNBOOK.md](RUNBOOK.md)。

This Compose project is independent from `/opt/hermes`. It must not stop, rename,
or reconfigure the existing Hermes Agent containers or the process using host port 8090.

## Security boundary

This is a q-uat integration deployment. Trigger is plain HTTP protected by the GitLab
Webhook Secret Token. Per design decision 2 (UAT LAN exposure), the MinIO API port
(9000) stays plain HTTP bound to the test subnet (`${MINIO_BIND_IP:-0.0.0.0}`) so the
Windows Client on the same subnet can reach it. The worker callbacks port (18091) is
mTLS since 2026-08-04 (Phase 3): the server requires a client certificate signed by
the deployment CA (`scripts/generate-certs.sh`, certs in `deploy/certs/`, gitignored).
Only the callbacks direction is covered — the Runtime→Agent dispatch direction
(agent port 8480) is still plain HTTP on the test subnet.
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
version used to send unconditionally. Since 2026-08-03 failed variants carry
interactive retry/ignore buttons: actions arrive over the WS listener's
card.action.trigger callback (not workflow signals — the workflow has already
ended), retry goes through the same claim-guarded path as `rerun`, and ignore
writes a `decisions` row with `actor="human"` for audit. The worker also runs an
optional command listener over the
app's WebSocket event subscription: when `FEISHU_CMD_WHITELIST` (comma-separated
open_ids) is set, whitelisted users can send the bot DM commands (`status`,
`devices`, `rerun <source_workflow_id> [variant]`, `unquarantine [device_id]`,
`plan <自然语言需求>`);
messages from anyone else are silently ignored.

`rerun` accepts only an authoritative, closed source recorded in `workflow_runs`.
Without a variant it retries only source-output entries satisfying
`verdict != PASSED && verdict != SKIPPED`; an explicit variant remains allowed when it
belongs to the source run, including PASSED or SKIPPED. Legacy rows returned by
`RecentRuns` are display-only and cannot be rerun. Each direct text command allocates a
fresh attempt and workflow ID. Explicit single-variant retries (text `rerun` with a
variant, and the card retry button) are guarded by a claim check: when the latest
retry for the same source run + variant is still open in Temporal, the command is
refused with an in-flight notice instead of allocating a new attempt.

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

The finer "PASSED logs 7 days, failures 90 days" split named in CLAUDE.md §12
Phase 3 is implemented at the application layer, not as a bucket rule — verdict
is not part of the object key and is unknown at upload time, so an S3 lifecycle
rule cannot see it. A Temporal Schedule (`evidence-lifecycle-daily`, daily at
03:00 UTC, created idempotently at worker startup) runs the
`EvidenceLifecycleWorkflow`: it queries `tasks` for COMPLETED rows past the
retention bound (PASSED > 7d, other verdicts > 90d) and deletes only the
`runs/{task_id}/` prefix. `evidence/{task_id}/` snapshots are deliberately out
of scope — they are the replay record behind a `decisions` row and must not
expire. The bucket-level 90-day `runs/` rule stays as a backstop; the schedule
is what enforces the 7-day PASSED half.

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
curl -fsS --cacert deploy/certs/ca-cert.pem --cert deploy/certs/client-windows-client-01.pem https://127.0.0.1:18091/healthz
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

### Device-quarantine notifications

When a device is auto-quarantined (three consecutive `device`-scoped failures), the
release path writes an `event_type=device-quarantined` outbox row in the same transaction
that flips the device to `QUARANTINED`. `relay` delivers it through the same `FEISHU_*`
credentials as `worker` — the shared `runtime-environment` compose anchor does not include
`FEISHU_*`, so the `relay` service definition passes them explicitly. Getting this wrong
does not make the notification silently disappear; it makes the outbox row silently
undeliverable, which is worse because nobody comes looking for it. Three configurations,
choose one on purpose:

| Config | Behavior |
|---|---|
| `FEISHU_*` set (either mode) | Delivered normally; failures retry and count toward `outbox_backlog` like any other row |
| `FEISHU_*` unset, `RELAY_DEVICE_NOTIFY` unset (default) | Treated as a deployment gap: the row stays pending with `last_error = "notifier not configured; ..."` and shows up in the backlog — it is never marked delivered |
| `RELAY_DEVICE_NOTIFY=off` | Intentionally disabled: the row is marked delivered immediately (logged at info) and does not occupy the backlog |

If `outbox_backlog` shows a stuck row with `last_error` mentioning "notifier not
configured", that is not an infra failure to retry away — it means `FEISHU_*` needs to be
set on the `relay` service, or `RELAY_DEVICE_NOTIFY=off` needs to be set deliberately.

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

### Phase 3 mTLS (optional)

Generate certificates:
```bash
./scripts/generate-certs.sh windows-client-01
```
This creates `deploy/certs/` containing:
- `ca-cert.pem` — CA cert (distribute to all nodes)
- `server-cert.pem` + `server-key.pem` — Runtime server identity
- `client-windows-client-01.pem` — Agent cert (copy to Windows, AGENT_MTLS_CERT_FILE)

Enable by setting the three `MTLS_*` env vars in `deploy/.env` and restarting
the worker. All three must be non-empty to activate; any empty variable
gracefully degrades to plain HTTP (backward-compatible with Phase 2).

## Grafana dashboard (Phase 4)

Grafana is a pure read-only consumer of the `hermes_runtime` database — it is
not on any execution critical path and writes no data. The dashboard JSON lives
in `deploy/grafana/dashboards/` and is provisioned automatically on container
start; change a panel, push, and `docker compose ... up -d --no-deps grafana`
picks it up.

The dashboard (`Device Test Operations`) shows:

| Panel | Source | Purpose |
|---|---|---|
| Task Status Distribution | `tasks.status` | How many tasks are in each lifecycle state (7d) |
| Verdict Breakdown | `tasks.verdict` (terminal states) | Donut: PASSED vs TEST_FAILED vs INFRA_ERROR vs INCONCLUSIVE |
| Error Category | `tasks.error_category` | INFRA / CODE / MODEL / DEVICE / PERF / UNKNOWN |
| Test Throughput | `tasks` stacked by verdict | 7d bar chart, green/red/orange/yellow/gray |
| Device Status | `devices` JOIN `clients` | Live table: serial, SoC, status, fail_streak, agent version, heartbeat |
| Device Fail Streak Trend | `task_events` JOIN `devices` | 7d step-line per device — catch escalating failure streaks |
| Task Duration P50/P99 | `results.result_json->duration_sec` | Percentile by task, top 20 |
| Metrics Baseline Delta % | `metrics` table | Latest value vs 30d median, sorted by abs(delta) — spot regressions |
| Outbox Backlog | `outbox` + `outbox_backlog` view | Pending / stuck / oldest age stat + 1h time series + error snippets |
| Decisions | `decisions` | Latest 15: actor (rule/hermes/human), model, output snippet |
| Audit Log | `audit_log` | Latest 15: actor, action, target |

Access (localhost-only, like Temporal UI):

```bash
# local
open http://127.0.0.1:${GRAFANA_HOST_PORT:-13000}

# remote via SSH port forwarding
ssh -L 13000:127.0.0.1:13000 "$Q_UAT_HOST"
```

Login with `GRAFANA_ADMIN_USER` (default `admin`) and
`GRAFANA_ADMIN_PASSWORD` (required in `deploy/.env`).

The datasource connects as `${RUNTIME_DB_USER}` (same role the Runtime uses)
with `sslmode: disable` inside the Compose network. Grafana's `editable: false`
flag prevents UI-side datasource tampering; the only way to change it is
through Git-tracked provisioning files.
