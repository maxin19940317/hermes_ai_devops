# Workflow Runs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Persist an immutable authoritative record for every new device-test workflow, make artifacts project-scoped, and change rerun/NL translation to use an exact source workflow ID without guessing Version or RuleVersion.

**Architecture:** A version-gated `RecordWorkflowRun` activity writes the normalized workflow input before any device activity. `workflow_runs` supplies immutable input identity, Temporal supplies closed-state and terminal output, and artifacts supply immutable package locations under a project-scoped key. RecentRuns reads authoritative rows first and only appends non-actionable legacy artifact fallback rows.

**Tech Stack:** Go 1.26, Temporal Go SDK, PostgreSQL 15, `database/sql` + pgx/lib/pq arrays, JSON Schema Draft 2020-12, pytest.

---

## Execution Rules

- Work only in `/home/maxin/Code/hermes_ai_devops/.worktrees/workflow-runs` on branch `workflow-runs`.
- Use `/home/maxin/.local/go/bin/go`; do not rely on shell state surviving between agents.
- Follow RED-GREEN-REFACTOR for every behavior change. Record the RED command and expected failure before editing production code.
- Run `gofmt -w` only on files touched by the current task.
- Do not alter or regenerate `runtime/internal/workflow/testdata/history-pre-notify-card.json`.
- Each task ends in its own commit and two reviews: spec compliance first, then code quality.
- Do not execute the production migration as part of implementation. Production deployment remains gated on the already-merged presign/evidence-v3/attribution batch being deployed and observed stable.

## File Map

**Persistence**

- `runtime/internal/store/schema.sql`: fresh-database schema.
- `deploy/postgres/migrations/2026-07-30-workflow-runs.sql`: stopped-writer upgrade from the old artifact key.
- `runtime/internal/store/workflow_runs.go`: types, canonicalization, errors, MemStore behavior.
- `runtime/internal/store/postgres_workflow_runs.go`: PG Record/Get/exact variant state behavior.
- `runtime/internal/store/store.go`: MemStore fields and project-aware artifact key.
- `runtime/internal/store/{postgres.go,fleet.go,postgres_fleet.go}`: project-aware artifact and RecentRuns queries.

**Workflow and Temporal**

- `runtime/internal/workflow/{types.go,devicetest.go}`: source lineage, request type, version gate.
- `runtime/internal/activity/workflow_runs.go`: activity/store adapter and non-retryable error mapping.
- `runtime/internal/trigger/starter.go`: closed-state and workflow-result reader.

**Commands and contracts**

- `runtime/internal/feishucmd/{command.go,executor.go,translate.go}`: exact rerun and authoritative snapshot validation.
- `runtime/internal/hermesclient/prompts/cmd_translate_v2.md`: new incompatible rerun prompt; v1 remains untouched.
- Three `command.schema.json` copies: contracts, Runtime embed, analyze bridge.
- Two `bundle.schema.json` copies plus kick validation: accepted projects cannot contain whitespace.

---

### Task 1: Schema, Migration, and Project-Scoped Artifact Identity

**Files:**
- Modify: `runtime/internal/store/schema.sql`
- Modify: `runtime/internal/store/store.go`
- Modify: `runtime/internal/store/postgres.go`
- Modify: `runtime/internal/store/fleet.go`
- Modify: `runtime/internal/store/postgres_fleet.go`
- Modify: `runtime/internal/store/conformance_test.go`
- Modify: `runtime/internal/store/pgtest_test.go`
- Create: `runtime/internal/store/store_test.go`
- Modify: `contracts/bundle.schema.json`
- Modify: `runtime/internal/trigger/bundle.schema.json`
- Modify: `runtime/internal/trigger/kick.go`
- Modify: `runtime/internal/trigger/bundle_test.go`
- Modify: `runtime/internal/trigger/kick_test.go`
- Create: `contracts/tests/examples/bundle/invalid/project_with_space.json`
- Create: `deploy/postgres/migrations/2026-07-30-workflow-runs.sql`
- Create: `runtime/internal/store/migration_workflow_runs_test.go`

- [ ] **Step 1: Write failing project-identity and migration tests**

Add a MemStore test that registration preserves both projects:

```go
func TestMemStoreArtifactKeyIncludesProject(t *testing.T) {
    s := NewMemStore()
    arts := []Artifact{
        {Project: "grp/a", CommitSHA: "abcd1234", PipelineID: 42, Variant: "v1",
            BuildType: "Release", URL: "a", SHA256: "sa", Size: 1, ManifestDigest: "ma"},
        {Project: "grp/b", CommitSHA: "abcd1234", PipelineID: 42, Variant: "v1",
            BuildType: "Release", URL: "b", SHA256: "sb", Size: 1, ManifestDigest: "mb"},
    }
    if err := s.RegisterArtifacts(ctx, arts); err != nil {
        t.Fatal(err)
    }
    got := s.Artifacts()
    if len(got) != 2 {
        t.Fatalf("artifacts = %+v, want both projects", got)
    }
}
```

Add `TestWorkflowRunsMigrationUpgradesLegacyArtifactKey` which creates an old-form artifacts table with:

```sql
DROP TABLE workflow_runs;
ALTER TABLE artifacts DROP CONSTRAINT artifacts_project_key;
ALTER TABLE artifacts
  ADD CONSTRAINT artifacts_commit_sha_pipeline_id_variant_key
  UNIQUE (commit_sha, pipeline_id, variant);
```

Before degrading the schema, capture a `schemaShape` by querying `information_schema.columns`,
`pg_constraint`, and `pg_indexes`. The shape includes every workflow_runs column/type/nullability/default,
its self-FK and CHECK constraints, both new indexes, `artifacts_project_key`, and the absence of the old
constraint.

The test then performs the statements above, reads
`../../../deploy/postgres/migrations/2026-07-30-workflow-runs.sql`, executes it twice, captures the same
shape, and requires `reflect.DeepEqual(upgraded, fresh)`. It also asserts:

