package cardaction

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"go.temporal.io/api/serviceerror"

	"hermes-devops/runtime/internal/rerun"
	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

type consumerTestStore struct {
	inbox               map[string]store.InboxRow
	actions             map[string]store.CardAction
	messages            map[string]store.MessageClaim
	audits              []store.AuditRow
	claimTokens         []string
	claimInboxCalls     int
	staleClaimCalls     int
	getActionCalls      int
	getActionErr        error
	getActionNil        bool
	acceptCalls         int
	rejectCalls         int
	finalizeCalls       int
	businessWrites      int
	attemptCalls        int
	acceptKind          string
	finalizeOK          bool
	finalizeErr         error
	acceptedPinsMutator func(*wf.DeviceTestInput)
	outcomeMutator      func(*store.AcceptOutcome)
}

type blockingClaimConsumerStore struct {
	*consumerTestStore

	deadline     time.Time
	deadlineSeen bool
}

func (s *blockingClaimConsumerStore) ClaimInbox(
	ctx context.Context, _ string, _ string, _ time.Duration,
) (*store.InboxRow, error) {
	s.deadline, s.deadlineSeen = ctx.Deadline()
	<-ctx.Done()
	return nil, ctx.Err()
}

func newConsumerTestStore(action string) *consumerTestStore {
	return &consumerTestStore{
		inbox: map[string]store.InboxRow{
			"e1": {
				EventID: "e1", Disposition: "accepted", AckToast: "已收到，正在处理",
				Action: action, WorkflowID: "wf1", ActorOpenID: "ou_1",
				OpenMessageID: "om_1", PayloadDigest: "digest", State: "received",
			},
		},
		actions:    make(map[string]store.CardAction),
		messages:   make(map[string]store.MessageClaim),
		acceptKind: "accepted",
		finalizeOK: true,
	}
}

func (s *consumerTestStore) ClaimInbox(
	_ context.Context, eventID, token string, lease time.Duration,
) (*store.InboxRow, error) {
	s.claimInboxCalls++
	s.claimTokens = append(s.claimTokens, token)
	row, ok := s.inbox[eventID]
	if !ok || row.State != "received" || row.Owner != "" || lease <= 0 {
		return nil, fmt.Errorf("%w: %s", store.ErrInboxNotClaimable, eventID)
	}
	now := time.Now().UTC()
	expires := now.Add(lease)
	row.Owner = token
	row.LeaseExpiresAt = &expires
	row.Attempts++
	s.inbox[eventID] = row
	copy := row
	return &copy, nil
}

func (s *consumerTestStore) ClaimStaleInbox(
	_ context.Context, token string, lease time.Duration,
) (*store.InboxRow, error) {
	s.staleClaimCalls++
	if lease <= 0 {
		return nil, nil
	}
	now := time.Now().UTC()
	for eventID, row := range s.inbox {
		if row.State != "received" ||
			(row.Owner != "" && row.LeaseExpiresAt != nil && row.LeaseExpiresAt.After(now)) {
			continue
		}
		expires := now.Add(lease)
		row.Owner = token
		row.LeaseExpiresAt = &expires
		row.Attempts++
		s.inbox[eventID] = row
		copy := row
		return &copy, nil
	}
	return nil, nil
}

func (s *consumerTestStore) GetCardAction(
	_ context.Context, workflowID string,
) (*store.CardAction, error) {
	s.getActionCalls++
	if s.getActionErr != nil {
		return nil, s.getActionErr
	}
	if s.getActionNil {
		return nil, nil
	}
	action, ok := s.actions[workflowID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", store.ErrCardActionNotFound, workflowID)
	}
	action.TargetInput = append([]byte(nil), action.TargetInput...)
	if action.LeaseExpiresAt != nil {
		lease := *action.LeaseExpiresAt
		action.LeaseExpiresAt = &lease
	}
	return &action, nil
}

