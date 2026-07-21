# Hermes DevOps Runtime Compose Deployment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build and validate an isolated q-uat Docker Compose stack containing PostgreSQL, Temporal, Trigger, and Worker without modifying the existing `/opt/hermes` Agent stack.

**Architecture:** A single non-root Runtime image contains the Trigger and Worker binaries; Compose runs them as separate services on an isolated bridge network. PostgreSQL persists Temporal and Runtime state in separate databases, Trigger receives GitLab hooks on host port 18090, and Worker callbacks remain localhost-only until HTTPS/mTLS is implemented.

**Tech Stack:** Go 1.26.5, Docker Compose v2, PostgreSQL 15, Temporal Server 1.29.6 (`auto-setup`, q-uat only), Temporal UI 2.49.1, Python `unittest`, POSIX shell.

---

## File map

- Modify `runtime/internal/callbacks/handler.go`: add Worker HTTP liveness route.
- Modify `runtime/internal/callbacks/handler_test.go`: verify liveness route and method handling.
- Modify `.gitignore`: exclude real deployment environment and image lock files.
- Create `runtime/Dockerfile`: build both Runtime binaries and a non-root runtime image.
- Create `.dockerignore`: constrain the Docker build context.
- Create `deploy/docker-compose.yml`: define the isolated q-uat stack.
- Create `deploy/.env.example`: document non-secret configuration and bootstrap image tags.
- Create `deploy/postgres/init/10-runtime-db.sh`: idempotently create the Runtime role/database.
- Create `deploy/scripts/lock-images.sh`: resolve bootstrap tags to immutable repo digests.
- Create `deploy/scripts/validate-env.sh`: fail before Compose if required secrets or digests are missing.
- Create `deploy/scripts/verify-pipeline.sh`: exercise Pipeline 656 from GitLab through Temporal.
- Create `deploy/tests/test_deploy_contracts.py`: dependency-free static deployment contract tests.
- Create `deploy/README.md`: q-uat deployment, verification, upgrade, and rollback runbook.
- Modify `runtime/README.md`: point operators to the deployment runbook and current Worker state.

## Preconditions

- Work only in `/home/maxin/Code/hermes_ai_devops/.worktrees/runtime-compose-deployment` on branch `feature/runtime-compose-deployment`.
- The existing `/opt/hermes/docker-compose.yml`, `hermes`, `hermes-dashboard`, and the process using host port 8090 are out of scope and must not be changed or stopped.
- q-uat must provide Docker Engine plus `docker compose`, and the deploying user must have Docker access. No sudo-dependent step is assumed.
- Before q-uat validation, rotate the Runner registration token exposed during earlier API diagnostics.
- Never paste `deploy/.env`, `GITLAB_TOKEN`, `TRIGGER_WEBHOOK_SECRET`, database passwords, or authorization headers into logs or chat.

### Task 1: Add Worker callback liveness endpoint

**Files:**
- Modify: `runtime/internal/callbacks/handler_test.go`
- Modify: `runtime/internal/callbacks/handler.go`

- [ ] **Step 1: Write the failing liveness test**

Append this test to `runtime/internal/callbacks/handler_test.go`:

```go
func TestHealthz(t *testing.T) {
	_, _, srv := newEnv(t)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q, want ok", body)
	}
}
```

Add `"io"` to the import block.

- [ ] **Step 2: Run the focused test and verify RED**

Run:

```bash
cd runtime
/home/maxin/.local/go/bin/go test ./internal/callbacks -run '^TestHealthz$' -v
```

Expected: FAIL because `GET /healthz` currently returns 404.

- [ ] **Step 3: Add the minimal endpoint**

Add this route inside `(*Handler).Mux`, before the callback routes:

```go
mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
})
```

The complete route block must be:

```go
func (h *Handler) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /callbacks/v1/heartbeat", h.heartbeat)
	mux.HandleFunc("POST /callbacks/v1/task-events", h.taskEvent)
	mux.HandleFunc("POST /callbacks/v1/results", h.result)
	return mux
}
```

