package cardaction

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/rs/zerolog"

	"hermes-devops/runtime/internal/feishu"
	"hermes-devops/runtime/internal/store"
	"hermes-devops/runtime/internal/trigger"
	wf "hermes-devops/runtime/internal/workflow"
)

const (
	sweepLeaseTTL     = 120 * time.Second
	sweepInterval     = 30 * time.Second
	cardReconcileWait = 60 * time.Second
)

// SweepStore is the persistence surface needed by recovery sweeps.
type SweepStore interface {
	ClaimStaleInbox(ctx context.Context, token string, lease time.Duration) (*store.InboxRow, error)
	ClaimStaleAction(ctx context.Context, token string, lease time.Duration) (*store.CardAction, error)
	ClaimMessage(ctx context.Context, token string, lease time.Duration) (*store.MessageClaim, error)
	GetCardSnapshot(ctx context.Context, workflowID string) ([]byte, error)
	CompleteMessageRender(ctx context.Context, claim store.MessageClaim, token string) (bool, error)
	DeferMessageRender(
		ctx context.Context,
		claim store.MessageClaim,
		token string,
		after time.Duration,
		lastErr string,
	) (bool, error)
	AbandonMessageRender(
		ctx context.Context, claim store.MessageClaim, token, lastErr string,
	) (bool, error)
	FinalizeAction(
		ctx context.Context, workflowID, token, state, lastErr string,
	) (bool, error)
}

// Sweeper recovers durable inbox, action, and card-render claims.
type Sweeper struct {
	Store    SweepStore
	Consumer *Consumer
	Starter  trigger.WorkflowStarter
	Updater  feishu.CardUpdater
	Log      *zerolog.Logger
	interval time.Duration
}

// RunOnce performs one inbox, action, and card sweep in deterministic order.
// Failures are joined only after all three independent passes have run.
func (s *Sweeper) RunOnce(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var joined []error
	if err := s.sweepInbox(ctx); err != nil {
		joined = append(joined, fmt.Errorf("inbox sweep: %w", err))
	}
	if err := s.sweepAction(ctx); err != nil {
		joined = append(joined, fmt.Errorf("action sweep: %w", err))
	}
	if err := s.sweepCard(ctx); err != nil {
		joined = append(joined, fmt.Errorf("card sweep: %w", err))
	}
	return errors.Join(joined...)
}

// Run executes an immediate recovery pass, then another every 30 seconds,
// until ctx is cancelled.
func (s *Sweeper) Run(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.runAndLog(ctx)
	ticker := time.NewTicker(s.runInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runAndLog(ctx)
		}
	}
}

func (s *Sweeper) runInterval() time.Duration {
	if s != nil && s.interval > 0 {
		return s.interval
	}
	return sweepInterval
}

func (s *Sweeper) runAndLog(ctx context.Context) {
	if err := s.RunOnce(ctx); err != nil && s != nil && s.Log != nil {
		s.Log.Error().Err(err).Msg("card action recovery sweep failed")
	}
}

func (s *Sweeper) sweepInbox(ctx context.Context) error {
	if s == nil || s.Store == nil {
		return errors.New("store is nil")
	}
	token, err := newFencingToken()
	if err != nil {
		return fmt.Errorf("create fencing token: %w", err)
	}
	row, err := s.Store.ClaimStaleInbox(ctx, token, sweepLeaseTTL)
	if err != nil {
		return fmt.Errorf("claim stale inbox: %w", err)
	}
	if row == nil {
		return nil
	}
	if s.Consumer == nil {
		return errors.New("consume claimed inbox: consumer is nil")
	}
	if err := s.Consumer.consumeClaimed(ctx, row, token); err != nil {
		return fmt.Errorf("consume claimed inbox %s: %w", row.EventID, err)
	}
	return nil
}

func (s *Sweeper) sweepAction(ctx context.Context) error {
	if s == nil || s.Store == nil {
		return errors.New("store is nil")
	}
	token, err := newFencingToken()
	if err != nil {
		return fmt.Errorf("create fencing token: %w", err)
	}
	action, err := s.Store.ClaimStaleAction(ctx, token, sweepLeaseTTL)
	if err != nil {
		return fmt.Errorf("claim stale action: %w", err)
	}
	if action == nil {
		return nil
	}

	input, err := decodePersistedActionInput(action)
	if err != nil {
		lastErr := "invalid persisted target input: " + err.Error()
		return s.finalizeRecoveredAction(ctx, action.WorkflowID, token, "failed", lastErr)
	}
	if s.Starter == nil {
		return fmt.Errorf("start recovered action %s: starter is nil", action.WorkflowID)
	}
	_, _, startErr := s.Starter.StartDeviceTest(ctx, input)
	if startErr != nil {
		if !isPermanentStartError(startErr) {
			return fmt.Errorf("start recovered action %s: %w", action.WorkflowID, startErr)
		}
		return s.finalizeRecoveredAction(
			ctx, action.WorkflowID, token, "failed", startErr.Error(),
		)
	}
	return s.finalizeRecoveredAction(ctx, action.WorkflowID, token, "succeeded", "")
}

func (s *Sweeper) finalizeRecoveredAction(
	ctx context.Context, workflowID, token, state, lastErr string,
) error {
	ok, err := s.Store.FinalizeAction(ctx, workflowID, token, state, lastErr)
	if err != nil {
		return fmt.Errorf("finalize action %s as %s: %w", workflowID, state, err)
	}
	if !ok {
		s.warnLostFence("action", workflowID, "")
	}
	return nil
}