func (s *consumerTestStore) CompleteAccept(
	_ context.Context, req store.AcceptRequest,
) (*store.AcceptOutcome, error) {
	s.acceptCalls++
	row, ok := s.inbox[req.EventID]
	if !ok || row.Owner != req.Token {
		return &store.AcceptOutcome{Kind: "lost"}, nil
	}
	if s.acceptKind != "accepted" {
		row.State = "processed"
		s.inbox[req.EventID] = row
		action := s.actions[req.WorkflowID]
		out := &store.AcceptOutcome{
			Kind: s.acceptKind, ActionToken: req.ActionToken, Attempt: action.Attempt,
		}
		if s.acceptKind == "resumed" {
			action.State = "pending"
			action.Owner = req.ActionToken
			action.LastError = ""
			action.Revision++
			s.actions[req.WorkflowID] = action
			out.TargetWorkflowID = action.TargetWorkflowID
			out.TargetInput = append([]byte(nil), action.TargetInput...)
		}
		if s.outcomeMutator != nil {
			s.outcomeMutator(out)
		}
		return out, nil
	}

	action := store.CardAction{
		WorkflowID: req.WorkflowID, Action: req.Action, ActorOpenID: req.ActorOpenID,
		State: "succeeded", Revision: 1,
	}
	out := &store.AcceptOutcome{Kind: "accepted"}
	if req.Action == "retry" {
		s.attemptCalls++
		raw, targetID, err := req.BuildTarget(1)
		if err != nil {
			return nil, err
		}
		if s.acceptedPinsMutator != nil {
			var authoritative wf.DeviceTestInput
			if err := json.Unmarshal(raw, &authoritative); err != nil {
				return nil, err
			}
			s.acceptedPinsMutator(&authoritative)
			raw, err = json.Marshal(authoritative)
			if err != nil {
				return nil, err
			}
			targetID = authoritative.WorkflowID()
		}
		action.State = "pending"
		action.Owner = req.ActionToken
		action.TargetInput = append([]byte(nil), raw...)
		action.TargetWorkflowID = targetID
		action.Attempt = 1
		out.ActionToken = req.ActionToken
		out.Attempt = 1
		out.TargetWorkflowID = targetID
		out.TargetInput = append([]byte(nil), raw...)
	}
	s.actions[req.WorkflowID] = action
	s.messages[req.WorkflowID+"\x00"+req.OpenMessageID] = store.MessageClaim{
		WorkflowID: req.WorkflowID, OpenMessageID: req.OpenMessageID,
		RenderKind: "action", ButtonsMode: "none", DesiredRevision: 1,
	}
	row.State = "processed"
	s.inbox[req.EventID] = row
	s.businessWrites++
	if s.outcomeMutator != nil {
		s.outcomeMutator(out)
	}
	return out, nil
}

func (s *consumerTestStore) CompleteReject(
	_ context.Context, eventID, token string, render store.RejectRender,
) error {
	s.rejectCalls++
	row, ok := s.inbox[eventID]
	if !ok || row.Owner != token {
		return nil
	}
	policy, ok := map[string]struct {
		buttons string
		suffix  string
	}{
		"StillRunning":     {buttons: "both", suffix: "still_running"},
		"ResultUnreadable": {buttons: "both", suffix: "result_unreadable"},
		"ArtifactMissing":  {buttons: "both", suffix: "artifact_missing"},
		"VariantNotMember": {buttons: "both", suffix: "variant_not_member"},
		"NotAuthoritative": {buttons: "none", suffix: "not_authoritative"},
		"NoFailedVariants": {buttons: "none", suffix: "no_failed_variants"},
	}[render.Code]
	if !ok {
		return fmt.Errorf("unsupported reject code %q", render.Code)
	}
	s.messages[row.WorkflowID+"\x00"+row.OpenMessageID] = store.MessageClaim{
		WorkflowID: row.WorkflowID, OpenMessageID: row.OpenMessageID,
		RenderKind: "rejection", RejectionReason: render.RejectionReason,
		ButtonsMode: policy.buttons, DesiredRevision: 1,
	}
	s.audits = append(s.audits, store.AuditRow{
		Action:       "card." + row.Action + ".rejected." + policy.suffix,
		Target:       row.WorkflowID,
		InboxEventID: row.EventID,
	})
	row.State = "processed"
	s.inbox[eventID] = row
	s.businessWrites++
	return nil
}

func (s *consumerTestStore) FinalizeAction(
	_ context.Context, workflowID, token, state, lastErr string,
) (bool, error) {
	s.finalizeCalls++
	if s.finalizeErr != nil {
		return false, s.finalizeErr
	}
	if !s.finalizeOK {
		return false, nil
	}
	action := s.actions[workflowID]
	if action.Owner != token || action.State != "pending" {
		return false, nil
	}
	action.State = state
	action.LastError = lastErr
	action.Owner = ""
	s.actions[workflowID] = action
	return true, nil
}

type fakeConsumerResolver struct {
	resolution        *rerun.Resolution
	failureRun        *rerun.FailureRun
	err               error
	failureRunCalls   int
	resolveRetryCalls int
}

func (r *fakeConsumerResolver) ResolveFailureRun(
	_ context.Context, _ string,
) (*rerun.FailureRun, error) {
	r.failureRunCalls++
	if r.err != nil {
		return nil, r.err
	}
	return r.failureRun, nil
}