- [ ] **Step 4: Run focused and package tests**

Run:

```bash
/home/maxin/.local/go/bin/gofmt -w runtime/internal/callbacks/handler.go runtime/internal/callbacks/handler_test.go
cd runtime
/home/maxin/.local/go/bin/go test ./internal/callbacks -v
```

Expected: all callback tests PASS.

- [ ] **Step 5: Commit**

```bash
git add runtime/internal/callbacks/handler.go runtime/internal/callbacks/handler_test.go
git commit -m "feat(runtime): add worker health endpoint"
```

### Task 2: Establish deployment contract tests and secret exclusions

**Files:**
- Create: `deploy/tests/test_deploy_contracts.py`
- Modify: `.gitignore`

- [ ] **Step 1: Create the first failing deployment contract tests**

Create `deploy/tests/test_deploy_contracts.py`:

```python
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
GITIGNORE = ROOT / ".gitignore"


class SecretExclusionContracts(unittest.TestCase):
    def test_real_deployment_state_is_ignored(self):
        text = GITIGNORE.read_text(encoding="utf-8")
        self.assertIn("/deploy/.env", text)
        self.assertIn("/deploy/images.lock.env", text)


if __name__ == "__main__":
    unittest.main()
```

- [ ] **Step 2: Run the test and verify RED**

Run:

```bash
python3 -m unittest deploy.tests.test_deploy_contracts -v
```

Expected: FAIL because the two deployment paths are not yet ignored.

- [ ] **Step 3: Exclude real deployment state**

Append to `.gitignore`:

```gitignore

# Runtime deployment secrets and platform-specific image digests
/deploy/.env
/deploy/images.lock.env
```

- [ ] **Step 4: Verify ignore rules**

Run:

```bash
git check-ignore -v deploy/.env deploy/images.lock.env
```

Expected: both paths are ignored by `.gitignore`. Then run:

```bash
python3 -m unittest deploy.tests.test_deploy_contracts -v
```

Expected: PASS.

- [ ] **Step 5: Commit the test and ignore rules**

```bash
git add .gitignore deploy/tests/test_deploy_contracts.py
git commit -m "test(deploy): define runtime compose contracts"
```

### Task 3: Build one non-root Runtime image for Trigger and Worker

**Files:**
- Create: `runtime/Dockerfile`
- Create: `.dockerignore`
- Test: `deploy/tests/test_deploy_contracts.py`

- [ ] **Step 1: Add the failing Runtime image contract**

Add this constant below `GITIGNORE` in `deploy/tests/test_deploy_contracts.py`:

```python
DOCKERFILE = ROOT / "runtime" / "Dockerfile"
```

Add this test class before the `if __name__ == "__main__"` block:

```python
class RuntimeImageContracts(unittest.TestCase):
    def test_runtime_image_is_non_root_and_builds_both_commands(self):
        text = DOCKERFILE.read_text(encoding="utf-8")
        self.assertIn("go build", text)
        self.assertIn("./cmd/trigger", text)
        self.assertIn("./cmd/worker", text)
        self.assertIn("USER hermes", text)
        self.assertIn("/etc/hermes/variants.yaml", text)
```

- [ ] **Step 2: Run the focused test and verify RED**

```bash
python3 -m unittest \
  deploy.tests.test_deploy_contracts.RuntimeImageContracts -v
```

Expected: FAIL because `runtime/Dockerfile` does not exist.

- [ ] **Step 3: Create the Docker build context exclusions**

Create `.dockerignore`:

```dockerignore
.git
.worktrees
.venv
__pycache__
.pytest_cache
agent-runs
deploy/.env
deploy/images.lock.env
docs
```

- [ ] **Step 4: Create the Runtime Dockerfile**

Create `runtime/Dockerfile`:

