package cardaction

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/rs/zerolog"
	"go.temporal.io/api/serviceerror"

	"hermes-devops/runtime/internal/rerun"
	"hermes-devops/runtime/internal/store"
	"hermes-devops/runtime/internal/trigger"
	wf "hermes-devops/runtime/internal/workflow"
)

const consumerLeaseTTL = 120 * time.Second

// ConsumerStore is the asynchronous card action consumer's persistence surface.
type ConsumerStore interface {
	ClaimInbox(ctx context.Context, eventID, token string, lease time.Duration) (*store.InboxRow, error)
	GetCardAction(ctx context.Context, workflowID string) (*store.CardAction, error)
	CompleteAccept(ctx context.Context, req store.AcceptRequest) (*store.AcceptOutcome, error)
	CompleteReject(ctx context.Context, eventID, token string, render store.RejectRender) error
	FinalizeAction(ctx context.Context, workflowID, token, state, lastErr string) (bool, error)
}

// ConsumerResolver is the read-only rerun resolution surface used by card actions.
type ConsumerResolver interface {
	ResolveFailureRun(ctx context.Context, workflowID string) (*rerun.FailureRun, error)
	ResolveRetry(ctx context.Context, workflowID, variant string) (*rerun.Resolution, error)
}

// Consumer claims durable clicks, resolves their target, and starts accepted retries.
type Consumer struct {
	Store    ConsumerStore
	Resolver ConsumerResolver
	Starter  trigger.WorkflowStarter
	Log      *zerolog.Logger

	mutateInput func(*wf.DeviceTestInput)
}

// ConsumeOne consumes one durable inbox event. A non-claimable event is an
// idempotent no-op because another worker owns it or it is already processed.
func (c *Consumer) ConsumeOne(ctx context.Context, eventID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if c == nil || c.Store == nil {
		return errors.New("consume card action: store is nil")
	}
	if c.Resolver == nil {
		return errors.New("consume card action: resolver is nil")
	}

	token, err := newFencingToken()
	if err != nil {
		return fmt.Errorf("consume card action %s: create inbox fencing token: %w", eventID, err)
	}
	row, err := c.Store.ClaimInbox(ctx, eventID, token, consumerLeaseTTL)
	if errors.Is(err, store.ErrInboxNotClaimable) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("consume card action %s: claim inbox: %w", eventID, err)
	}
	if row == nil {
		return fmt.Errorf("consume card action %s: claim inbox returned nil row", eventID)
	}

	switch row.Action {
	case "ignore":
		return c.consumeIgnore(ctx, row, token)
	case "retry":
		return c.consumeRetry(ctx, row, token)
	default:
		return fmt.Errorf("consume card action %s: unsupported persisted action %q", eventID, row.Action)
	}
}

func (c *Consumer) consumeIgnore(ctx context.Context, row *store.InboxRow, token string) error {
	if _, err := c.Resolver.ResolveFailureRun(ctx, row.WorkflowID); err != nil {
		return c.completeRejection(ctx, row, token, err)
	}
	out, err := c.Store.CompleteAccept(ctx, acceptRequest(row, token, ""))
	if err != nil {
		return fmt.Errorf("consume card action %s: complete ignore: %w", row.EventID, err)
	}
	if out == nil {
		return fmt.Errorf("consume card action %s: complete ignore returned nil outcome", row.EventID)
	}
	switch out.Kind {
	case "accepted", "conflict", "legacy", "lost":
		return nil
	default:
		return fmt.Errorf("consume card action %s: unexpected ignore outcome %q", row.EventID, out.Kind)
	}
}