```sql
INSERT INTO artifacts (...) VALUES ('grp/a', ...), ('grp/b', ...);
```

succeeds for the same commit/pipeline/variant, while a duplicate in `grp/a` fails.

Add `project_with_space.json` using `"project": "grp/bad project"`,
`TestBundleSchemaRejectsUncommandableProject`, and
`TestKickRejectsUncommandableProject`. The kick table covers `grp/bad project`, `/absolute`,
`grp//double`, `"grp/trailing\n"`, a 257-character value, and accepted `grp/good_project`.

- [ ] **Step 2: Run the focused tests and verify RED**

```bash
cd /home/maxin/Code/hermes_ai_devops/.worktrees/workflow-runs/runtime
PATH=/home/maxin/.local/go/bin:$PATH go test ./internal/store ./internal/trigger \
  -run 'ArtifactKeyIncludesProject|WorkflowRunsMigration|UncommandableProject' \
  -count=1 -v
cd ..
/home/maxin/Code/hermes_ai_devops/.venv/bin/python -m pytest \
  contracts/tests/test_bundle_schema.py -q
```

Expected: artifact registration collapses the two projects, migration test cannot find the new file/table, and project-with-space is accepted.

- [ ] **Step 3: Implement the final fresh schema and stopped-writer migration**

Change artifacts to:

```sql
CONSTRAINT artifacts_project_key
    UNIQUE (project, commit_sha, pipeline_id, variant)
```

Add the exact `workflow_runs` table and indexes from the approved spec to `schema.sql`.
Place `tasks_run_variant_latest_idx` after the existing `CREATE TABLE tasks` statement; placing it beside
workflow_runs near the top would reference `tasks` before that table exists on a fresh database.

Create the migration with these operations in one transaction:

```sql
BEGIN;

CREATE TABLE IF NOT EXISTS workflow_runs (
    workflow_id        TEXT PRIMARY KEY,
    project            TEXT NOT NULL,
    commit_sha         TEXT NOT NULL,
    pipeline_id        INTEGER NOT NULL CHECK (pipeline_id > 0),
    version            TEXT NOT NULL,
    rule_version       TEXT NOT NULL,
    scope              TEXT NOT NULL DEFAULT '',
    attempt            INTEGER NOT NULL CHECK (attempt >= 0),
    variants           TEXT[] NOT NULL,
    source_workflow_id TEXT REFERENCES workflow_runs(workflow_id) ON DELETE RESTRICT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (source_workflow_id IS NULL OR source_workflow_id <> workflow_id)
);

CREATE INDEX IF NOT EXISTS workflow_runs_recent_idx
    ON workflow_runs(created_at DESC, workflow_id DESC);
CREATE INDEX IF NOT EXISTS tasks_run_variant_latest_idx
    ON tasks(workflow_id, test_id, attempt DESC, created_at DESC);

CREATE UNIQUE INDEX IF NOT EXISTS artifacts_project_key
    ON artifacts(project, commit_sha, pipeline_id, variant);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'artifacts'::regclass
          AND conname = 'artifacts_project_key'
    ) THEN
        ALTER TABLE artifacts
            ADD CONSTRAINT artifacts_project_key
            UNIQUE USING INDEX artifacts_project_key;
    END IF;
END $$;

ALTER TABLE artifacts
    DROP CONSTRAINT IF EXISTS artifacts_commit_sha_pipeline_id_variant_key;

COMMIT;
```

Update PG registration to:

```sql
ON CONFLICT ON CONSTRAINT artifacts_project_key DO NOTHING
```

and change the MemStore artifact key to:

```go
func artifactKey(project, commit string, pipelineID int, variant string) string {
    return project + "|" + commit + "|" + strconv.Itoa(pipelineID) + "|" + variant
}
```

Use this helper for `rows` and `rowSeq`. The still-legacy project-agnostic List/Next methods compare
struct fields rather than map-key prefixes and fail closed when matching rows span multiple projects;
they must not return mixed packages or increment either project's counter. PG legacy methods perform the
same distinct-project check before reading/updating. Task 6 changes the public signatures atomically with
rerun and removes this transitional path.

Add `workflow_runs` to the PG test `TRUNCATE`.

Update the existing adversarial RecentRuns conformance assertion to key results by
`Project + "|" + Variant`, not Variant alone. After project-scoped registration both projects are real
rows; a variant-only map would overwrite one of them and make the old test nondeterministic before
Task 4 replaces prefix association.

- [ ] **Step 4: Enforce command-expressible GitLab project paths**

Set both bundle schema copies to:

```json
"project": {
  "type": "string",
  "pattern": "^[A-Za-z0-9_][A-Za-z0-9_.-]*(?:/[A-Za-z0-9_][A-Za-z0-9_.-]*)*$",
  "maxLength": 256,
  "not": { "pattern": "\\s" }
}
```

Use the same compiled regexp in kick validation:

```go
var kickProjectRegexp = regexp.MustCompile(
    `^[A-Za-z0-9_][A-Za-z0-9_.-]*(?:/[A-Za-z0-9_][A-Za-z0-9_.-]*)*$`,
)
```

Validation requires this regexp, `len(project) <= 256`, and `!strings.Contains(project, "..")`. This
deliberately excludes whitespace, absolute paths, empty segments and `..` while preserving real GitLab
namespace paths. With the existing 128-character variant bound, the longest generated scoped retry
workflow ID remains below the command schema's 512-character argument limit.

- [ ] **Step 5: Run GREEN tests and the migration twice**

```bash
cd /home/maxin/Code/hermes_ai_devops/.worktrees/workflow-runs/runtime
PATH=/home/maxin/.local/go/bin:$PATH gofmt -w \
  internal/store/store.go internal/store/postgres.go \
  internal/store/fleet.go internal/store/postgres_fleet.go \
  internal/store/conformance_test.go internal/store/pgtest_test.go \
  internal/store/store_test.go \
  internal/store/migration_workflow_runs_test.go \
  internal/trigger/kick.go internal/trigger/kick_test.go internal/trigger/bundle_test.go
PATH=/home/maxin/.local/go/bin:$PATH go test ./internal/store ./internal/trigger -count=1
cd ..
/home/maxin/Code/hermes_ai_devops/.venv/bin/python -m pytest \
  contracts/tests/test_bundle_schema.py -q
```

