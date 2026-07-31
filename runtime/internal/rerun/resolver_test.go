package rerun

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

var ctx = context.Background()

// fakeStore is a minimal in-memory Store fake: one WorkflowRun keyed by ID,
// plus a flat artifact list filtered by (project, commit, pipeline).
type fakeStore struct {
	t    *testing.T
	runs map[string]store.WorkflowRun
	arts []store.Artifact
}

func newFakeStore(t *testing.T, run store.WorkflowRun, arts ...store.Artifact) *fakeStore {
	t.Helper()
	return &fakeStore{t: t, runs: map[string]store.WorkflowRun{run.WorkflowID: run}, arts: arts}
}

func (s *fakeStore) GetWorkflowRun(_ context.Context, workflowID string) (*store.WorkflowRun, error) {
	run, ok := s.runs[workflowID]
	if !ok {
		return nil, store.ErrWorkflowRunNotFound
	}
	return &run, nil
}

func (s *fakeStore) ListArtifacts(
	_ context.Context, project, commitSHA string, pipelineID int,
) ([]store.Artifact, error) {
	var out []store.Artifact
	for _, a := range s.arts {
		if a.Project == project && a.CommitSHA == commitSHA && a.PipelineID == pipelineID {
			out = append(out, a)
		}
	}
	return out, nil
}

// fakeLookup is a minimal WorkflowLookup fake.
type fakeLookup struct {
	closed      bool
	closedErr   error
	result      *wf.DeviceTestOutput
	resultErr   error
	resultCalls int
}

func (f *fakeLookup) WorkflowClosed(_ context.Context, _ string) (bool, error) {
	return f.closed, f.closedErr
}

func (f *fakeLookup) WorkflowResult(_ context.Context, _ string) (*wf.DeviceTestOutput, error) {
	f.resultCalls++
	return f.result, f.resultErr
}

// run builds a store.WorkflowRun fixture matching the fields ResolveRetry/
// ResolveFailureRun read: identity for a new DeviceTestInput plus membership set.
func run(workflowID, project, commitSHA string, pipelineID int, variants ...string) store.WorkflowRun {
	return store.WorkflowRun{
		WorkflowID: workflowID, Project: project, CommitSHA: commitSHA, PipelineID: pipelineID,
		Version: "1.2.3", RuleVersion: "verdict-rules-v7", Scope: "source", Variants: variants,
	}
}

// art builds a store.Artifact fixture that matches run("wf1", "grp/p", "abcd1234", 42, ...)'s
// (project, commit, pipeline) so fakeStore.ListArtifacts returns it for that run.
func art(variant string) store.Artifact {
	return store.Artifact{
		Project: "grp/p", CommitSHA: "abcd1234", PipelineID: 42, Variant: variant,
		URL: "https://example/" + variant, SHA256: "sha-" + variant,
	}
}

// TestExplicitVariantSkipsWorkflowResult: an explicit variant is the user's
// explicit choice, so it only checks membership and artifact presence, never
// reads Temporal output — which is why it may re-run a PASSED/SKIPPED variant.
func TestExplicitVariantSkipsWorkflowResult(t *testing.T) {
	st := newFakeStore(t, run("wf1", "grp/p", "abcd1234", 42, "v1", "v2"), art("v1"), art("v2"))
	lookup := &fakeLookup{closed: true, resultErr: errors.New("must not be called")}
	r := &Resolver{Store: st, Starter: lookup}

	res, err := r.ResolveRetry(ctx, "wf1", "v1")
	if err != nil {
		t.Fatalf("ResolveRetry: %v", err)
	}
	if lookup.resultCalls != 0 {
		t.Fatalf("WorkflowResult 被调用 %d 次,显式模式必须零调用", lookup.resultCalls)
	}
	if !reflect.DeepEqual(res.Targets, []string{"v1"}) || res.Scope != "v1" {
		t.Fatalf("targets=%v scope=%q", res.Targets, res.Scope)
	}
}

// TestEmptyVariantFiltersDedupesSorts: empty-variant mode derives the target
// set from the output, ignoring empty Variant, deduping, and sorting.
func TestEmptyVariantFiltersDedupesSorts(t *testing.T) {
	st := newFakeStore(t, run("wf1", "grp/p", "abcd1234", 42, "v1", "v2"), art("v1"), art("v2"))
	lookup := &fakeLookup{closed: true, result: &wf.DeviceTestOutput{Tasks: []wf.TaskSummary{
		{Variant: "v2", Verdict: "TEST_FAILED"},
		{Variant: "", Verdict: "INFRA_ERROR"},   // empty Variant must be ignored
		{Variant: "v2", Verdict: "INFRA_ERROR"}, // duplicate must be deduped
		{Variant: "v1", Verdict: "INFRA_ERROR"},
		{Variant: "v3", Verdict: "PASSED"}, // excluded
		{Variant: "v4", Verdict: wf.VerdictSkipped},
	}}}
	r := &Resolver{Store: st, Starter: lookup}

	res, err := r.ResolveRetry(ctx, "wf1", "")
	if err != nil {
		t.Fatalf("ResolveRetry: %v", err)
	}
	if !reflect.DeepEqual(res.Targets, []string{"v1", "v2"}) {
		t.Fatalf("targets = %v, want [v1 v2]", res.Targets)
	}
}