func (c *Consumer) consumeRetry(ctx context.Context, row *store.InboxRow, token string) error {
	existing, err := c.Store.GetCardAction(ctx, row.WorkflowID)
	firstAccept := errors.Is(err, store.ErrCardActionNotFound)
	if err != nil && !firstAccept {
		return fmt.Errorf(
			"consume card action %s: get existing retry action: %w",
			row.EventID, err,
		)
	}
	if err == nil && existing == nil {
		return fmt.Errorf(
			"consume card action %s: get existing retry action returned nil",
			row.EventID,
		)
	}

	actionToken, err := newFencingToken()
	if err != nil {
		return fmt.Errorf("consume card action %s: create action fencing token: %w", row.EventID, err)
	}
	req := acceptRequest(row, token, actionToken)
	if firstAccept {
		resolution, err := c.Resolver.ResolveRetry(ctx, row.WorkflowID, "")
		if err != nil {
			return c.completeRejection(ctx, row, token, err)
		}
		if resolution == nil {
			return fmt.Errorf("consume card action %s: retry resolver returned nil resolution", row.EventID)
		}
		if resolution.Run.WorkflowID != row.WorkflowID {
			return fmt.Errorf(
				"consume card action %s: resolver source workflow %q does not match claimed inbox %q",
				row.EventID, resolution.Run.WorkflowID, row.WorkflowID,
			)
		}
		req.Project = resolution.Run.Project
		req.CommitSHA = resolution.Run.CommitSHA
		req.PipelineID = resolution.Run.PipelineID
		req.BuildTarget = func(attempt int) ([]byte, string, error) {
			built := buildTargetInput(resolution, attempt)
			if c.mutateInput != nil {
				c.mutateInput(&built)
			}
			if err := assertTargetInput(built, resolution, attempt); err != nil {
				return nil, "", fmt.Errorf("target input mismatch (implementation defect): %w", err)
			}
			raw, err := json.Marshal(built)
			if err != nil {
				return nil, "", fmt.Errorf("marshal target input: %w", err)
			}
			return raw, built.WorkflowID(), nil
		}
	}
	if c.Starter == nil {
		return fmt.Errorf("consume card action %s: starter is nil", row.EventID)
	}

	out, err := c.Store.CompleteAccept(ctx, req)
	if err != nil {
		return fmt.Errorf("consume card action %s: complete retry: %w", row.EventID, err)
	}
	if out == nil {
		return fmt.Errorf("consume card action %s: complete retry returned nil outcome", row.EventID)
	}
	switch out.Kind {
	case "conflict", "legacy", "lost":
		return nil
	case "accepted", "resumed":
	default:
		return fmt.Errorf("consume card action %s: unexpected retry outcome %q", row.EventID, out.Kind)
	}

	input, err := retryInputFromOutcome(row, out)
	if err != nil {
		return fmt.Errorf(
			"consume card action %s: invalid %s store outcome: %w",
			row.EventID, out.Kind, err,
		)
	}
	_, _, startErr := c.Starter.StartDeviceTest(ctx, input)
	state, lastErr := "succeeded", ""
	if startErr != nil {
		if !isPermanentStartError(startErr) {
			return fmt.Errorf("consume card action %s: start retry workflow: %w", row.EventID, startErr)
		}
		state, lastErr = "failed", startErr.Error()
	}
	ok, finalizeErr := c.Store.FinalizeAction(
		ctx, row.WorkflowID, out.ActionToken, state, lastErr,
	)
	if finalizeErr != nil {
		return fmt.Errorf("consume card action %s: finalize retry action: %w", row.EventID, finalizeErr)
	}
	if !ok && c.Log != nil {
		c.Log.Warn().
			Str("event_id", row.EventID).
			Str("workflow_id", row.WorkflowID).
			Msg("finalize card action lost fencing; sweep will take over")
	}
	return nil
}

func retryInputFromOutcome(
	row *store.InboxRow, out *store.AcceptOutcome,
) (wf.DeviceTestInput, error) {
	if out.ActionToken == "" {
		return wf.DeviceTestInput{}, errors.New("action token is empty")
	}
	if out.Attempt <= 0 {
		return wf.DeviceTestInput{}, fmt.Errorf("attempt = %d, want positive", out.Attempt)
	}
	if out.TargetWorkflowID == "" {
		return wf.DeviceTestInput{}, errors.New("target workflow id is empty")
	}
	if len(out.TargetInput) == 0 {
		return wf.DeviceTestInput{}, errors.New("target input is empty")
	}
	var input wf.DeviceTestInput
	if err := json.Unmarshal(out.TargetInput, &input); err != nil {
		return wf.DeviceTestInput{}, fmt.Errorf("decode target input: %w", err)
	}
	if input.Attempt != out.Attempt {
		return wf.DeviceTestInput{}, fmt.Errorf(
			"target input attempt = %d, outcome attempt = %d",
			input.Attempt, out.Attempt,
		)
	}
	if got := input.WorkflowID(); got != out.TargetWorkflowID {
		return wf.DeviceTestInput{}, fmt.Errorf(
			"target input workflow id = %q, outcome target = %q",
			got, out.TargetWorkflowID,
		)
	}
	if input.SourceWorkflowID != row.WorkflowID {
		return wf.DeviceTestInput{}, fmt.Errorf(
			"target input source workflow = %q, claimed source = %q",
			input.SourceWorkflowID, row.WorkflowID,
		)
	}
	return input, nil
}