The migration test must execute the migration twice and prove the second execution is a no-op.

- [ ] **Step 6: Commit**

```bash
git add contracts/bundle.schema.json contracts/tests/examples/bundle/invalid/project_with_space.json \
  runtime/internal/trigger/bundle.schema.json runtime/internal/trigger/bundle_test.go \
  runtime/internal/trigger/kick.go runtime/internal/trigger/kick_test.go \
  runtime/internal/store/schema.sql runtime/internal/store/store.go \
  runtime/internal/store/postgres.go runtime/internal/store/fleet.go \
  runtime/internal/store/postgres_fleet.go runtime/internal/store/conformance_test.go \
  runtime/internal/store/pgtest_test.go runtime/internal/store/store_test.go \
  runtime/internal/store/migration_workflow_runs_test.go \
  deploy/postgres/migrations/2026-07-30-workflow-runs.sql
git commit -m "feat(store): scope artifacts by project"
```

---

### Task 2: Immutable WorkflowRun Store

**Files:**
- Create: `runtime/internal/store/workflow_runs.go`
- Create: `runtime/internal/store/postgres_workflow_runs.go`
- Modify: `runtime/internal/store/store.go`
- Modify: `runtime/internal/store/conformance_test.go`

- [ ] **Step 1: Write failing Mem/PG conformance tests**

Extend `fullStore` with:

```go
RecordWorkflowRun(context.Context, WorkflowRun) error
GetWorkflowRun(context.Context, string) (*WorkflowRun, error)
ListWorkflowRunVariantStates(context.Context, string) ([]RunVariantState, error)
```

Add subtests named:

```text
WorkflowRunRecordGetCanonical
WorkflowRunIdempotent
WorkflowRunConflictEveryField
WorkflowRunRejectsMissingSource
WorkflowRunDefensiveCopy
WorkflowRunVariantStatesAreExact
```

Use this seed:

```go
base := WorkflowRun{
    WorkflowID: "device-test-grp/p-gabcd1234-p42",
    Project: "grp/p", CommitSHA: "abcd1234", PipelineID: 42,
    Version: "1.2.3", RuleVersion: "verdict-rules-v1",
    Variants: []string{"v2", "", "v1", "v2"},
}
```

Assert the stored variants are exactly `[]string{"v1", "v2"}`. Keeping WorkflowID fixed, change each
other immutable field exactly once and assert `errors.Is(err, ErrWorkflowRunConflict)`. WorkflowID itself
is the row key, so a different ID is a different row rather than a conflict case. Record a child only
after the parent exists; seed a second valid parent before testing a conflicting SourceWorkflowID. A
missing parent must satisfy `errors.Is(err, ErrWorkflowRunPermanent)`.

- [ ] **Step 2: Run tests and verify RED**

```bash
cd /home/maxin/Code/hermes_ai_devops/.worktrees/workflow-runs/runtime
PATH=/home/maxin/.local/go/bin:$PATH go test ./internal/store \
  -run 'Test(Mem|PG)StoreConformance/WorkflowRun' -count=1 -v
```

Expected: undefined WorkflowRun APIs.

- [ ] **Step 3: Implement types, canonicalization, errors, and MemStore**

Define:

```go
var (
    ErrWorkflowRunNotFound = errors.New("workflow run not found")
    ErrWorkflowRunConflict = errors.New("workflow run immutable content conflict")
    ErrWorkflowRunPermanent = errors.New("workflow run permanent error")
)
```

`canonicalWorkflowRun` must copy, remove empty variants, sort, deduplicate, and validate non-empty
WorkflowID/Project/CommitSHA/Version/RuleVersion, positive PipelineID, non-negative Attempt, and
`SourceWorkflowID != WorkflowID`. It must never sort a caller-owned slice in place.

Add to MemStore:

```go
workflowRuns map[string]WorkflowRun
runSeq       map[string]int64
```

Record under the existing mutex. Enforce the source FK in memory. Compare all immutable fields except
CreatedAt. Return defensive copies from Record/Get/List.

- [ ] **Step 4: Implement the PostgreSQL store**

Use `pq.Array` for variants. Record in a transaction:

```sql
INSERT INTO workflow_runs
  (workflow_id,project,commit_sha,pipeline_id,version,rule_version,
   scope,attempt,variants,source_workflow_id)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''))
ON CONFLICT (workflow_id) DO NOTHING
```

Then execute a separate SELECT statement in the same read-committed transaction and compare the
canonical row. Do not use a single CTE snapshot and do not update on conflict.

Classify `*pgconn.PgError` codes `23502`, `23503`, `23505`, and `23514` as
`ErrWorkflowRunPermanent`; an already-existing workflow ID is first read back and becomes either
idempotent success or `ErrWorkflowRunConflict`.

For variant state, use:

```sql
SELECT DISTINCT ON (test_id)
       test_id, status, verdict, ended_at
FROM tasks
WHERE workflow_id = $1
ORDER BY test_id, attempt DESC, created_at DESC, task_id DESC
```

- [ ] **Step 5: Run focused and full store tests**

```bash
cd /home/maxin/Code/hermes_ai_devops/.worktrees/workflow-runs/runtime
PATH=/home/maxin/.local/go/bin:$PATH gofmt -w \
  internal/store/workflow_runs.go internal/store/postgres_workflow_runs.go \
  internal/store/store.go internal/store/conformance_test.go
PATH=/home/maxin/.local/go/bin:$PATH go test ./internal/store \
  -run 'Test(Mem|PG)StoreConformance/WorkflowRun' -count=1 -v
PATH=/home/maxin/.local/go/bin:$PATH go test ./internal/store -count=1
```

- [ ] **Step 6: Commit**