// TestArtifactMissingCarriesCount: the text-rerun reply
// "变体 %s 的 artifact 数量为 %d，要求恰好 1 个" needs Count — a bare enum
// value cannot reproduce it.
func TestArtifactMissingCarriesCount(t *testing.T) {
	st := newFakeStore(t, run("wf1", "grp/p", "abcd1234", 42, "v1")) // no artifact
	r := &Resolver{Store: st, Starter: &fakeLookup{closed: true}}

	_, err := r.ResolveRetry(ctx, "wf1", "v1")
	var reason *RejectReason
	if !errors.As(err, &reason) {
		t.Fatalf("err = %v, want *RejectReason", err)
	}
	if reason.Code != "ArtifactMissing" || reason.Variant != "v1" || reason.Count != 0 {
		t.Fatalf("reason = %#v", reason)
	}
}

// TestResolveFailureRunIgnoresArtifacts: ignore is a pure record action, so it
// must still succeed even when every artifact is missing — decoupled from
// retry (design §4.0).
func TestResolveFailureRunIgnoresArtifacts(t *testing.T) {
	st := newFakeStore(t, run("wf1", "grp/p", "abcd1234", 42, "v1")) // no artifact
	lookup := &fakeLookup{closed: true, result: &wf.DeviceTestOutput{
		Tasks: []wf.TaskSummary{{Variant: "v1", Verdict: "TEST_FAILED"}}}}
	r := &Resolver{Store: st, Starter: lookup}

	fr, err := r.ResolveFailureRun(ctx, "wf1")
	if err != nil {
		t.Fatalf("ResolveFailureRun 不应因缺 artifact 失败: %v", err)
	}
	if !reflect.DeepEqual(fr.Targets, []string{"v1"}) {
		t.Fatalf("targets = %v", fr.Targets)
	}
	// The same scenario must reject retry.
	if _, err := r.ResolveRetry(ctx, "wf1", ""); err == nil {
		t.Fatal("ResolveRetry 缺 artifact 时必须失败")
	}
}

// TestNotAuthoritativeRejected: an unknown workflow ID must reject before
// ever touching Starter.
func TestNotAuthoritativeRejected(t *testing.T) {
	st := newFakeStore(t, run("wf1", "grp/p", "abcd1234", 42, "v1"))
	lookup := &fakeLookup{closed: true, closedErr: errors.New("must not be called")}
	r := &Resolver{Store: st, Starter: lookup}

	_, err := r.ResolveRetry(ctx, "unknown", "")
	var reason *RejectReason
	if !errors.As(err, &reason) || reason.Code != "NotAuthoritative" {
		t.Fatalf("err = %v, want RejectReason{NotAuthoritative}", err)
	}
}

// TestStillRunningRejected: an open workflow must reject before reading
// Temporal output.
func TestStillRunningRejected(t *testing.T) {
	st := newFakeStore(t, run("wf1", "grp/p", "abcd1234", 42, "v1"))
	lookup := &fakeLookup{closed: false, resultErr: errors.New("must not be called")}
	r := &Resolver{Store: st, Starter: lookup}

	_, err := r.ResolveRetry(ctx, "wf1", "")
	var reason *RejectReason
	if !errors.As(err, &reason) || reason.Code != "StillRunning" {
		t.Fatalf("err = %v, want RejectReason{StillRunning}", err)
	}
}

// TestVariantNotMemberRejected: an explicit variant outside the source run's
// Variants set must reject at membership check, before artifact resolution.
func TestVariantNotMemberRejected(t *testing.T) {
	st := newFakeStore(t, run("wf1", "grp/p", "abcd1234", 42, "v1"), art("v1"), art("v2"))
	r := &Resolver{Store: st, Starter: &fakeLookup{closed: true}}

	_, err := r.ResolveRetry(ctx, "wf1", "v2")
	var reason *RejectReason
	if !errors.As(err, &reason) || reason.Code != "VariantNotMember" || reason.Variant != "v2" {
		t.Fatalf("err = %v, want RejectReason{VariantNotMember, v2}", err)
	}
}