```dockerfile
# syntax=docker/dockerfile:1.7
ARG GO_IMAGE=golang:1.26.5-bookworm
ARG RUNTIME_BASE_IMAGE=alpine:3.22.1

FROM ${GO_IMAGE} AS build
WORKDIR /src/runtime
COPY runtime/go.mod runtime/go.sum ./
RUN go mod download
COPY runtime/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/hermes-trigger ./cmd/trigger \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/hermes-worker ./cmd/worker

FROM ${RUNTIME_BASE_IMAGE}
RUN apk add --no-cache ca-certificates wget \
 && addgroup -S hermes \
 && adduser -S -G hermes -h /nonexistent -s /sbin/nologin hermes
COPY --from=build /out/hermes-trigger /app/hermes-trigger
COPY --from=build /out/hermes-worker /app/hermes-worker
COPY ci/variants.yaml /etc/hermes/variants.yaml
USER hermes
WORKDIR /app
CMD ["/app/hermes-trigger"]
```

- [ ] **Step 5: Run static image contract tests**

Run:

```bash
python3 -m unittest deploy.tests.test_deploy_contracts.DeployContracts.test_runtime_image_is_non_root_and_builds_both_commands -v
```

Expected: the focused test and the complete deployment test module PASS.

- [ ] **Step 6: Build and smoke the image on q-uat**

This step cannot run in the current development environment because Docker is absent. On q-uat, from the repository root, run:

```bash
docker build \
  --build-arg GO_IMAGE=golang:1.26.5-bookworm \
  --build-arg RUNTIME_BASE_IMAGE=alpine:3.22.1 \
  -f runtime/Dockerfile \
  -t hermes-runtime:test .

docker run --rm --entrypoint /app/hermes-trigger hermes-runtime:test 2>&1 | tail -n 1
docker run --rm --entrypoint /app/hermes-worker hermes-runtime:test 2>&1 | tail -n 1
```

Expected: image build succeeds; each binary starts and then fails fast with a missing required configuration message. Neither command may fail with “not found”, loader, permission, or CA errors.

- [ ] **Step 7: Commit**

```bash
git add .dockerignore runtime/Dockerfile deploy/tests/test_deploy_contracts.py
git commit -m "build(runtime): add container image"
```

### Task 4: Add PostgreSQL and isolated Runtime Compose stack

**Files:**
- Create: `deploy/.env.example`
- Create: `deploy/postgres/init/10-runtime-db.sh`
- Create: `deploy/scripts/lock-images.sh`
- Create: `deploy/scripts/validate-env.sh`
- Create: `deploy/docker-compose.yml`
- Test: `deploy/tests/test_deploy_contracts.py`

- [ ] **Step 1: Add failing Compose, image-lock, and secret-template contracts**

Add `import re` to `deploy/tests/test_deploy_contracts.py`, then add these constants below
`DOCKERFILE`:

```python
COMPOSE = ROOT / "deploy" / "docker-compose.yml"
ENV_EXAMPLE = ROOT / "deploy" / ".env.example"
INIT_DB = ROOT / "deploy" / "postgres" / "init" / "10-runtime-db.sh"
LOCK_IMAGES = ROOT / "deploy" / "scripts" / "lock-images.sh"
VALIDATE_ENV = ROOT / "deploy" / "scripts" / "validate-env.sh"
```

Add this class before the module's `if __name__ == "__main__"` block:

```python
class ComposeContracts(unittest.TestCase):
    def test_required_compose_files_exist(self):
        for path in (COMPOSE, ENV_EXAMPLE, INIT_DB, LOCK_IMAGES, VALIDATE_ENV):
            self.assertTrue(path.is_file(), path)

    def test_compose_isolated_services_and_ports(self):
        text = COMPOSE.read_text(encoding="utf-8")
        for service in ("postgres:", "temporal:", "temporal-ui:", "trigger:", "worker:"):
            self.assertIn(service, text)
        self.assertIn(
            '${TRIGGER_BIND_IP:-0.0.0.0}:${TRIGGER_HOST_PORT:-18090}:8090',
            text,
        )
        self.assertIn(
            '127.0.0.1:${WORKER_CALLBACKS_HOST_PORT:-18091}:8091',
            text,
        )
        self.assertIn(
            '127.0.0.1:${TEMPORAL_UI_HOST_PORT:-18080}:8080',
            text,
        )
        self.assertIn("TEMPORAL_ADDRESS: temporal:7233", text)
        self.assertIn("TEMPORAL_TASK_QUEUE: device-test", text)
        self.assertNotIn("network_mode: host", text)
        self.assertNotIn("container_name:", text)

    def test_compose_requires_locked_third_party_images(self):
        text = COMPOSE.read_text(encoding="utf-8")
        for variable in (
            "POSTGRES_IMAGE",
            "TEMPORAL_IMAGE",
            "TEMPORAL_UI_IMAGE",
            "GO_IMAGE",
            "RUNTIME_BASE_IMAGE",
        ):
            self.assertIn(variable, text)
        self.assertNotRegex(text, re.compile(r"image:\s+[^$\n]*:latest(?:\s|$)"))

    def test_example_contains_no_real_secret(self):
        text = ENV_EXAMPLE.read_text(encoding="utf-8")
        for key in (
            "POSTGRES_ADMIN_PASSWORD",
            "RUNTIME_DB_PASSWORD",
            "GITLAB_TOKEN",
            "TRIGGER_WEBHOOK_SECRET",
        ):
            self.assertRegex(text, rf"(?m)^{key}=\s*$")
        self.assertNotIn("PRIVATE-TOKEN:", text)
```

- [ ] **Step 2: Run the Compose contract and verify RED**

```bash
python3 -m unittest deploy.tests.test_deploy_contracts.ComposeContracts -v
```

Expected: FAIL because the Compose, environment, database init, and helper files do not exist.

- [ ] **Step 3: Create the non-secret environment template**

Create `deploy/.env.example`:

```dotenv
COMPOSE_PROJECT_NAME=hermes-runtime

# Bootstrap tags. Run deploy/scripts/lock-images.sh deploy/.env before compose up;
# the script writes immutable repo digests to deploy/images.lock.env.
POSTGRES_IMAGE=postgres:15.13-bookworm
TEMPORAL_IMAGE=temporalio/auto-setup:1.29.6
TEMPORAL_UI_IMAGE=temporalio/ui:2.49.1
GO_IMAGE=golang:1.26.5-bookworm
RUNTIME_BASE_IMAGE=alpine:3.22.1
RUNTIME_IMAGE_TAG=dev

POSTGRES_ADMIN_USER=temporal_admin
POSTGRES_ADMIN_PASSWORD=
RUNTIME_DB_USER=hermes_runtime
RUNTIME_DB_PASSWORD=

GITLAB_BASE_URL=https://gitlab2.quectel.com
GITLAB_TOKEN=
GITLAB_TOKEN_HEADER=PRIVATE-TOKEN
PACKAGE_NAME=algo-super-sdk
TRIGGER_WEBHOOK_SECRET=
TRIGGER_REFS=master

TRIGGER_BIND_IP=0.0.0.0
TRIGGER_HOST_PORT=18090
WORKER_CALLBACKS_HOST_PORT=18091
TEMPORAL_UI_HOST_PORT=18080

CALLBACK_BASE_URL=http://127.0.0.1:18091
ARTIFACT_AUTH_TYPE=job_token
ARTIFACT_AUTH_TOKEN=
FEISHU_WEBHOOK_URL=
```

- [ ] **Step 4: Create the idempotent Runtime database initializer**

Create `deploy/postgres/init/10-runtime-db.sh`:

```sh
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
```

Make it executable:

```bash
chmod 0755 deploy/postgres/init/10-runtime-db.sh
bash -n deploy/postgres/init/10-runtime-db.sh
```

Expected: shell syntax passes.

- [ ] **Step 5: Create immutable image locking helper**

Create `deploy/scripts/lock-images.sh`:

```sh
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
for key in POSTGRES_IMAGE TEMPORAL_IMAGE TEMPORAL_UI_IMAGE GO_IMAGE RUNTIME_BASE_IMAGE; do
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
```

Make it executable and syntax-check it:

```bash
chmod 0755 deploy/scripts/lock-images.sh
bash -n deploy/scripts/lock-images.sh
```

- [ ] **Step 6: Create preflight environment validation**

Create `deploy/scripts/validate-env.sh`:

```sh
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

if ss -ltnH "sport = :${TRIGGER_HOST_PORT:-18090}" | grep -q .; then
  echo "ERROR: trigger host port ${TRIGGER_HOST_PORT:-18090} is occupied" >&2
  exit 1
fi

echo "Deployment environment is valid"
```

Make it executable and syntax-check it:

```bash
chmod 0755 deploy/scripts/validate-env.sh
bash -n deploy/scripts/validate-env.sh
```

- [ ] **Step 7: Create the isolated Compose stack**

Create `deploy/docker-compose.yml`:

```yaml
name: hermes-runtime

x-runtime-common: &runtime-common
  image: hermes-runtime:${RUNTIME_IMAGE_TAG:-dev}
  restart: unless-stopped
  networks: [runtime]
  read_only: true
  tmpfs: [/tmp]
  security_opt: [no-new-privileges:true]
  cap_drop: [ALL]
  environment: &runtime-environment
    TEMPORAL_ADDRESS: temporal:7233
    TEMPORAL_TASK_QUEUE: device-test
    DATABASE_URL: >-
      host=postgres port=5432 user=${RUNTIME_DB_USER:-hermes_runtime}
      password=${RUNTIME_DB_PASSWORD:?RUNTIME_DB_PASSWORD is required}
      dbname=hermes_runtime sslmode=disable

services:
  postgres:
    image: ${POSTGRES_IMAGE:?POSTGRES_IMAGE must come from images.lock.env}
    restart: unless-stopped
    networks: [runtime]
    environment:
      POSTGRES_DB: postgres
      POSTGRES_USER: ${POSTGRES_ADMIN_USER:-temporal_admin}
      POSTGRES_PASSWORD: ${POSTGRES_ADMIN_PASSWORD:?POSTGRES_ADMIN_PASSWORD is required}
      RUNTIME_DB_USER: ${RUNTIME_DB_USER:-hermes_runtime}
      RUNTIME_DB_PASSWORD: ${RUNTIME_DB_PASSWORD:?RUNTIME_DB_PASSWORD is required}
    volumes:
      - postgres-data:/var/lib/postgresql/data
      - ./postgres/init/10-runtime-db.sh:/docker-entrypoint-initdb.d/10-runtime-db.sh:ro
    healthcheck:
      test: [CMD-SHELL, "pg_isready -U $$POSTGRES_USER -d $$POSTGRES_DB"]
      interval: 5s
      timeout: 5s
      retries: 30
      start_period: 10s

  temporal:
    image: ${TEMPORAL_IMAGE:?TEMPORAL_IMAGE must come from images.lock.env}
    restart: unless-stopped
    networks: [runtime]
    depends_on:
      postgres:
        condition: service_healthy
    environment:
      DB: postgres12
      DB_PORT: 5432
      POSTGRES_USER: ${POSTGRES_ADMIN_USER:-temporal_admin}
      POSTGRES_PWD: ${POSTGRES_ADMIN_PASSWORD:?POSTGRES_ADMIN_PASSWORD is required}
      POSTGRES_SEEDS: postgres
      DBNAME: temporal
      VISIBILITY_DBNAME: temporal_visibility
    healthcheck:
      test: [CMD-SHELL, "tctl --address 127.0.0.1:7233 cluster health >/dev/null 2>&1"]
      interval: 5s
      timeout: 5s
      retries: 60
      start_period: 30s

  temporal-ui:
    image: ${TEMPORAL_UI_IMAGE:?TEMPORAL_UI_IMAGE must come from images.lock.env}
    restart: unless-stopped
    networks: [runtime]
    depends_on:
      temporal:
        condition: service_healthy
    environment:
      TEMPORAL_ADDRESS: temporal:7233
      TEMPORAL_CORS_ORIGINS: http://localhost:${TEMPORAL_UI_HOST_PORT:-18080}
    ports:
      - "127.0.0.1:${TEMPORAL_UI_HOST_PORT:-18080}:8080"

  trigger:
    <<: *runtime-common
    build:
      context: ..
      dockerfile: runtime/Dockerfile
      args:
        GO_IMAGE: ${GO_IMAGE:?GO_IMAGE must come from images.lock.env}
        RUNTIME_BASE_IMAGE: ${RUNTIME_BASE_IMAGE:?RUNTIME_BASE_IMAGE must come from images.lock.env}
    command: [/app/hermes-trigger]
    depends_on:
      temporal:
        condition: service_healthy
      postgres:
        condition: service_healthy
    environment:
      <<: *runtime-environment
      TRIGGER_ADDR: :8090
      TRIGGER_WEBHOOK_SECRET: ${TRIGGER_WEBHOOK_SECRET:?TRIGGER_WEBHOOK_SECRET is required}
      TRIGGER_REFS: ${TRIGGER_REFS:-master}
      GITLAB_BASE_URL: ${GITLAB_BASE_URL:-https://gitlab2.quectel.com}
      GITLAB_TOKEN: ${GITLAB_TOKEN:?GITLAB_TOKEN is required}
      GITLAB_TOKEN_HEADER: ${GITLAB_TOKEN_HEADER:-PRIVATE-TOKEN}
      PACKAGE_NAME: ${PACKAGE_NAME:-algo-super-sdk}
    ports:
      - "${TRIGGER_BIND_IP:-0.0.0.0}:${TRIGGER_HOST_PORT:-18090}:8090"
    healthcheck:
      test: [CMD, wget, -qO-, http://127.0.0.1:8090/healthz]
      interval: 5s
      timeout: 3s
      retries: 20
      start_period: 10s

  worker:
    <<: *runtime-common
    command: [/app/hermes-worker]
    depends_on:
      trigger:
        condition: service_healthy
    environment:
      <<: *runtime-environment
      WORKER_CALLBACKS_ADDR: :8091
      VARIANTS_CONFIG: /etc/hermes/variants.yaml
      CALLBACK_BASE_URL: ${CALLBACK_BASE_URL:-http://127.0.0.1:18091}
      ARTIFACT_AUTH_TYPE: ${ARTIFACT_AUTH_TYPE:-job_token}
      ARTIFACT_AUTH_TOKEN: ${ARTIFACT_AUTH_TOKEN:-}
      FEISHU_WEBHOOK_URL: ${FEISHU_WEBHOOK_URL:-}
    ports:
      - "127.0.0.1:${WORKER_CALLBACKS_HOST_PORT:-18091}:8091"
    healthcheck:
      test: [CMD, wget, -qO-, http://127.0.0.1:8091/healthz]
      interval: 5s
      timeout: 3s
      retries: 20
      start_period: 10s

networks:
  runtime:
    name: hermes-runtime

volumes:
  postgres-data:
    name: hermes-runtime-postgres
```