```bash
git add runtime/internal/store/workflow_runs.go \
  runtime/internal/store/postgres_workflow_runs.go \
  runtime/internal/store/store.go runtime/internal/store/conformance_test.go
git commit -m "feat(store): add immutable workflow run registry"
```

---

### Task 3: Record Activity and Version-Gated Workflow Boundary

**Files:**
- Modify: `runtime/internal/workflow/types.go`
- Modify: `runtime/internal/workflow/devicetest.go`
- Modify: `runtime/internal/workflow/devicetest_test.go`
- Modify: `runtime/internal/activity/acts.go`
- Create: `runtime/internal/activity/workflow_runs.go`
- Create: `runtime/internal/activity/workflow_runs_test.go`
- Verify unchanged: `runtime/internal/workflow/replay_test.go`
- Verify unchanged: `runtime/internal/workflow/testdata/history-pre-notify-card.json`

- [ ] **Step 1: Write failing activity tests**

Define the workflow-layer request without importing store:

```go
type RecordWorkflowRunRequest struct {
    WorkflowID       string   `json:"workflow_id"`
    Project          string   `json:"project"`
    CommitSHA        string   `json:"commit_sha"`
    PipelineID       int      `json:"pipeline_id"`
    Version          string   `json:"version"`
    RuleVersion      string   `json:"rule_version"`
    Scope            string   `json:"scope"`
    Attempt          int      `json:"attempt"`
    Variants         []string `json:"variants"`
    SourceWorkflowID string   `json:"source_workflow_id,omitempty"`
}
```

Add tests:

```text
TestRecordWorkflowRunPersistsCanonicalRequest
TestRecordWorkflowRunConflictIsNonRetryable
TestRecordWorkflowRunPermanentErrorIsNonRetryable
TestRecordWorkflowRunTransientErrorRemainsRetryable
```

Use `temporal.IsApplicationError`/`ApplicationError.NonRetryable()` to distinguish permanent errors.

- [ ] **Step 2: Write failing workflow-order and retry-isolation tests**

Extend `fakeActs` with:

```go
recordCalls []RecordWorkflowRunRequest
callOrder   []string
recordErrs  []error
selectErr   error
```

Add:

```text
TestWorkflowRecordsRunBeforeSelect
TestWorkflowRecordRetriesPastDefaultMaximum
TestWorkflowRecordPermanentFailureBlocksSelect
TestWorkflowRecordRetryPolicyDoesNotLeakToSelect
TestWorkflowRecordUsesActualExecutionID
```

The transient retry test returns errors on the first four Record calls and succeeds on the fifth,
proving `MaximumAttempts=0`. The isolation test makes Select fail permanently and asserts exactly three
Select attempts under the original policy. Never create a test whose Record always returns a retryable
error without a cancellation deadline.

- [ ] **Step 3: Run RED**

```bash
cd /home/maxin/Code/hermes_ai_devops/.worktrees/workflow-runs/runtime
PATH=/home/maxin/.local/go/bin:$PATH go test ./internal/activity ./internal/workflow \
  -run 'RecordWorkflowRun|WorkflowRecord' -count=1 -v
```

Expected: request/activity is undefined and Select is currently the first activity.

- [ ] **Step 4: Implement the activity adapter**

Add `RecordWorkflowRun` to `activity.Store`. Convert the workflow request to `store.WorkflowRun`.
Map `ErrWorkflowRunConflict` and `ErrWorkflowRunPermanent` to:

```go
return temporal.NewNonRetryableApplicationError(
    "record workflow run: "+err.Error(),
    "WorkflowRunPermanent",
    err,
)
```

Return transient errors wrapped with `%w` so Temporal retries them.

- [ ] **Step 5: Implement the workflow gate**

Add to `DeviceTestInput`:

```go
SourceWorkflowID string `json:"source_workflow_id,omitempty"`
```

After RuleVersion defaulting and validation, add:

```go
if workflow.GetVersion(
    ctx, "record-workflow-run-v1", workflow.DefaultVersion, 1,
) != workflow.DefaultVersion {
    actualID := workflow.GetInfo(ctx).WorkflowExecution.ID
    if actualID != in.WorkflowID() {
        return nil, fmt.Errorf(
            "workflow execution id %q does not match input id %q",
            actualID, in.WorkflowID(),
        )
    }
    recordCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
        StartToCloseTimeout: 30 * time.Second,
        RetryPolicy: &temporal.RetryPolicy{
            InitialInterval: 2 * time.Second,
            BackoffCoefficient: 2,
            MaximumInterval: time.Minute,
            MaximumAttempts: 0,
        },
    })
    req := newRecordWorkflowRunRequest(actualID, in, ruleVersion)
    if err := workflow.ExecuteActivity(
        recordCtx, "RecordWorkflowRun", req,
    ).Get(recordCtx, nil); err != nil {
        return nil, fmt.Errorf("record workflow run: %w", err)
    }
}
```

`newRecordWorkflowRunRequest` copies package variants before sorting/deduplication; it must not reorder
`in.Packages`.

- [ ] **Step 6: Run GREEN plus the immutable replay fixture**

```bash
cd /home/maxin/Code/hermes_ai_devops/.worktrees/workflow-runs/runtime
PATH=/home/maxin/.local/go/bin:$PATH gofmt -w \
  internal/workflow/types.go internal/workflow/devicetest.go \
  internal/workflow/devicetest_test.go internal/activity/acts.go \
  internal/activity/workflow_runs.go internal/activity/workflow_runs_test.go
PATH=/home/maxin/.local/go/bin:$PATH go test ./internal/activity ./internal/workflow -count=1
git diff --exit-code -- runtime/internal/workflow/testdata/history-pre-notify-card.json
```

- [ ] **Step 7: Commit**

```bash
git add runtime/internal/workflow/types.go runtime/internal/workflow/devicetest.go \
  runtime/internal/workflow/devicetest_test.go runtime/internal/activity/acts.go \
  runtime/internal/activity/workflow_runs.go runtime/internal/activity/workflow_runs_test.go
git commit -m "feat(workflow): record authoritative run before device work"
```

