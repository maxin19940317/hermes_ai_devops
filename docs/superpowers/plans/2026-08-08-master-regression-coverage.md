# Master Regression Coverage Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Restore localhost-only Grafana publishing and lock the existing SoC-precheck and Feishu-card fallback fixes behind direct regression tests.

**Architecture:** Keep the production changes minimal: one Compose port correction and no behavioral refactor of the two already-fixed Go paths. Add black-box regression tests through `Executor.Execute` and `Executor.devices`, then prove test sensitivity by temporarily restoring each old defect in the isolated worktree and observing the targeted test fail.

**Tech Stack:** Docker Compose YAML, Go 1.23 tests, Python pytest deployment contracts, Git.

---

### Task 1: Restore the Grafana localhost boundary

**Files:**
- Modify: `deploy/docker-compose.yml:279-282`
- Test: `deploy/tests/test_deploy_contracts.py`

- [ ] **Step 1: Run the existing failing deployment contracts**

Run:

```bash
.venv/bin/python -m pytest -q \
  deploy/tests/test_deploy_contracts.py::ComposeContracts::test_compose_never_publishes_internal_ports \
  deploy/tests/test_deploy_contracts.py::GrafanaConfigContracts::test_compose_grafana_localhost_only
```

Expected: both tests fail because Compose contains `0.0.0.0:${GRAFANA_HOST_PORT:-13000}:3000`.

- [ ] **Step 2: Restore the declared network boundary**

Replace the Grafana `ports` block with:

```yaml
    ports:
      - "127.0.0.1:${GRAFANA_HOST_PORT:-13000}:3000"
```

- [ ] **Step 3: Verify the deployment contracts pass**

Run the Step 1 command again.

Expected: `2 passed`.

- [ ] **Step 4: Commit the boundary fix**

```bash
git add deploy/docker-compose.yml
git commit -m "fix(deploy): restore localhost-only Grafana"
```

### Task 2: Cover real-model Android SoC precheck

**Files:**
- Modify: `agent/internal/executor/executor_test.go:34-80`
- Test: `agent/internal/executor/executor_test.go`

- [ ] **Step 1: Parameterize the package test helper**

Keep existing callers unchanged by making `buildPackage` delegate to a new helper:

```go
func buildPackage(t *testing.T, timeoutSec int) string {
	return buildPackageForSOC(t, timeoutSec, "QCM6125")
}

func buildPackageForSOC(t *testing.T, timeoutSec int, soc string) string {
	t.Helper()
	manifest := fmt.Sprintf(`manifest_version: 1
artifact: {project: p, commit: deadbee1, pipeline_id: 1, platform: aarch64_Android_SNPE_2.21, build_type: Release}
requirements: {os: android, abi: arm64-v8a, soc: [%s], min_free_storage_mb: 100}
deploy:
  workdir: %s
  files:
    - {src: run.sh, dst: run.sh, mode: "0755", sha256: %s}
  env: {LD_LIBRARY_PATH: "{workdir}/lib"}
test:
  entry: ./run.sh
  args: ["--suite", "s"]
  timeout_sec: %d
  success: {exit_code: 0, require_files: [results/result.json]}
