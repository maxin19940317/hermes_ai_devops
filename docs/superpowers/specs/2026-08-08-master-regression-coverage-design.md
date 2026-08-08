# Master Regression Coverage Design

**Date:** 2026-08-08
**Status:** Approved for planning

## Goal

Restore the documented localhost-only Grafana boundary and add direct regression
coverage for the Android SoC precheck and Feishu device-card fallback fixes already
present on `master`.

## Scope

This change contains exactly three deliverables:

1. Bind the Compose Grafana port to `127.0.0.1` again.
2. Prove Android precheck accepts `ro.soc.model=SM6225` even when
   `ro.board.platform=bengal` and no alias is configured.
3. Prove a failed Feishu device-card send falls back to a non-empty text reply.

Expanding `scripts/lint.sh` or adding a hosted CI workflow is explicitly out of
scope for this change.

## Design

### Grafana network boundary

`deploy/docker-compose.yml` will publish Grafana as
`127.0.0.1:${GRAFANA_HOST_PORT:-13000}:3000`. The obsolete comment that describes
LAN exposure through `0.0.0.0` will be removed. Existing deployment contracts,
README instructions, and the SSH forwarding workflow remain authoritative and do
not require semantic changes.

### Android SoC precheck regression

The executor test suite will construct an Android package whose manifest requires
`SM6225`. Its fake device will return `SM6225` for `ro.soc.model` and `bengal` for
the platform and board properties. Execution must pass precheck without an alias,
and the recorded environment SoC must be `SM6225`.

The test will exercise the real shared `adb.ProbeAndroidSOC` path through
`Executor.Execute`; it will not test a mock implementation of the probe.

### Device-card fallback regression

The Feishu command test suite will use a sender that implements both text and card
sending, with `SendCard` returning a deterministic error. Executing `devices`
through the real executor must return a non-empty textual device list. The test
will prove the fallback response contains the device identity rather than merely
checking that an error occurred.

## Test sensitivity

Both behavioral fixes already exist on `master`, so newly added regression tests
would otherwise pass on their first run. To verify that each test detects its
original defect, implementation will use mutation checks in the isolated worktree:

1. Temporarily restore the old SoC precheck behavior and confirm the new SoC test
   fails with `soc mismatch`; then restore the current shared-probe implementation.
2. Temporarily restore the old unconditional empty card response and confirm the
   fallback test fails; then restore the current boolean fallback implementation.

No mutation will be committed.

## Verification

Completion requires all of the following on the final tree:

- Targeted Android SoC regression test passes.
- Targeted card fallback regression test passes.
- Runtime and Agent `go test ./...` pass.
- Runtime and Agent `go vet ./...` pass.
- Python contract, CI, deployment, analyze bridge, MCP bridge, workflow bridge,
  and kanban bridge tests pass.
- Critical Runtime and Agent packages pass `go test -race`.
- `gofmt -l` returns no files and `git diff --check` succeeds.

## Non-goals

- No new quality-gate script behavior.
- No GitHub Actions or GitLab CI configuration.
- No change to Grafana authentication, dashboards, or datasource permissions.
- No card-actions branch integration.