---

### Task 4: Authoritative RecentRuns With Legacy Fallback

**Files:**
- Modify: `runtime/internal/store/fleet.go`
- Modify: `runtime/internal/store/postgres_fleet.go`
- Modify: `runtime/internal/store/conformance_test.go`

- [ ] **Step 1: Replace prefix-based tests with failing authoritative tests**

Extend `RecentRun`:

```go
type RecentRun struct {
    WorkflowID   string
    Project      string
    Commit       string
    PipelineID   int
    Version      string
    RuleVersion  string
    Variant      string
    Verdict      string
    EndedAt      time.Time
    Authoritative bool
}
```

Add conformance subtests:

```text
RecentRunsAuthoritativeFirst
RecentRunsExpandsVariantsBeforeLimit
RecentRunsExactTaskAssociation
RecentRunsLegacyFallbackAndGlobalDedup
RecentRunsReturnsDefensiveCopies
```

The limit case must create one newest run with more variants than the limit and another older run; assert
the first run alone fills the result. The fallback test must create a workflow_run excluded by the current
authoritative page and prove its artifact tuple is still excluded from fallback.

- [ ] **Step 2: Run RED**

```bash
cd /home/maxin/Code/hermes_ai_devops/.worktrees/workflow-runs/runtime
PATH=/home/maxin/.local/go/bin:$PATH go test ./internal/store \
  -run 'RecentRuns' -count=1 -v
```

Expected: fields are absent and prefix-associated legacy results violate exact matching/order.

- [ ] **Step 3: Implement MemStore ordering and fallback**

Under one mutex:

1. sort all workflow runs by `runSeq DESC`, then WorkflowID DESC;
2. expand canonical Variants and stop at limit;
3. exact-match tasks by WorkflowID and TestID;
4. build a set of every authoritative `(project,commit,pipeline,variant)` in the store, not just the page;
5. append legacy artifacts by `rowSeq DESC` only when the tuple is absent from that set.

Set authoritative identity/version fields only for workflow-run rows. Legacy rows leave them empty.

- [ ] **Step 4: Implement the PG query**

The authoritative page query must expand before limit:

```sql
SELECT wr.workflow_id, wr.project, wr.commit_sha, wr.pipeline_id,
       wr.version, wr.rule_version, v.variant
FROM workflow_runs wr
CROSS JOIN LATERAL unnest(wr.variants)
    WITH ORDINALITY AS v(variant, ord)
ORDER BY wr.created_at DESC, wr.workflow_id DESC, v.ord
LIMIT $1
```

Join task state by exact workflow/test identity using a lateral query ordered by
`attempt DESC, created_at DESC, task_id DESC`.

The fallback query uses `NOT EXISTS` over all workflow_runs:

```sql
NOT EXISTS (
  SELECT 1
  FROM workflow_runs wr
  WHERE wr.project = a.project
    AND wr.commit_sha = a.commit_sha
    AND wr.pipeline_id = a.pipeline_id
    AND a.variant = ANY(wr.variants)
)
```

Only fetch `limit-len(authoritative)` fallback rows. Do not globally resort the appended legacy rows.

- [ ] **Step 5: Run GREEN and commit**

```bash
cd /home/maxin/Code/hermes_ai_devops/.worktrees/workflow-runs/runtime
PATH=/home/maxin/.local/go/bin:$PATH gofmt -w \
  internal/store/fleet.go internal/store/postgres_fleet.go \
  internal/store/conformance_test.go
PATH=/home/maxin/.local/go/bin:$PATH go test ./internal/store -run 'RecentRuns' -count=1 -v
PATH=/home/maxin/.local/go/bin:$PATH go test ./internal/store -count=1
git add internal/store/fleet.go internal/store/postgres_fleet.go \
  internal/store/conformance_test.go
git commit -m "feat(store): resolve recent runs from workflow registry"
```

---

### Task 5: Temporal Closed-State and Result Reader

**Files:**
- Modify: `runtime/internal/trigger/starter.go`
- Modify: `runtime/internal/trigger/starter_test.go`

- [ ] **Step 1: Write failing integration tests**

Add:

```text
TestTemporalStarterWorkflowClosed
TestTemporalStarterWorkflowClosedRejectsUnspecified
TestTemporalStarterWorkflowPausedIsNotClosed
TestTemporalStarterWorkflowResult
TestTemporalStarterWorkflowResultUnavailable
```

Use the existing Temporal dev server helper. Start a real SDK worker on a dedicated task queue and
register a test workflow that blocks on a `finish` signal before returning:

```go
func inspectableWorkflow(ctx workflow.Context) (*wf.DeviceTestOutput, error) {
    workflow.GetSignalChannel(ctx, "finish").Receive(ctx, nil)
    return &wf.DeviceTestOutput{Tasks: []wf.TaskSummary{
        {Variant: "v1", Verdict: "TEST_FAILED"},
        {Variant: "v2", Verdict: wf.VerdictSkipped},
    }}, nil
}
```

After ExecuteWorkflow, poll Describe with a bounded deadline until the execution is RUNNING, assert
`WorkflowClosed` returns `(false,nil)`, signal `finish`, wait for `run.Get`, then assert closed is true and
the result round-trips exactly. NotFound returns an error.

`UNSPECIFIED` is not constructible from a healthy dev server. For that case use
`go.temporal.io/sdk/mocks.Client`, configure `DescribeWorkflowExecution` to return a response whose status
is `WORKFLOW_EXECUTION_STATUS_UNSPECIFIED`, and assert the method returns an error. Use the same mock path
for PAUSED if the dev server cannot create a paused execution.

- [ ] **Step 2: Run RED**

```bash
cd /home/maxin/Code/hermes_ai_devops/.worktrees/workflow-runs/runtime
PATH=/home/maxin/.local/go/bin:$PATH go test ./internal/trigger \
  -run 'WorkflowClosed|WorkflowResult' -count=1 -v
```

