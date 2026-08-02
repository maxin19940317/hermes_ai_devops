package activity

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"

	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

type recordWorkflowRunStore struct {
	Store
	err error
}

func (s *recordWorkflowRunStore) RecordWorkflowRun(_ context.Context, run store.WorkflowRun) error {
	return s.err
}

func recordRequest() wf.RecordWorkflowRunRequest {
	return wf.RecordWorkflowRunRequest{
		WorkflowID:       "device-test-grp/p-gabcd1234-p42-r1",
		Project:          "grp/p",
		CommitSHA:        "abcd1234",
		PipelineID:       42,
		Version:          "1.2.3",
		RuleVersion:      "verdict-rules-v1",
		Scope:            "aarch64",
		Attempt:          1,
		Variants:         []string{"v1", "v2"},
		SourceWorkflowID: "device-test-grp/p-gabcd1234-p42",
	}
}

func TestRecordWorkflowRunPersistsCanonicalRequest(t *testing.T) {
	s := store.NewMemStore()
	a := &Acts{Store: s}
	req := recordRequest()
	req.SourceWorkflowID = ""
	req.Variants = []string{"v2", "", "v1", "v2"}

	if err := a.RecordWorkflowRun(ctx, req); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetWorkflowRun(ctx, req.WorkflowID)
	if err != nil {
		t.Fatal(err)
	}
	got.CreatedAt = time.Time{}
	want := store.WorkflowRun{
		WorkflowID: req.WorkflowID, Project: req.Project, CommitSHA: req.CommitSHA,
		PipelineID: req.PipelineID, Version: req.Version, RuleVersion: req.RuleVersion,
		Scope: req.Scope, Attempt: req.Attempt, Variants: []string{"v1", "v2"},
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("recorded = %+v, want %+v", *got, want)
	}
}

func TestRecordWorkflowRunConflictIsNonRetryable(t *testing.T) {
	assertRecordWorkflowRunError(t, store.ErrWorkflowRunConflict, true)
}

func TestRecordWorkflowRunPermanentErrorIsNonRetryable(t *testing.T) {
	assertRecordWorkflowRunError(t, store.ErrWorkflowRunPermanent, true)
}

func TestRecordWorkflowRunTransientErrorRemainsRetryable(t *testing.T) {
	assertRecordWorkflowRunError(t, errors.New("database unavailable"), false)
}

func assertRecordWorkflowRunError(t *testing.T, cause error, wantNonRetryable bool) {
	t.Helper()
	sentinel := errors.New("sentinel")
	s := &recordWorkflowRunStore{err: errors.Join(cause, sentinel)}
	err := (&Acts{Store: s}).RecordWorkflowRun(ctx, recordRequest())
	if err == nil {
		t.Fatal("RecordWorkflowRun returned nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error does not preserve cause: %v", err)
	}
	if temporal.IsApplicationError(err) != wantNonRetryable {
		t.Fatalf("IsApplicationError(%v) = %v, want %v", err,
			temporal.IsApplicationError(err), wantNonRetryable)
	}
	if wantNonRetryable {
		var appErr *temporal.ApplicationError
		if !errors.As(err, &appErr) {
			t.Fatalf("error is not ApplicationError: %T %v", err, err)
		}
		if !appErr.NonRetryable() || appErr.Type() != "WorkflowRunPermanent" {
			t.Fatalf("application error = nonretryable:%v type:%q",
				appErr.NonRetryable(), appErr.Type())
		}
	}
}
