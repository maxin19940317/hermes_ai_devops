package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"
)

var (
	ErrWorkflowRunNotFound  = errors.New("workflow run not found")
	ErrWorkflowRunConflict  = errors.New("workflow run immutable content conflict")
	ErrWorkflowRunPermanent = errors.New("workflow run permanent error")
)

type WorkflowRun struct {
	WorkflowID       string
	Project          string
	CommitSHA        string
	PipelineID       int
	Version          string
	RuleVersion      string
	Scope            string
	Attempt          int
	Variants         []string
	SourceWorkflowID string
	CreatedAt        time.Time
}

func canonicalWorkflowRun(run WorkflowRun) (WorkflowRun, error) {
	if run.WorkflowID == "" || run.Project == "" || run.CommitSHA == "" ||
		run.Version == "" || run.RuleVersion == "" {
		return WorkflowRun{}, fmt.Errorf("%w: required field is empty", ErrWorkflowRunPermanent)
	}
	if run.PipelineID <= 0 {
		return WorkflowRun{}, fmt.Errorf("%w: pipeline_id must be positive", ErrWorkflowRunPermanent)
	}
	if run.Attempt < 0 {
		return WorkflowRun{}, fmt.Errorf("%w: attempt must be non-negative", ErrWorkflowRunPermanent)
	}
	if run.SourceWorkflowID == run.WorkflowID {
		return WorkflowRun{}, fmt.Errorf("%w: source workflow must differ from workflow", ErrWorkflowRunPermanent)
	}

	variants := append([]string(nil), run.Variants...)
	sort.Strings(variants)
	out := variants[:0]
	for _, variant := range variants {
		if variant == "" || (len(out) > 0 && out[len(out)-1] == variant) {
			continue
		}
		out = append(out, variant)
	}
	run.Variants = make([]string, len(out))
	copy(run.Variants, out)
	return run, nil
}

func workflowRunImmutableEqual(a, b WorkflowRun) bool {
	a.CreatedAt = time.Time{}
	b.CreatedAt = time.Time{}
	return reflect.DeepEqual(a, b)
}

func cloneWorkflowRun(run WorkflowRun) WorkflowRun {
	variants := make([]string, len(run.Variants))
	copy(variants, run.Variants)
	run.Variants = variants
	return run
}

func (s *MemStore) RecordWorkflowRun(_ context.Context, run WorkflowRun) error {
	canonical, err := canonicalWorkflowRun(run)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.workflowRuns[canonical.WorkflowID]; ok {
		if workflowRunImmutableEqual(existing, canonical) {
			return nil
		}
		return fmt.Errorf("%w: %s", ErrWorkflowRunConflict, canonical.WorkflowID)
	}
	if canonical.SourceWorkflowID != "" {
		if _, ok := s.workflowRuns[canonical.SourceWorkflowID]; !ok {
			return fmt.Errorf("%w: source workflow %s not found",
				ErrWorkflowRunPermanent, canonical.SourceWorkflowID)
		}
	}
	s.seq++
	canonical.CreatedAt = time.Now().UTC()
	s.workflowRuns[canonical.WorkflowID] = cloneWorkflowRun(canonical)
	s.runSeq[canonical.WorkflowID] = s.seq
	return nil
}

func (s *MemStore) GetWorkflowRun(_ context.Context, workflowID string) (*WorkflowRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.workflowRuns[workflowID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrWorkflowRunNotFound, workflowID)
	}
	out := cloneWorkflowRun(run)
	return &out, nil
}