- [ ] **Step 3: Implement exact Temporal inspection**

Add:

```go
func (s *TemporalStarter) WorkflowClosed(
    ctx context.Context, workflowID string,
) (bool, error)

func (s *TemporalStarter) WorkflowResult(
    ctx context.Context, workflowID string,
) (*wf.DeviceTestOutput, error)
```

`WorkflowClosed` calls `DescribeWorkflowExecution(ctx, workflowID, "")`. Return false for `RUNNING` and
`PAUSED`; reject `UNSPECIFIED` with an error; return true for COMPLETED, FAILED, CANCELED, TERMINATED,
TIMED_OUT and CONTINUED_AS_NEW. NotFound and transport errors remain errors.

`WorkflowResult` calls:

```go
var out wf.DeviceTestOutput
if err := s.Client.GetWorkflow(ctx, workflowID, "").Get(ctx, &out); err != nil {
    return nil, fmt.Errorf("get workflow result %s: %w", workflowID, err)
}
return &out, nil
```

- [ ] **Step 4: Run GREEN and commit**

```bash
cd /home/maxin/Code/hermes_ai_devops/.worktrees/workflow-runs/runtime
PATH=/home/maxin/.local/go/bin:$PATH gofmt -w \
  internal/trigger/starter.go internal/trigger/starter_test.go
PATH=/home/maxin/.local/go/bin:$PATH go test ./internal/trigger -count=1
git add internal/trigger/starter.go internal/trigger/starter_test.go
git commit -m "feat(trigger): read closed workflow results"
```

---

### Task 6: Exact Rerun and Final Project-Aware Artifact APIs

**Files:**
- Modify: `runtime/internal/store/store.go`
- Modify: `runtime/internal/store/postgres.go`
- Modify: `runtime/internal/store/fleet.go`
- Modify: `runtime/internal/store/postgres_fleet.go`
- Modify: `runtime/internal/store/conformance_test.go`
- Modify: `runtime/internal/trigger/kick.go`
- Modify: `runtime/internal/trigger/kick_test.go`
- Modify: `runtime/internal/feishucmd/command.go`
- Modify: `runtime/internal/feishucmd/executor.go`
- Modify: `runtime/internal/feishucmd/executor_test.go`
- Modify: `runtime/cmd/worker/main.go`

- [ ] **Step 1: Write failing project-aware artifact tests**

Change final signatures everywhere:

```go
ListArtifacts(ctx, project, commitSHA, pipelineID)
NextWorkflowAttempt(ctx, project, commitSHA, pipelineID, variant)
NextWorkflowAttemptAll(ctx, project, commitSHA, pipelineID)
```

Extend `ListArtifactsAndAttemptAll` with two projects sharing the remaining key and assert each query and
counter changes only its own project.

- [ ] **Step 2: Replace old rerun tests with the exact contract**

The fake starter records call order and implements Start, Closed, and Result. Add table/subtests covering:

```text
LegacyTwoArgsShowsMigration
LegacyThreeArgsShowsMigration
NewOneArgRerunsFailedOutputOnly
NewTwoArgsRerunsExplicitPassedVariant
ExplicitSkippedVariantAllowed
UnknownOrLegacyRunRejected
RunningOrDescribeErrorRejected
WorkflowResultErrorRejectedWithoutVariant
NoFailuresDoesNotAllocateAttempt
MissingArtifactDoesNotAllocateAttempt
ProjectVersionRuleAndSourceAreInherited
PreCreateTaskFailureWithoutTaskIDIsRetried
StaleTaskTableDoesNotOverrideWorkflowOutput
AlreadyStartedIsReportedButNotClaimDedup
```

Use an output where v1 is PASSED, v2 is TEST_FAILED with empty TaskID, v3 is INFRA_ERROR, and v4 is
SKIPPED. The no-variant input must contain only v2/v3 packages in canonical order.

- [ ] **Step 3: Run RED**

```bash
cd /home/maxin/Code/hermes_ai_devops/.worktrees/workflow-runs/runtime
PATH=/home/maxin/.local/go/bin:$PATH go test \
  ./internal/store ./internal/trigger ./internal/feishucmd ./cmd/worker \
  -count=1 -v
```

Expected: interface/signature compilation failures and old rerun semantics.

- [ ] **Step 4: Implement final project-aware artifact methods**

Every Mem/PG WHERE clause includes Project. Bundle attempt still increments all artifact rows for that
project/commit/pipeline and returns the maximum. Update kick to pass `p.Project`.

Delete any transitional project-agnostic helper added in Task 1. No exported project-agnostic List/Next
method may remain.

- [ ] **Step 5: Implement exact rerun**

The old syntax detector runs before GetWorkflowRun:

```go
if len(args) == 3 ||
    (len(args) == 2 && validateSHA(strings.ToLower(args[0])) == nil &&
        isPositiveInt(args[1])) {
    return "旧 rerun 语法已停用，请使用 rerun <source_workflow_id> [variant]", nil
}
if len(args) < 1 || len(args) > 2 {
    return "用法: rerun <source_workflow_id> [variant]", nil
}
```

Execution order:

```text
GetWorkflowRun
WorkflowClosed
[no variant only] WorkflowResult and failed-summary filtering
ListArtifacts(project,commit,pipeline)
target membership and exactly-one-artifact validation
NextWorkflowAttempt or NextWorkflowAttemptAll
StartDeviceTest
```

Build the input only from the source run:

```go
in := wf.DeviceTestInput{
    Project: source.Project, Commit: source.CommitSHA,
    PipelineID: source.PipelineID, Version: source.Version,
    RuleVersion: source.RuleVersion,
    SourceWorkflowID: source.WorkflowID,
}
```

For explicit variant set Scope to that variant and do not call WorkflowResult. For no variant, use
`DeviceTestOutput.Tasks` and select `Verdict != PASSED && Verdict != SKIPPED`; never inspect TaskID or
infer from missing tasks. Preserve source Scope. Filter artifacts to the canonical target set and require
exactly one row per target before allocating attempt.

