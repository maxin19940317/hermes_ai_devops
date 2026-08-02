package activity

import (
	"context"
	"errors"
	"fmt"

	"go.temporal.io/sdk/temporal"

	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

// RecordWorkflowRun persists the immutable identity of a workflow before any
// device-selection work begins.
func (a *Acts) RecordWorkflowRun(ctx context.Context, req wf.RecordWorkflowRunRequest) error {
	err := a.Store.RecordWorkflowRun(ctx, store.WorkflowRun{
		WorkflowID:       req.WorkflowID,
		Project:          req.Project,
		CommitSHA:        req.CommitSHA,
		PipelineID:       req.PipelineID,
		Version:          req.Version,
		RuleVersion:      req.RuleVersion,
		Scope:            req.Scope,
		Attempt:          req.Attempt,
		Variants:         req.Variants,
		SourceWorkflowID: req.SourceWorkflowID,
	})
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrWorkflowRunConflict) ||
		errors.Is(err, store.ErrWorkflowRunPermanent) {
		return temporal.NewNonRetryableApplicationError(
			"record workflow run: "+err.Error(),
			"WorkflowRunPermanent",
			err,
		)
	}
	return fmt.Errorf("record workflow run: %w", err)
}