func (c *Consumer) completeRejection(
	ctx context.Context, row *store.InboxRow, token string, cause error,
) error {
	var reason *rerun.RejectReason
	if !errors.As(cause, &reason) {
		return fmt.Errorf("consume card action %s: resolve %s: %w", row.EventID, row.Action, cause)
	}
	// CONTRACT-ISSUE: rerun preserves the legacy CheckFailed distinction, while
	// card rejection storage has a closed five-code contract. Treat it as a
	// transient resolution failure rather than silently renaming the reason.
	if reason.Code == "CheckFailed" {
		return fmt.Errorf("consume card action %s: check workflow state: %w", row.EventID, cause)
	}
	render, err := renderRejectReason(reason)
	if err != nil {
		return fmt.Errorf("consume card action %s: render rejection: %w", row.EventID, err)
	}
	if err := c.Store.CompleteReject(ctx, row.EventID, token, render); err != nil {
		return fmt.Errorf("consume card action %s: complete rejection: %w", row.EventID, err)
	}
	return nil
}

func acceptRequest(row *store.InboxRow, token, actionToken string) store.AcceptRequest {
	return store.AcceptRequest{
		EventID: row.EventID, Token: token, WorkflowID: row.WorkflowID,
		Action: row.Action, ActorOpenID: row.ActorOpenID,
		OpenMessageID: row.OpenMessageID, PayloadDigest: row.PayloadDigest,
		ActionToken: actionToken,
	}
}

func buildTargetInput(resolution *rerun.Resolution, attempt int) wf.DeviceTestInput {
	return wf.DeviceTestInput{
		Project: resolution.Run.Project, Commit: resolution.Run.CommitSHA,
		PipelineID: resolution.Run.PipelineID, Version: resolution.Run.Version,
		RuleVersion: resolution.Run.RuleVersion,
		Packages:    append([]wf.PackageRef(nil), resolution.Packages...),
		Scope:       resolution.Scope, Attempt: attempt,
		SourceWorkflowID: resolution.Run.WorkflowID,
	}
}

func assertTargetInput(
	input wf.DeviceTestInput, resolution *rerun.Resolution, attempt int,
) error {
	if input.Project != resolution.Run.Project {
		return fmt.Errorf("project = %q, want %q", input.Project, resolution.Run.Project)
	}
	if input.Commit != resolution.Run.CommitSHA {
		return fmt.Errorf("commit = %q, want %q", input.Commit, resolution.Run.CommitSHA)
	}
	if input.PipelineID != resolution.Run.PipelineID {
		return fmt.Errorf("pipeline_id = %d, want %d", input.PipelineID, resolution.Run.PipelineID)
	}
	if input.Version != resolution.Run.Version {
		return fmt.Errorf("version = %q, want %q", input.Version, resolution.Run.Version)
	}
	if input.RuleVersion != resolution.Run.RuleVersion {
		return fmt.Errorf("rule_version = %q, want %q", input.RuleVersion, resolution.Run.RuleVersion)
	}
	if !slices.Equal(input.Packages, resolution.Packages) {
		return fmt.Errorf("packages differ from resolution")
	}
	if input.Scope != resolution.Scope {
		return fmt.Errorf("scope = %q, want %q", input.Scope, resolution.Scope)
	}
	if input.SourceWorkflowID != resolution.Run.WorkflowID {
		return fmt.Errorf(
			"source_workflow_id = %q, want %q",
			input.SourceWorkflowID, resolution.Run.WorkflowID,
		)
	}
	if input.Attempt != attempt {
		return fmt.Errorf("attempt = %d, want %d", input.Attempt, attempt)
	}
	expected := buildTargetInput(resolution, attempt)
	if input.WorkflowID() != expected.WorkflowID() {
		return fmt.Errorf("workflow_id = %q, want %q", input.WorkflowID(), expected.WorkflowID())
	}
	return nil
}

func renderRejectReason(reason *rerun.RejectReason) (store.RejectRender, error) {
	if reason == nil {
		return store.RejectRender{}, errors.New("nil reject reason")
	}
	var text string
	switch reason.Code {
	case "NotAuthoritative":
		text = fmt.Sprintf("查无权威 workflow 运行记录: %s", reason.WorkflowID)
	case "StillRunning":
		text = fmt.Sprintf("workflow 尚未结束: %s", reason.WorkflowID)
	case "ResultUnreadable":
		text = fmt.Sprintf("读取 workflow 结果失败: %v", reason.Err)
	case "NoFailedVariants":
		text = fmt.Sprintf("workflow 没有失败变体: %s", reason.WorkflowID)
	case "ArtifactMissing":
		text = fmt.Sprintf(
			"变体 %s 的 artifact 数量为 %d，要求恰好 1 个",
			reason.Variant, reason.Count,
		)
	default:
		return store.RejectRender{}, fmt.Errorf("unsupported resolver rejection %q", reason.Code)
	}
	return store.RejectRender{Code: reason.Code, RejectionReason: text}, nil
}

func isPermanentStartError(err error) bool {
	var invalid *serviceerror.InvalidArgument
	return errors.As(err, &invalid)
}

func newFencingToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
