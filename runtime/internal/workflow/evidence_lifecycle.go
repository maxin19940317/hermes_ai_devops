package workflow

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// EvidenceLifecycleWorkflowName is the workflow type name for the daily MinIO cleanup schedule.
const EvidenceLifecycleWorkflowName = "EvidenceLifecycleWorkflow"

// EvidenceLifecycleWorkflow runs the MinIO lifecycle sweep as a cron-driven maintenance workflow.
// It does not return a business result — the sweep outcome is logged by the activity.
func EvidenceLifecycleWorkflow(ctx workflow.Context) error {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    30 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumAttempts:    3,
		},
	})
	return workflow.ExecuteActivity(ctx, "EvidenceLifecycle").Get(ctx, nil)
}