- [ ] **Step 8: Run static tests and shell syntax checks**

Run:

```bash
python3 -m unittest deploy.tests.test_deploy_contracts -v
bash -n deploy/postgres/init/10-runtime-db.sh
bash -n deploy/scripts/lock-images.sh
bash -n deploy/scripts/validate-env.sh
```

Expected: all deployment contract tests and shell syntax checks PASS.

- [ ] **Step 9: Validate Compose on q-uat**

Run:

```bash
cp deploy/.env.example deploy/.env
chmod 0600 deploy/.env
# Edit deploy/.env locally on q-uat; do not paste its contents.
deploy/scripts/lock-images.sh deploy/.env deploy/images.lock.env
deploy/scripts/validate-env.sh deploy/.env deploy/images.lock.env
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml config >/tmp/hermes-runtime-compose.rendered.yml
```

Expected: image pulls succeed, all five third-party references are digest locked, preflight reports valid, and Compose renders without warnings. Inspect the rendered file locally because it contains secrets; delete it after inspection.

- [ ] **Step 10: Commit**

```bash
git add deploy/.env.example deploy/postgres/init/10-runtime-db.sh \
  deploy/scripts/lock-images.sh deploy/scripts/validate-env.sh \
  deploy/docker-compose.yml deploy/tests/test_deploy_contracts.py
git commit -m "feat(deploy): add isolated runtime compose stack"
```