func (r *fakeConsumerResolver) ResolveRetry(
	_ context.Context, _ string, variant string,
) (*rerun.Resolution, error) {
	r.resolveRetryCalls++
	if variant != "" {
		return nil, fmt.Errorf("variant = %q, want empty", variant)
	}
	if r.err != nil {
		return nil, r.err
	}
	return r.resolution, nil
}

type fakeConsumerStarter struct {
	started    bool
	err        error
	startCalls int
	inputs     []wf.DeviceTestInput
}

func (s *fakeConsumerStarter) StartDeviceTest(
	_ context.Context, in wf.DeviceTestInput,
) (string, bool, error) {
	s.startCalls++
	s.inputs = append(s.inputs, in)
	if s.err != nil {
		return "", false, s.err
	}
	return in.WorkflowID(), s.started, nil
}

func fullResolution() *rerun.Resolution {
	return &rerun.Resolution{
		Run: store.WorkflowRun{
			WorkflowID: "wf1", Project: "grp/p", CommitSHA: "abcd1234",
			PipelineID: 42, Version: "1.2.3", RuleVersion: "rules-v1",
			Scope: "failed", Variants: []string{"v1"},
		},
		Targets: []string{"v1"},
		Packages: []wf.PackageRef{{
			Variant: "v1", PackageFile: "v1.tar.gz", URL: "https://registry/v1",
			SHA256: "sha-v1", Size: 7, ManifestDigest: "manifest-v1",
		}},
		Scope: "failed",
	}
}

func newTestConsumer(action string) (*Consumer, *consumerTestStore, *fakeConsumerResolver, *fakeConsumerStarter) {
	st := newConsumerTestStore(action)
	resolution := fullResolution()
	resolver := &fakeConsumerResolver{
		resolution: resolution,
		failureRun: &rerun.FailureRun{Run: resolution.Run, Targets: resolution.Targets},
	}
	starter := &fakeConsumerStarter{started: true}
	return &Consumer{Store: st, Resolver: resolver, Starter: starter}, st, resolver, starter
}

func TestConsumeOneBoundsBlockingClaim(t *testing.T) {
	const (
		operationTimeout = 25 * time.Millisecond
		safetyTimeout    = 500 * time.Millisecond
	)
	st := &blockingClaimConsumerStore{consumerTestStore: newConsumerTestStore("ignore")}
	c := &Consumer{
		Store: st, Resolver: &fakeConsumerResolver{},
		operationTimeout: operationTimeout,
	}
	parent, cancel := context.WithTimeout(context.Background(), safetyTimeout)
	defer cancel()

	started := time.Now()
	err := c.ConsumeOne(parent, "e1")
	elapsed := time.Since(started)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ConsumeOne error = %v, want deadline exceeded", err)
	}
	if !st.deadlineSeen {
		t.Fatal("ClaimInbox context had no deadline")
	}
	if got := st.deadline.Sub(started); got <= 0 || got > 4*operationTimeout {
		t.Fatalf("ClaimInbox deadline after start = %s, want within %s", got, 4*operationTimeout)
	}
	if elapsed >= safetyTimeout/2 {
		t.Fatalf("ConsumeOne elapsed = %s, want internal timeout before safety deadline", elapsed)
	}
}

func TestConsumerBackgroundTimeoutDefaultsWithinLease(t *testing.T) {
	if defaultBackgroundOperationTimeout <= 0 ||
		defaultBackgroundOperationTimeout >= consumerLeaseTTL {
		t.Fatalf(
			"default background operation timeout = %s, want > 0 and < lease %s",
			defaultBackgroundOperationTimeout, consumerLeaseTTL,
		)
	}
	if got := (&Consumer{}).backgroundTimeout(); got != defaultBackgroundOperationTimeout {
		t.Fatalf("zero consumer timeout = %s, want %s", got, defaultBackgroundOperationTimeout)
	}
	if got := (&Consumer{operationTimeout: -time.Second}).backgroundTimeout(); got !=
		defaultBackgroundOperationTimeout {
		t.Fatalf("negative consumer timeout = %s, want %s", got, defaultBackgroundOperationTimeout)
	}
	const configured = 7 * time.Millisecond
	if got := (&Consumer{operationTimeout: configured}).backgroundTimeout(); got != configured {
		t.Fatalf("configured consumer timeout = %s, want %s", got, configured)
	}
}