Do not claim text rerun is click-idempotent. Each explicit command allocates a new attempt; persistent
action claim belongs to the next button round.

Remove `Executor.ExpectedVariants`, its worker wiring, and its package-completeness messages/tests. The
new no-variant contract selects the failed summaries of one exact source run; global configured variant
count is no longer part of rerun validity.

- [ ] **Step 6: Run GREEN**

```bash
cd /home/maxin/Code/hermes_ai_devops/.worktrees/workflow-runs/runtime
PATH=/home/maxin/.local/go/bin:$PATH gofmt -w \
  internal/store/store.go internal/store/postgres.go internal/store/fleet.go \
  internal/store/postgres_fleet.go internal/store/conformance_test.go \
  internal/trigger/kick.go internal/trigger/kick_test.go \
  internal/feishucmd/command.go internal/feishucmd/executor.go \
  internal/feishucmd/executor_test.go cmd/worker/main.go
PATH=/home/maxin/.local/go/bin:$PATH go test \
  ./internal/store ./internal/trigger ./internal/feishucmd ./cmd/worker -count=1
```

- [ ] **Step 7: Commit**

```bash
git add runtime/internal/store runtime/internal/trigger/kick.go \
  runtime/internal/trigger/kick_test.go runtime/internal/feishucmd/command.go \
  runtime/internal/feishucmd/executor.go runtime/internal/feishucmd/executor_test.go \
  runtime/cmd/worker/main.go
git commit -m "feat(feishucmd): rerun exact authoritative workflow inputs"
```

---

### Task 7: NL Translation v2 and Authoritative Snapshot

**Files:**
- Modify: `runtime/internal/feishucmd/translate.go`
- Modify: `runtime/internal/feishucmd/translate_test.go`
- Modify: `runtime/internal/hermesclient/hermesclient.go`
- Modify: `runtime/internal/hermesclient/prompt.go`
- Modify: `runtime/internal/hermesclient/hermesclient_test.go`
- Create: `runtime/internal/hermesclient/prompts/cmd_translate_v2.md`
- Modify: `contracts/command.schema.json`
- Modify: `runtime/internal/hermesclient/command.schema.json`
- Modify: `hermes/analyze_bridge/command.schema.json`
- Modify: `contracts/tests/examples/command/valid/rerun_full.json`
- Modify: `contracts/tests/examples/command/valid/rerun_no_variant.json`
- Modify: all other command fixtures to translation version 2 where applicable
- Create: `contracts/tests/examples/command/invalid/old_translation_version.json`
- Modify: `contracts/tests/test_command_schema.py`
- Modify: `hermes/analyze_bridge/test_analyze_bridge.py`

- [ ] **Step 1: Write failing schema tests and fixtures**

Set valid rerun examples to:

```json
{
  "translation_version": 2,
  "command": "rerun",
  "args": [
    "device-test-grp/algo-super-sdk-g9da3b9d9-p56",
    "aarch64_Android_SNPE_1.68"
  ],
  "confidence": 0.9
}
```

Add tests proving:

- `/` and a 65+ character workflow ID are accepted;
- any whitespace in an arg is rejected;
- translation version 1 is rejected by the current schema;
- rerun accepts exactly 1 or 2 args;
- status/devices/none accept zero args;
- unquarantine accepts zero or one arg;
- all three schema copies are byte-identical.

- [ ] **Step 2: Write failing snapshot and translator tests**

Expand `snapshotRun` golden data with:

```go
WorkflowID    string `json:"workflow_id,omitempty"`
Version       string `json:"version,omitempty"`
RuleVersion   string `json:"rule_version,omitempty"`
Authoritative bool   `json:"authoritative"`
```

Add:

```text
TestTranslateSnapshotCarriesAuthoritativeWorkflowIdentity
TestTranslateAcceptsAuthoritativeWorkflowRerun
TestTranslateRejectsFabricatedWorkflowID
TestTranslateRejectsLegacyFallbackRerun
TestTranslateRejectsVariantOutsideSourceRun
```

`checkArgs` must receive snapshot runs. A rerun workflow ID must match at least one
`Authoritative=true` row; a supplied variant must match an authoritative row with the same WorkflowID.

- [ ] **Step 3: Run RED**

```bash
cd /home/maxin/Code/hermes_ai_devops/.worktrees/workflow-runs
/home/maxin/Code/hermes_ai_devops/.venv/bin/python -m pytest \
  contracts/tests/test_command_schema.py \
  hermes/analyze_bridge/test_analyze_bridge.py -q
cd runtime
PATH=/home/maxin/.local/go/bin:$PATH go test \
  ./internal/hermesclient ./internal/feishucmd -count=1
```

- [ ] **Step 4: Implement command schema v2**

All three schema copies use:

```json
"translation_version": { "const": 2 },
"args": {
  "type": "array",
  "items": {
    "type": "string",
    "minLength": 1,
    "maxLength": 512,
    "not": { "pattern": "\\s" }
  }
}
```

Add Draft 2020-12 `if/then` clauses by command:

```json
{
  "if": {
    "properties": { "command": { "const": "rerun" } },
    "required": ["command"]
  },
  "then": {
    "required": ["args"],
    "properties": { "args": { "minItems": 1, "maxItems": 2 } }
  }
}
```

Repeat with maxItems 0 for status/devices/none and maxItems 1 for unquarantine. Keep
`additionalProperties:false`.

- [ ] **Step 5: Add prompt v2 without rewriting history**

Keep `cmd_translate_v1.md` unchanged. Add v2 describing:

```text
rerun <source_workflow_id> [variant]
```

and requiring workflow ID/variant to come from the same `authoritative:true` recent-run entry. Update:

```go
const PromptVersionTranslate = "cmd_translate_v2"

//go:embed prompts/cmd_translate_v2.md
var PromptTranslate string
```