### Task 5: Add safe Pipeline 656 verification automation

**Files:**
- Create: `deploy/scripts/verify-pipeline.sh`
- Test: `deploy/tests/test_deploy_contracts.py`

- [ ] **Step 1: Add the failing verification-script contract**

Add this constant to `deploy/tests/test_deploy_contracts.py`:

```python
VERIFY_PIPELINE = ROOT / "deploy" / "scripts" / "verify-pipeline.sh"
```

Add this class before the module's `if __name__ == "__main__"` block:

```python
class PipelineVerificationContracts(unittest.TestCase):
    def test_verifier_checks_health_registry_database_temporal_and_dedup(self):
        text = VERIFY_PIPELINE.read_text(encoding="utf-8")
        for marker in (
            "/healthz",
            "/api/v4/projects/$project_id/pipelines/$pipeline_id",
            "/webhooks/gitlab",
            "SELECT count(*) FROM artifacts",
            "workflow describe",
            "taskqueue describe",
            ".started == false",
        ):
            self.assertIn(marker, text)
```

- [ ] **Step 2: Run the focused test and verify RED**

```bash
python3 -m unittest \
  deploy.tests.test_deploy_contracts.PipelineVerificationContracts -v
```

Expected: FAIL because `deploy/scripts/verify-pipeline.sh` does not exist.

- [ ] **Step 3: Create the verification script**

Create `deploy/scripts/verify-pipeline.sh`:

```sh
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
```

Make it executable:

```bash
chmod 0755 deploy/scripts/verify-pipeline.sh
```

- [ ] **Step 4: Run shell and static tests**

Run:

```bash
bash -n deploy/scripts/verify-pipeline.sh
python3 -m unittest deploy.tests.test_deploy_contracts -v
```

Expected: syntax check and all deployment contract tests PASS.

- [ ] **Step 5: Start and inspect the q-uat stack**

On q-uat:

```bash
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml up -d --build
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml ps
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml logs --tail 100 trigger worker temporal postgres
```

Expected: PostgreSQL, Temporal, Trigger, and Worker are healthy; UI is running; no service is restarting; logs contain `trigger service listening`, `callbacks service listening`, and `temporal worker starting`.

- [ ] **Step 6: Run Pipeline 656 verification on q-uat**

```bash
deploy/scripts/verify-pipeline.sh deploy/.env deploy/images.lock.env
```

Expected:

Expected output starts with `PASS: pipeline 656 ->`, contains the returned deterministic
workflow ID, and ends with `8 artifacts, duplicate suppressed`.

The actual workflow may wait for a device and later resolve as no-device because Client RPC/heartbeat deployment is outside this plan.

- [ ] **Step 7: Commit**

```bash
git add deploy/scripts/verify-pipeline.sh deploy/tests/test_deploy_contracts.py
git commit -m "test(deploy): verify registry to temporal flow"
```

### Task 6: Document operations and run final verification

**Files:**
- Create: `deploy/README.md`
- Modify: `runtime/README.md`

- [ ] **Step 1: Write the q-uat runbook**

Create `deploy/README.md` with these exact sections and commands:

```markdown
# Hermes DevOps Runtime deployment

This Compose project is independent from `/opt/hermes`. It must not stop, rename,
or reconfigure the existing Hermes Agent containers or the process using host port 8090.

## Security boundary

This is a q-uat integration deployment. Trigger is plain HTTP protected by the GitLab
Webhook Secret Token. Worker callbacks bind to localhost because the callback handler does
not yet enforce the mTLS declared by the OpenAPI contract. Do not expose port 18091 until
HTTPS/mTLS or a test-subnet firewall rule is in place.

## Configure

```bash
cp deploy/.env.example deploy/.env
chmod 0600 deploy/.env
# Fill secrets locally; never print or commit this file.
deploy/scripts/lock-images.sh deploy/.env deploy/images.lock.env
deploy/scripts/validate-env.sh deploy/.env deploy/images.lock.env
```

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
```

- [ ] **Step 2: Update Runtime README state and deployment link**

Change the opening line of `runtime/README.md` to:

```markdown
当前内容：Phase 1.4 Temporal spike、Phase 1.5 Trigger、Phase 1.6
DeviceTestWorkflow/Worker 主干。q-uat 容器部署见 [`../deploy/README.md`](../deploy/README.md)。
```

Change the final “后续” line to:

```markdown
后续：Client Agent RPC/心跳接入、MinIO 预签名直传，以及生产 HTTPS/mTLS 硬化。
```

- [ ] **Step 3: Run complete local verification**

Run:

```bash
python3 -m unittest discover -s deploy/tests -v
bash -n deploy/postgres/init/10-runtime-db.sh
bash -n deploy/scripts/lock-images.sh
bash -n deploy/scripts/validate-env.sh
bash -n deploy/scripts/verify-pipeline.sh
cd runtime
/home/maxin/.local/go/bin/go test ./...
cd ..
git diff --check
git status --short
```

Expected: all Python and Go tests PASS, all shell syntax checks PASS, `git diff --check` is silent, and status lists only the intended documentation changes.

- [ ] **Step 4: Re-run q-uat acceptance after documentation is final**

Run on q-uat:

```bash
deploy/scripts/validate-env.sh deploy/.env deploy/images.lock.env
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml config --quiet
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml ps
deploy/scripts/verify-pipeline.sh deploy/.env deploy/images.lock.env
```

Expected: environment valid, Compose valid, four core services healthy, and Pipeline 656 verification PASS. Save only non-secret command output as deployment evidence.

- [ ] **Step 5: Commit**

```bash
git add deploy/README.md runtime/README.md
git commit -m "docs: add runtime deployment runbook"
```

### Task 7: Final review and branch handoff

**Files:**
- Review all files changed since `a6d09cb`.

- [ ] **Step 1: Inspect the complete branch diff**

```bash
git diff --stat a6d09cb..HEAD
git diff --check a6d09cb..HEAD
git log --oneline a6d09cb..HEAD
```

Expected: only the approved design, implementation plan, Worker health endpoint, Runtime image,
deployment files, tests, and documentation are present.

- [ ] **Step 2: Run the final local suite again**

```bash
python3 -m unittest discover -s deploy/tests -v
cd runtime && /home/maxin/.local/go/bin/go test ./...
```

Expected: all tests PASS with no skipped deployment contract tests.

- [ ] **Step 3: Request code review**

Use `superpowers:requesting-code-review` with base `a6d09cb` and the branch HEAD. Review must
specifically check secret handling, callback exposure, Compose dependency health, PostgreSQL
persistence, idempotent Pipeline 656 verification, and non-interference with `/opt/hermes`.

- [ ] **Step 4: Apply valid review findings and re-verify**

Use `superpowers:receiving-code-review`. For each accepted finding, add or update a failing test,
apply the smallest fix, and rerun the focused test plus the complete local suite. If Compose changes,
repeat q-uat `docker compose config --quiet` and Pipeline 656 verification.

- [ ] **Step 5: Prepare integration options**

Use `superpowers:finishing-a-development-branch` only after local tests and q-uat acceptance are
freshly passing. Do not merge or push without presenting the branch status and integration options
to the user.