func TestTargetInputAssertedFieldByField(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*wf.DeviceTestInput)
	}{
		{"Version", func(in *wf.DeviceTestInput) { in.Version = "" }},
		{"RuleVersion", func(in *wf.DeviceTestInput) { in.RuleVersion = "" }},
		{"Packages", func(in *wf.DeviceTestInput) { in.Packages = nil }},
		{"SourceWorkflowID", func(in *wf.DeviceTestInput) { in.SourceWorkflowID = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, _, starter := newTestConsumer("retry")
			c.mutateInput = tt.mutate

			err := c.ConsumeOne(context.Background(), "e1")
			if err == nil {
				t.Fatalf("缺 %s 的 target_input 必须被拒", tt.name)
			}
			if len(st.actions) != 0 || st.businessWrites != 0 {
				t.Fatalf("断言失败后业务写入: actions=%d writes=%d", len(st.actions), st.businessWrites)
			}
			if starter.startCalls != 0 {
				t.Fatalf("StartDeviceTest calls = %d, want 0", starter.startCalls)
			}
		})
	}
}

func TestResolutionSourceMustMatchClaimedInbox(t *testing.T) {
	c, st, resolver, starter := newTestConsumer("retry")
	resolver.resolution.Run.WorkflowID = "wf_other"
	err := c.ConsumeOne(context.Background(), "e1")
	if err == nil {
		t.Fatal("mismatched resolver source must be rejected")
	}
	if len(st.actions) != 0 || st.businessWrites != 0 || starter.startCalls != 0 {
		t.Fatalf(
			"source mismatch wrote actions=%d writes=%d starts=%d",
			len(st.actions), st.businessWrites, starter.startCalls,
		)
	}
}

func TestStartedFalseIsIdempotentSuccess(t *testing.T) {
	c, st, _, starter := newTestConsumer("retry")
	starter.started = false

	if err := c.ConsumeOne(context.Background(), "e1"); err != nil {
		t.Fatalf("ConsumeOne: %v", err)
	}
	if got := st.actions["wf1"].State; got != "succeeded" {
		t.Fatalf("state = %q, want succeeded", got)
	}
	if st.finalizeCalls != 1 {
		t.Fatalf("FinalizeAction calls = %d, want 1", st.finalizeCalls)
	}
}

func TestCrashBetweenAckAndClaimIsRecovered(t *testing.T) {
	c, st, _, _ := newTestConsumer("retry")
	if row := st.inbox["e1"]; row.State != "received" || row.Owner != "" {
		t.Fatalf("precondition inbox = %#v", row)
	}

	if err := c.ConsumeOne(context.Background(), "e1"); err != nil {
		t.Fatalf("sweep 接管后必须能完成: %v", err)
	}
	if st.actions["wf1"].State != "succeeded" {
		t.Fatalf("动作丢失: %#v", st.actions["wf1"])
	}
}

func TestClaimedStaleInboxIsConsumedWithoutReclaim(t *testing.T) {
	c, st, resolver, starter := newTestConsumer("ignore")
	const token = "stale-inbox-owner"
	row, err := st.ClaimStaleInbox(context.Background(), token, consumerLeaseTTL)
	if err != nil || row == nil {
		t.Fatalf("ClaimStaleInbox = (%#v, %v)", row, err)
	}

	if err := c.consumeClaimed(context.Background(), row, token); err != nil {
		t.Fatalf("consumeClaimed: %v", err)
	}
	if st.staleClaimCalls != 1 || st.claimInboxCalls != 0 {
		t.Fatalf(
			"claims stale=%d immediate=%d, want 1/0",
			st.staleClaimCalls, st.claimInboxCalls,
		)
	}
	if action := st.actions["wf1"]; action.State != "succeeded" {
		t.Fatalf("action = %#v, want succeeded", action)
	}
	if resolver.failureRunCalls != 1 || starter.startCalls != 0 {
		t.Fatalf(
			"calls ResolveFailureRun=%d StartDeviceTest=%d, want 1/0",
			resolver.failureRunCalls, starter.startCalls,
		)
	}
}

func TestConsumeClaimedRejectsInvalidFence(t *testing.T) {
	tests := []struct {
		name  string
		row   *store.InboxRow
		token string
	}{
		{name: "nil row", token: "owner"},
		{name: "empty token", row: &store.InboxRow{Owner: "owner"}},
		{name: "owner mismatch", row: &store.InboxRow{Owner: "owner"}, token: "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, resolver, starter := newTestConsumer("ignore")
			if err := c.consumeClaimed(context.Background(), tt.row, tt.token); err == nil {
				t.Fatal("invalid claimed row fence must fail")
			}
			if st.claimInboxCalls != 0 || st.acceptCalls != 0 || st.rejectCalls != 0 ||
				resolver.failureRunCalls != 0 || starter.startCalls != 0 {
				t.Fatalf(
					"invalid fence side effects claim=%d accept=%d reject=%d resolve=%d start=%d",
					st.claimInboxCalls, st.acceptCalls, st.rejectCalls,
					resolver.failureRunCalls, starter.startCalls,
				)
			}
		})
	}
}