Change the Translation comment/current contract to version 2. Historical command_translations rows keep
their old `prompt_version=cmd_translate_v1` and remain truthful.

- [ ] **Step 6: Implement snapshot and Runtime validation**

Populate all four new fields from `store.RecentRun`. Legacy rows emit
`authoritative:false` and omit empty identity/version fields. Update `checkArgs` to validate exact
authoritative membership before returning a pending confirmation.

This validation is an early safety gate only; Executor still re-reads WorkflowRun and Temporal state at
execution time.

- [ ] **Step 7: Run GREEN and commit**

```bash
cd /home/maxin/Code/hermes_ai_devops/.worktrees/workflow-runs
/home/maxin/Code/hermes_ai_devops/.venv/bin/python -m pytest \
  contracts/tests/test_command_schema.py \
  hermes/analyze_bridge/test_analyze_bridge.py -q
cd runtime
PATH=/home/maxin/.local/go/bin:$PATH gofmt -w \
  internal/feishucmd/translate.go internal/feishucmd/translate_test.go \
  internal/hermesclient/hermesclient.go internal/hermesclient/prompt.go \
  internal/hermesclient/hermesclient_test.go
PATH=/home/maxin/.local/go/bin:$PATH go test \
  ./internal/hermesclient ./internal/feishucmd -count=1
cd ..
git add contracts/command.schema.json contracts/tests/examples/command \
  contracts/tests/test_command_schema.py \
  runtime/internal/hermesclient/command.schema.json \
  runtime/internal/hermesclient/hermesclient.go \
  runtime/internal/hermesclient/hermesclient_test.go \
  runtime/internal/hermesclient/prompt.go \
  runtime/internal/hermesclient/prompts/cmd_translate_v2.md \
  runtime/internal/feishucmd/translate.go runtime/internal/feishucmd/translate_test.go \
  hermes/analyze_bridge/command.schema.json \
  hermes/analyze_bridge/test_analyze_bridge.py
git commit -m "feat(feishucmd): translate rerun by workflow id"
```

---

### Task 8: Documentation, Deployment Gate, and Final Verification

**Files:**
- Modify: `CLAUDE.md`
- Modify: `docs/superpowers/specs/2026-07-28-feishu-cmd-nl-translate-design.md`
- Modify: `docs/device-test-sequence.md`
- Modify: `deploy/README.md`
- Modify: `deploy/tests/test_deploy_contracts.py`

- [ ] **Step 1: Write failing deployment contract assertions**

Add assertions that:

- migration contains `workflow_runs`, `artifacts_project_key`, and drops the old constraint;
- deploy documentation contains the exact order:
  `prior batch stable -> stop writers -> migrate -> deploy all new binaries -> resume`;
- docs do not advertise `rerun <sha> <pipeline_iid>`.

Do not scan archived implementation plans for obsolete syntax; plans are historical artifacts.

- [ ] **Step 2: Run RED**

```bash
cd /home/maxin/Code/hermes_ai_devops/.worktrees/workflow-runs
/home/maxin/Code/hermes_ai_devops/.venv/bin/python -m pytest \
  deploy/tests/test_deploy_contracts.py -q
```

- [ ] **Step 3: Update operational documentation**

Document:

- new `rerun <source_workflow_id> [variant]` syntax;
- legacy RecentRuns are display-only;
- workflow_runs is immutable and not backfilled;
- production migration is not rolling-compatible;
- the already-merged presign/evidence-v3/attribution batch must be deployed and observed stable first;
- this branch being merged does not authorize production migration;
- migration requires all old artifact writers stopped before the old unique constraint is removed.

Correct the old statement that direct rerun is idempotent through Temporal RejectDuplicate: every command
allocates a new attempt; only the next button round's persistent claim will make click handling idempotent.

- [ ] **Step 4: Run repository-wide verification**

```bash
cd /home/maxin/Code/hermes_ai_devops/.worktrees/workflow-runs/runtime
PATH=/home/maxin/.local/go/bin:$PATH go test ./... -count=1
PATH=/home/maxin/.local/go/bin:$PATH go vet ./...
cd ..
/home/maxin/Code/hermes_ai_devops/.venv/bin/python -m pytest \
  contracts/tests deploy/tests \
  hermes/analyze_bridge/test_analyze_bridge.py -q
git diff --check
git status --short
```

The store suite must actually run PostgreSQL conformance through embedded PG or `TEST_DATABASE_URL`; a
skip is not completion evidence. Temporal starter integration and the old WorkflowReplayer fixture must
also execute, not skip.

- [ ] **Step 5: Run mutation checks for the carrying assertions**

Perform and then revert these local mutations one at a time:

1. remove `project` from one artifact WHERE clause: cross-project conformance must fail;
2. change Record `MaximumAttempts` back to 3: the fifth-attempt workflow test must fail;
3. associate RecentRuns by prefix: adversarial exact-association test must fail;
4. select rerun targets from tasks instead of WorkflowResult: pre-CreateTask failure test must fail;
5. permit command-schema whitespace or version 1: schema fixtures must fail;
6. remove the deployment prerequisite sentence: deploy contract test must fail.

After reverting each mutation, rerun its focused test. Finish with `git diff --check` and a clean status
apart from the intended documentation changes.

- [ ] **Step 6: Commit**

```bash
git add CLAUDE.md docs/device-test-sequence.md \
  docs/superpowers/specs/2026-07-28-feishu-cmd-nl-translate-design.md \
  deploy/README.md deploy/tests/test_deploy_contracts.py
git commit -m "docs: document workflow run migration gate"
```

- [ ] **Step 7: Final branch review**

Dispatch one fresh reviewer against the complete diff from `03cd30a` through HEAD. It must check:

- every approved spec acceptance item has a mechanical test;
- the migration and fresh schema converge;
- old history replay is unchanged and green;
- no action/button/audit_log work leaked into this round;
- production deployment remains explicitly blocked on the prior batch stability gate.

Fix all findings, rerun full verification, and only then use
`superpowers:finishing-a-development-branch` for local fast-forward merge.