collect: [results/result.json, results/*.json, logs/*.log]
cleanup: {remove_workdir: true, keep_on_failure: true}
`, soc, workdir, sha256hex(runSh), timeoutSec)

	path := filepath.Join(t.TempDir(), "pkg.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, e := range map[string]struct {
		data string
		mode int64
	}{
		"run.sh":        {runSh, 0o755},
		"manifest.yaml": {manifest, 0o644},
	} {
		hdr := &tar.Header{Name: name, Size: int64(len(e.data)), Mode: e.mode}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
```

- [ ] **Step 2: Add the regression test**

Add after the existing precheck tests:

```go
func TestPrecheckUsesRealSOCModelBeforePlatform(t *testing.T) {
	props := defaultProps()
	props["ro.soc.model"] = "SM6225"
	props["ro.board.platform"] = "bengal"
	props["ro.product.board"] = "bengal"
	f := &fakeADB{props: props, dfAvailKB: 1 << 20}
	e, _ := newExecutor(f)

	sum, err := e.Execute(context.Background(), Options{
		PackagePath: buildPackageForSOC(t, 900, "SM6225"),
		Serial: serial,
		OutDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("real SoC model should satisfy precheck without alias: %v", err)
	}
	if got := sum.Environment["soc"]; got != "SM6225" {
		t.Fatalf("environment soc = %q, want SM6225", got)
	}
}
```

- [ ] **Step 3: Prove the test catches the old defect**

In the isolated worktree only, temporarily replace the shared-probe call in
`agent/internal/executor/executor.go` with the old platform/board-only lookup.

Run:

```bash
cd agent
go test -count=1 ./internal/executor -run TestPrecheckUsesRealSOCModelBeforePlatform
```

Expected: FAIL containing `soc mismatch`.

Restore `agent/internal/executor/executor.go` to the current shared
`adb.ProbeAndroidSOC` implementation without committing the mutation.

- [ ] **Step 4: Verify the final regression test passes**

Run the Step 3 command again.

Expected: PASS.

- [ ] **Step 5: Commit the SoC regression coverage**

```bash
git add agent/internal/executor/executor_test.go
git commit -m "test(agent): cover real SoC model precheck"
```

### Task 3: Cover failed device-card text fallback

**Files:**
- Modify: `runtime/internal/feishucmd/devices_card_test.go`
- Test: `runtime/internal/feishucmd/devices_card_test.go`

- [ ] **Step 1: Add a failing-card sender test double**

Extend the imports with `context`, `errors`, and add:

```go
type failingDeviceCardSender struct {
	err   error
	texts []string
}

func (s *failingDeviceCardSender) SendText(_ context.Context, text string) error {
	s.texts = append(s.texts, text)
	return nil
}

func (s *failingDeviceCardSender) SendCard(context.Context, any) error {
	return s.err
}
```

- [ ] **Step 2: Add the fallback regression test**

```go
func TestDevicesFallsBackToTextWhenCardSendFails(t *testing.T) {
	st := store.NewMemStore()
	if err := st.UpsertClientDevices(ctx, store.Client{ClientID: "c1"}, []store.Device{{
		DeviceID: "dev-1", Serial: "dev-1", DisplayName: "SM6225-dev-1",
		ClientID: "c1", SOC: "SM6225",
	}}); err != nil {
		t.Fatal(err)
	}
	sender := &failingDeviceCardSender{err: errors.New("card API unavailable")}
	e := &Executor{Store: st, CardSender: sender}

	got, err := e.devices(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got == "" || !strings.Contains(got, "SM6225-dev-1") {
		t.Fatalf("fallback reply = %q, want non-empty device text", got)
	}
}
```

- [ ] **Step 3: Prove the test catches the old defect**

In the isolated worktree only, temporarily change the successful card branch in
`runtime/internal/feishucmd/executor.go` to call `replyCard` and return `"", nil`
without checking its boolean result.

Run:

```bash
cd runtime
go test -count=1 ./internal/feishucmd -run TestDevicesFallsBackToTextWhenCardSendFails
```

Expected: FAIL with `fallback reply = ""`.

Restore the current boolean fallback implementation without committing the mutation.

- [ ] **Step 4: Verify the final regression test passes**

Run the Step 3 command again.

Expected: PASS.

- [ ] **Step 5: Commit the card fallback coverage**

```bash
git add runtime/internal/feishucmd/devices_card_test.go
git commit -m "test(feishu): cover device card text fallback"
```

### Task 4: Run repository verification

**Files:**
- Verify: all changed files and repository test suites

- [ ] **Step 1: Format and whitespace checks**

```bash
gofmt -w agent/internal/executor/executor_test.go runtime/internal/feishucmd/devices_card_test.go
test -z "$(gofmt -l runtime agent)"
git diff --check
```

Expected: no output and exit code 0.

- [ ] **Step 2: Run Go tests and vet**

```bash
(cd runtime && go test -count=1 ./... && go vet ./...)
(cd agent && go test -count=1 ./... && go vet ./...)
```

Expected: all packages pass.

- [ ] **Step 3: Run Python repository tests**

```bash
.venv/bin/python -m pytest -q \
  contracts/tests ci/tests deploy/tests \
  hermes/analyze_bridge/test_analyze_bridge.py \
  hermes/mcp_bridge/test_mcp_bridge.py \
  hermes/kanban_bridge/test_kanban_bridge.py
```

Expected: all tests pass; the previous two Grafana contract failures are gone.

- [ ] **Step 4: Run targeted race tests**

```bash
(cd runtime && go test -race -count=1 ./internal/feishucmd ./internal/callbacks ./internal/store ./internal/workflow)
(cd agent && go test -race -count=1 ./internal/adb ./internal/executor ./internal/reporter ./internal/server)
```

Expected: all targeted packages pass with no race report.

- [ ] **Step 5: Verify final branch state**

```bash
git status --short --branch
git log --oneline origin/master..HEAD
```

Expected: clean `fix/master-regression-coverage` branch containing the design and three implementation commits.