func TestRejectionRendersOnCard(t *testing.T) {
	for _, code := range []string{"NotAuthoritative", "StillRunning", "NoFailedVariants"} {
		t.Run(code, func(t *testing.T) {
			c, st, resolver, starter := newTestConsumer("retry")
			resolver.err = &rerun.RejectReason{Code: code, WorkflowID: "wf1"}

			if err := c.ConsumeOne(context.Background(), "e1"); err != nil {
				t.Fatalf("ConsumeOne: %v", err)
			}
			if st.attemptCalls != 0 || starter.startCalls != 0 {
				t.Fatalf("拒绝路径调用 attempt=%d start=%d", st.attemptCalls, starter.startCalls)
			}
			message := st.messages["wf1\x00om_1"]
			if message.RenderKind != "rejection" || message.RejectionReason == "" {
				t.Fatalf("message = %#v", message)
			}
			if st.rejectCalls != 1 || st.acceptCalls != 0 {
				t.Fatalf("complete calls: reject=%d accept=%d", st.rejectCalls, st.acceptCalls)
			}
		})
	}
}

func TestVariantNotMemberRejectionCompletesTerminally(t *testing.T) {
	c, st, resolver, starter := newTestConsumer("retry")
	resolver.err = &rerun.RejectReason{
		Code: "VariantNotMember", WorkflowID: "wf1", Variant: "v2",
	}

	if err := c.ConsumeOne(context.Background(), "e1"); err != nil {
		t.Fatalf("ConsumeOne: %v", err)
	}
	if err := c.ConsumeOne(context.Background(), "e1"); err != nil {
		t.Fatalf("duplicate ConsumeOne: %v", err)
	}

	inbox := st.inbox["e1"]
	if inbox.State != "processed" || inbox.Attempts != 1 {
		t.Fatalf("inbox = %#v, want processed exactly once", inbox)
	}
	message := st.messages["wf1\x00om_1"]
	if message.RenderKind != "rejection" ||
		message.RejectionReason != "运行结果中的变体 v2 不属于源 workflow wf1" ||
		message.ButtonsMode != "both" {
		t.Fatalf("message = %#v", message)
	}
	if len(st.audits) != 1 ||
		st.audits[0].Action != "card.retry.rejected.variant_not_member" ||
		st.audits[0].InboxEventID != "e1" {
		t.Fatalf("audits = %#v", st.audits)
	}
	if len(st.actions) != 0 || st.acceptCalls != 0 || st.attemptCalls != 0 ||
		starter.startCalls != 0 {
		t.Fatalf(
			"rejection created actions=%d accepts=%d attempts=%d starts=%d",
			len(st.actions), st.acceptCalls, st.attemptCalls, starter.startCalls,
		)
	}
}