func decodePersistedActionInput(action *store.CardAction) (wf.DeviceTestInput, error) {
	if action == nil {
		return wf.DeviceTestInput{}, errors.New("action is nil")
	}
	if action.WorkflowID == "" || action.Action != "retry" ||
		action.State != "pending" || action.Attempt <= 0 ||
		action.TargetWorkflowID == "" || len(action.TargetInput) == 0 {
		return wf.DeviceTestInput{}, errors.New("action pins are incomplete")
	}
	var input wf.DeviceTestInput
	decoder := json.NewDecoder(bytes.NewReader(action.TargetInput))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return wf.DeviceTestInput{}, fmt.Errorf("decode: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return wf.DeviceTestInput{}, err
	}
	if input.Attempt != action.Attempt {
		return wf.DeviceTestInput{}, fmt.Errorf(
			"input attempt %d does not match action attempt %d",
			input.Attempt, action.Attempt,
		)
	}
	if got := input.WorkflowID(); got != action.TargetWorkflowID {
		return wf.DeviceTestInput{}, fmt.Errorf(
			"input workflow id %q does not match target %q",
			got, action.TargetWorkflowID,
		)
	}
	if input.SourceWorkflowID != action.WorkflowID {
		return wf.DeviceTestInput{}, fmt.Errorf(
			"input source workflow %q does not match action %q",
			input.SourceWorkflowID, action.WorkflowID,
		)
	}
	return input, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode trailing input: %w", err)
	}
	return errors.New("target input contains multiple JSON values")
}

func (s *Sweeper) sweepCard(ctx context.Context) error {
	if s == nil || s.Store == nil {
		return errors.New("store is nil")
	}
	token, err := newFencingToken()
	if err != nil {
		return fmt.Errorf("create fencing token: %w", err)
	}
	claim, err := s.Store.ClaimMessage(ctx, token, sweepLeaseTTL)
	if err != nil {
		return fmt.Errorf("claim message: %w", err)
	}
	if claim == nil {
		return nil
	}
	snapshot, err := s.Store.GetCardSnapshot(ctx, claim.WorkflowID)
	if err != nil {
		return fmt.Errorf("get card snapshot %s: %w", claim.WorkflowID, err)
	}
	rendered, err := RenderCard(snapshot, *claim)
	if err != nil {
		return fmt.Errorf("render message %s: %w", claim.OpenMessageID, err)
	}
	if s.Updater == nil {
		return fmt.Errorf("patch message %s: updater is nil", claim.OpenMessageID)
	}
	patchErr := s.Updater.PatchCard(
		ctx, claim.OpenMessageID, json.RawMessage(rendered),
	)
	switch classifyPatchError(patchErr) {
	case patchOK:
		ok, err := s.Store.CompleteMessageRender(ctx, *claim, token)
		if err != nil {
			return fmt.Errorf("complete message render %s: %w", claim.OpenMessageID, err)
		}
		if !ok {
			s.warnLostFence("message", claim.WorkflowID, claim.OpenMessageID)
		}
		return nil
	case patchAmbiguous:
		ok, err := s.Store.DeferMessageRender(
			ctx, *claim, token, cardReconcileWait, patchErr.Error(),
		)
		if err != nil {
			return errors.Join(
				fmt.Errorf("patch message %s was ambiguous: %w", claim.OpenMessageID, patchErr),
				fmt.Errorf("defer message render %s: %w", claim.OpenMessageID, err),
			)
		}
		if !ok {
			s.warnLostFence("message", claim.WorkflowID, claim.OpenMessageID)
		}
		return fmt.Errorf("patch message %s was ambiguous: %w", claim.OpenMessageID, patchErr)
	case patchPermanent:
		ok, err := s.Store.AbandonMessageRender(
			ctx, *claim, token, patchErr.Error(),
		)
		if err != nil {
			return fmt.Errorf("abandon message render %s: %w", claim.OpenMessageID, err)
		}
		if !ok {
			s.warnLostFence("message", claim.WorkflowID, claim.OpenMessageID)
		}
		return nil
	case patchTransient:
		return fmt.Errorf("patch message %s transient failure: %w", claim.OpenMessageID, patchErr)
	default:
		return fmt.Errorf("patch message %s: unknown classification", claim.OpenMessageID)
	}
}

func (s *Sweeper) warnLostFence(kind, workflowID, messageID string) {
	if s == nil || s.Log == nil {
		return
	}
	event := s.Log.Warn().Str("kind", kind).Str("workflow_id", workflowID)
	if messageID != "" {
		event = event.Str("open_message_id", messageID)
	}
	event.Msg("card action sweep completion lost fencing; no compensating write")
}

type patchResult int

const (
	patchOK patchResult = iota
	patchAmbiguous
	patchPermanent
	patchTransient
)

func classifyPatchError(err error) patchResult {
	if err == nil {
		return patchOK
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return patchAmbiguous
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return patchAmbiguous
	}
	if code, ok := feishu.BusinessErrorCode(err); ok {
		switch code {
		case 230001, // invalid parameter / message unavailable
			230006, // bot ability disabled
			230011, // message recalled
			230013, // bot unavailable to user
			230025, // content too large
			230027, // permission denied
			230028, // DLP audit rejected
			230031, // older than 14-day update window
			230099, // invalid card content
			230110, // message deleted
			232009: // chat dissolved
			return patchPermanent
		case 230020:
			return patchTransient
		default:
			return patchTransient
		}
	}
	if status, ok := feishu.HTTPErrorStatus(err); ok {
		switch status {
		case http.StatusForbidden, http.StatusNotFound, http.StatusGone:
			return patchPermanent
		default:
			return patchTransient
		}
	}
	return patchTransient
}