func TestButtonsModeByReason(t *testing.T) {
	tests := []struct {
		code string
		want string
	}{
		{"StillRunning", "both"},
		{"ResultUnreadable", "both"},
		{"ArtifactMissing", "both"},
		{"NotAuthoritative", "none"},
		{"NoFailedVariants", "none"},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			c, st, resolver, _ := newTestConsumer("retry")
			resolver.err = &rerun.RejectReason{
				Code: tt.code, WorkflowID: "wf1", Variant: "v1", Count: 0,
				Err: errors.New("unreadable"),
			}
			if err := c.ConsumeOne(context.Background(), "e1"); err != nil {
				t.Fatalf("ConsumeOne: %v", err)
			}
			if got := st.messages["wf1\x00om_1"].ButtonsMode; got != tt.want {
				t.Fatalf("buttons_mode = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIgnoreUsesFailureRunOnlyAndDoesNotStart(t *testing.T) {
	c, st, resolver, starter := newTestConsumer("ignore")
	if err := c.ConsumeOne(context.Background(), "e1"); err != nil {
		t.Fatalf("ConsumeOne: %v", err)
	}
	if st.getActionCalls != 1 {
		t.Fatalf("GetCardAction calls = %d, want 1", st.getActionCalls)
	}
	if resolver.failureRunCalls != 1 || resolver.resolveRetryCalls != 0 {
		t.Fatalf("resolver calls failure=%d retry=%d", resolver.failureRunCalls, resolver.resolveRetryCalls)
	}
	if starter.startCalls != 0 || st.attemptCalls != 0 {
		t.Fatalf("ignore calls start=%d attempt=%d", starter.startCalls, st.attemptCalls)
	}
	if action := st.actions["wf1"]; action.State != "succeeded" ||
		action.TargetWorkflowID != "" || len(action.TargetInput) != 0 {
		t.Fatalf("ignore action = %#v", action)
	}
}

func TestExistingRetryActionMakesIgnoreUseAuthoritativeConflict(t *testing.T) {
	for _, state := range []string{"pending", "succeeded"} {
		t.Run(state, func(t *testing.T) {
			c, st, resolver, starter := newTestConsumer("ignore")
			st.actions["wf1"] = store.CardAction{
				WorkflowID: "wf1", Action: "retry", ActorOpenID: "ou_previous",
				State: state, Owner: "existing-owner", Attempt: 3, Revision: 1,
			}
			st.acceptKind = "conflict"
			resolver.err = &rerun.RejectReason{
				Code: "ResultUnreadable", WorkflowID: "wf1",
				Err: errors.New("temporary result read failure"),
			}

			if err := c.ConsumeOne(context.Background(), "e1"); err != nil {
				t.Fatalf("ConsumeOne: %v", err)
			}
			if st.getActionCalls != 1 || resolver.failureRunCalls != 0 {
				t.Fatalf(
					"preflight calls GetCardAction=%d ResolveFailureRun=%d, want 1/0",
					st.getActionCalls, resolver.failureRunCalls,
				)
			}
			if st.acceptCalls != 1 || st.acceptKind != "conflict" || st.rejectCalls != 0 {
				t.Fatalf(
					"completion calls accept=%d kind=%q reject=%d, want 1/conflict/0",
					st.acceptCalls, st.acceptKind, st.rejectCalls,
				)
			}
			if starter.startCalls != 0 {
				t.Fatalf("StartDeviceTest calls = %d, want 0", starter.startCalls)
			}
		})
	}
}

func TestIgnoreActionLookupErrorPropagates(t *testing.T) {
	c, st, resolver, starter := newTestConsumer("ignore")
	lookupErr := errors.New("database unavailable")
	st.getActionErr = lookupErr

	err := c.ConsumeOne(context.Background(), "e1")
	if !errors.Is(err, lookupErr) {
		t.Fatalf("ConsumeOne error = %v, want wrapped lookup error", err)
	}
	if resolver.failureRunCalls != 0 || st.acceptCalls != 0 || st.rejectCalls != 0 ||
		starter.startCalls != 0 {
		t.Fatalf(
			"lookup error calls resolve=%d accept=%d reject=%d start=%d",
			resolver.failureRunCalls, st.acceptCalls, st.rejectCalls, starter.startCalls,
		)
	}
}

func TestIgnoreActionLookupNilIsImplementationError(t *testing.T) {
	c, st, resolver, starter := newTestConsumer("ignore")
	st.getActionNil = true

	if err := c.ConsumeOne(context.Background(), "e1"); err == nil {
		t.Fatal("nil action with nil error must fail")
	}
	if resolver.failureRunCalls != 0 || st.acceptCalls != 0 || st.rejectCalls != 0 ||
		starter.startCalls != 0 {
		t.Fatalf(
			"nil lookup calls resolve=%d accept=%d reject=%d start=%d",
			resolver.failureRunCalls, st.acceptCalls, st.rejectCalls, starter.startCalls,
		)
	}
}

func TestRetryStartsExactPinnedInput(t *testing.T) {
	c, st, resolver, starter := newTestConsumer("retry")
	st.acceptedPinsMutator = func(in *wf.DeviceTestInput) {
		in.Packages[0].URL = "https://registry/authoritative-persisted-v1"
	}
	if err := c.ConsumeOne(context.Background(), "e1"); err != nil {
		t.Fatalf("ConsumeOne: %v", err)
	}
	if len(starter.inputs) != 1 {
		t.Fatalf("start inputs = %d, want 1", len(starter.inputs))
	}
	var pinned wf.DeviceTestInput
	if err := json.Unmarshal(st.actions["wf1"].TargetInput, &pinned); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(starter.inputs[0], pinned) {
		t.Fatalf("started input = %#v, pinned = %#v", starter.inputs[0], pinned)
	}
	if resolver.resolveRetryCalls != 1 {
		t.Fatalf("ResolveRetry calls = %d, want 1", resolver.resolveRetryCalls)
	}
}

func TestFailedRetryResumesFromPersistedPinsWithoutResolving(t *testing.T) {
	c, st, resolver, starter := newTestConsumer("retry")
	persisted := buildTargetInput(fullResolution(), 7)
	persisted.Packages[0].URL = "https://registry/persisted-old-v1"
	raw, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	st.actions["wf1"] = store.CardAction{
		WorkflowID: "wf1", Action: "retry", ActorOpenID: "ou_1",
		State: "failed", Attempt: persisted.Attempt, Revision: 2,
		TargetWorkflowID: persisted.WorkflowID(), TargetInput: raw,
		LastError: "previous temporal failure",
	}
	st.acceptKind = "resumed"
	resolver.err = &rerun.RejectReason{
		Code: "ArtifactMissing", WorkflowID: "wf1", Variant: "v1", Count: 0,
	}

	if err := c.ConsumeOne(context.Background(), "e1"); err != nil {
		t.Fatalf("ConsumeOne: %v", err)
	}
	if resolver.resolveRetryCalls != 0 {
		t.Fatalf("ResolveRetry calls = %d, want 0", resolver.resolveRetryCalls)
	}
	if st.getActionCalls != 1 {
		t.Fatalf("GetCardAction calls = %d, want 1", st.getActionCalls)
	}
	if st.attemptCalls != 0 {
		t.Fatalf("BuildTarget calls = %d, want 0", st.attemptCalls)
	}
	if starter.startCalls != 1 || len(starter.inputs) != 1 {
		t.Fatalf("StartDeviceTest calls = %d inputs=%d, want 1", starter.startCalls, len(starter.inputs))
	}
	if !reflect.DeepEqual(starter.inputs[0], persisted) {
		t.Fatalf("started input = %#v, persisted = %#v", starter.inputs[0], persisted)
	}
	if action := st.actions["wf1"]; action.State != "succeeded" ||
		action.Attempt != persisted.Attempt || action.TargetWorkflowID != persisted.WorkflowID() ||
		!reflect.DeepEqual(action.TargetInput, raw) {
		t.Fatalf("resumed action = %#v", action)
	}
}

func TestRetryDoesNotStartInvalidAuthoritativePins(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *store.AcceptOutcome)
	}{
		{"invalid JSON", func(_ *testing.T, out *store.AcceptOutcome) {
			out.TargetInput = []byte("{")
		}},
		{"attempt mismatch", func(_ *testing.T, out *store.AcceptOutcome) {
			out.Attempt++
		}},
		{"target workflow mismatch", func(_ *testing.T, out *store.AcceptOutcome) {
			out.TargetWorkflowID += "-other"
		}},
		{"source workflow mismatch", func(t *testing.T, out *store.AcceptOutcome) {
			var input wf.DeviceTestInput
			if err := json.Unmarshal(out.TargetInput, &input); err != nil {
				t.Fatal(err)
			}
			input.SourceWorkflowID = "wf-other"
			raw, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			out.TargetInput = raw
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, _, starter := newTestConsumer("retry")
			st.outcomeMutator = func(out *store.AcceptOutcome) {
				tt.mutate(t, out)
			}

			if err := c.ConsumeOne(context.Background(), "e1"); err == nil {
				t.Fatal("invalid authoritative pins must fail")
			}
			if starter.startCalls != 0 || st.finalizeCalls != 0 {
				t.Fatalf(
					"invalid pins called start=%d finalize=%d",
					starter.startCalls, st.finalizeCalls,
				)
			}
		})
	}
}

func TestClaimUsesFresh128BitHexToken(t *testing.T) {
	c1, st1, _, _ := newTestConsumer("ignore")
	c2, st2, _, _ := newTestConsumer("ignore")
	if err := c1.ConsumeOne(context.Background(), "e1"); err != nil {
		t.Fatal(err)
	}
	if err := c2.ConsumeOne(context.Background(), "e1"); err != nil {
		t.Fatal(err)
	}
	tokens := []string{st1.claimTokens[0], st2.claimTokens[0]}
	for _, token := range tokens {
		raw, err := hex.DecodeString(token)
		if err != nil || len(raw) != 16 {
			t.Fatalf("token %q = %x, %v; want 128-bit hex", token, raw, err)
		}
	}
	if tokens[0] == tokens[1] {
		t.Fatalf("fencing token reused: %q", tokens[0])
	}
}

func TestNonClaimableIsBenignNoop(t *testing.T) {
	c, st, resolver, starter := newTestConsumer("retry")
	row := st.inbox["e1"]
	row.State = "processed"
	st.inbox["e1"] = row
	if err := c.ConsumeOne(context.Background(), "e1"); err != nil {
		t.Fatalf("ConsumeOne: %v", err)
	}
	if resolver.resolveRetryCalls != 0 || starter.startCalls != 0 || st.acceptCalls != 0 {
		t.Fatal("non-claimable event must have no downstream side effects")
	}
}

func TestNonExecutingAcceptOutcomesDoNotStart(t *testing.T) {
	for _, kind := range []string{"conflict", "legacy", "lost"} {
		t.Run(kind, func(t *testing.T) {
			c, st, _, starter := newTestConsumer("retry")
			st.acceptKind = kind
			if err := c.ConsumeOne(context.Background(), "e1"); err != nil {
				t.Fatalf("ConsumeOne: %v", err)
			}
			if starter.startCalls != 0 || st.finalizeCalls != 0 {
				t.Fatalf("%s calls start=%d finalize=%d", kind, starter.startCalls, st.finalizeCalls)
			}
		})
	}
}

func TestExistingPendingRetryDoesNotResolveOrStart(t *testing.T) {
	c, st, resolver, starter := newTestConsumer("retry")
	persisted := buildTargetInput(fullResolution(), 3)
	raw, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	st.actions["wf1"] = store.CardAction{
		WorkflowID: "wf1", Action: "retry", ActorOpenID: "ou_1",
		State: "pending", Owner: "existing-owner", Attempt: persisted.Attempt, Revision: 1,
		TargetWorkflowID: persisted.WorkflowID(), TargetInput: raw,
	}
	st.acceptKind = "conflict"
	resolver.err = &rerun.RejectReason{
		Code: "ArtifactMissing", WorkflowID: "wf1", Variant: "v1", Count: 0,
	}

	if err := c.ConsumeOne(context.Background(), "e1"); err != nil {
		t.Fatalf("ConsumeOne: %v", err)
	}
	if resolver.resolveRetryCalls != 0 {
		t.Fatalf("ResolveRetry calls = %d, want 0", resolver.resolveRetryCalls)
	}
	if st.acceptCalls != 1 || starter.startCalls != 0 || st.finalizeCalls != 0 {
		t.Fatalf(
			"calls accept=%d start=%d finalize=%d, want 1/0/0",
			st.acceptCalls, starter.startCalls, st.finalizeCalls,
		)
	}
}

func TestRetryActionLookupErrorPropagates(t *testing.T) {
	c, st, resolver, starter := newTestConsumer("retry")
	lookupErr := errors.New("database unavailable")
	st.getActionErr = lookupErr

	err := c.ConsumeOne(context.Background(), "e1")
	if !errors.Is(err, lookupErr) {
		t.Fatalf("ConsumeOne error = %v, want wrapped lookup error", err)
	}
	if resolver.resolveRetryCalls != 0 || st.acceptCalls != 0 ||
		starter.startCalls != 0 || st.finalizeCalls != 0 {
		t.Fatalf(
			"lookup error calls resolve=%d accept=%d start=%d finalize=%d",
			resolver.resolveRetryCalls, st.acceptCalls, starter.startCalls, st.finalizeCalls,
		)
	}
}

func TestTemporaryStarterErrorLeavesActionPending(t *testing.T) {
	c, st, _, starter := newTestConsumer("retry")
	starter.err = serviceerror.NewUnavailable("temporal unavailable")
	err := c.ConsumeOne(context.Background(), "e1")
	if err == nil {
		t.Fatal("temporary starter error must propagate")
	}
	if st.actions["wf1"].State != "pending" || st.finalizeCalls != 0 {
		t.Fatalf("action=%#v finalizeCalls=%d", st.actions["wf1"], st.finalizeCalls)
	}
}

func TestPermanentStarterErrorFinalizesFailed(t *testing.T) {
	c, st, _, starter := newTestConsumer("retry")
	starter.err = serviceerror.NewInvalidArgument("invalid input")
	if err := c.ConsumeOne(context.Background(), "e1"); err != nil {
		t.Fatalf("ConsumeOne: %v", err)
	}
	action := st.actions["wf1"]
	if action.State != "failed" || action.LastError == "" || st.finalizeCalls != 1 {
		t.Fatalf("action=%#v finalizeCalls=%d", action, st.finalizeCalls)
	}
}

func TestFinalizeErrorPropagates(t *testing.T) {
	c, st, _, _ := newTestConsumer("retry")
	st.finalizeErr = errors.New("database unavailable")
	if err := c.ConsumeOne(context.Background(), "e1"); err == nil {
		t.Fatal("FinalizeAction error must propagate")
	}
}
