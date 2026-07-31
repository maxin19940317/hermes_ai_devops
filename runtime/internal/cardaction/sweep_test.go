package cardaction

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/api/serviceerror"

	"hermes-devops/runtime/internal/feishu"
	"hermes-devops/runtime/internal/rerun"
	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

type sweepTestStore struct {
	*consumerTestStore

	inboxSweepCalls  int
	actionSweepCalls int
	cardSweepCalls   int
	inboxClaimErr    error
	actionClaimErr   error
	cardClaimErr     error
	claimTokens      []string
	claimLeases      []time.Duration

	messageClaim *store.MessageClaim
	snapshot     []byte
	snapshotErr  error
	snapshotGets int

	completeCalls int
	completeClaim store.MessageClaim
	completeToken string
	completeOK    bool
	completeErr   error

	deferCalls int
	deferClaim store.MessageClaim
	deferToken string
	deferAfter time.Duration
	deferError string
	deferOK    bool
	deferErr   error

	abandonCalls int
	abandonClaim store.MessageClaim
	abandonToken string
	abandonError string
	abandonOK    bool
	abandonErr   error

	passDone chan struct{}
	passOnce sync.Once
}

func newSweepTestStore() *sweepTestStore {
	base := newConsumerTestStore("ignore")
	delete(base.inbox, "e1")
	return &sweepTestStore{
		consumerTestStore: base,
		snapshot:          minimalSnapshot(),
		completeOK:        true,
		deferOK:           true,
		abandonOK:         true,
	}
}

func (s *sweepTestStore) ClaimStaleInbox(
	ctx context.Context, token string, lease time.Duration,
) (*store.InboxRow, error) {
	s.inboxSweepCalls++
	s.recordClaim(token, lease)
	if s.inboxClaimErr != nil {
		return nil, s.inboxClaimErr
	}
	return s.consumerTestStore.ClaimStaleInbox(ctx, token, lease)
}

func (s *sweepTestStore) ClaimStaleAction(
	_ context.Context, token string, lease time.Duration,
) (*store.CardAction, error) {
	s.actionSweepCalls++
	s.recordClaim(token, lease)
	if s.actionClaimErr != nil {
		return nil, s.actionClaimErr
	}
	now := time.Now().UTC()
	for workflowID, action := range s.actions {
		if action.State != "pending" ||
			(action.LeaseExpiresAt != nil && !action.LeaseExpiresAt.Before(now)) {
			continue
		}
		expires := now.Add(lease)
		action.Owner = token
		action.LeaseExpiresAt = &expires
		action.TargetInput = append([]byte(nil), action.TargetInput...)
		s.actions[workflowID] = action
		copy := action
		return &copy, nil
	}
	return nil, nil
}

func (s *sweepTestStore) ClaimMessage(
	_ context.Context, token string, lease time.Duration,
) (*store.MessageClaim, error) {
	s.cardSweepCalls++
	s.recordClaim(token, lease)
	if s.passDone != nil {
		s.passOnce.Do(func() { close(s.passDone) })
	}
	if s.cardClaimErr != nil {
		return nil, s.cardClaimErr
	}
	return cloneMessageClaim(s.messageClaim), nil
}

func (s *sweepTestStore) GetCardSnapshot(
	_ context.Context, _ string,
) ([]byte, error) {
	s.snapshotGets++
	if s.snapshotErr != nil {
		return nil, s.snapshotErr
	}
	return append([]byte(nil), s.snapshot...), nil
}

func (s *sweepTestStore) CompleteMessageRender(
	_ context.Context, claim store.MessageClaim, token string,
) (bool, error) {
	s.completeCalls++
	s.completeClaim = copyMessageClaim(claim)
	s.completeToken = token
	return s.completeOK, s.completeErr
}

func (s *sweepTestStore) DeferMessageRender(
	_ context.Context,
	claim store.MessageClaim,
	token string,
	after time.Duration,
	lastErr string,
) (bool, error) {
	s.deferCalls++
	s.deferClaim = copyMessageClaim(claim)
	s.deferToken = token
	s.deferAfter = after
	s.deferError = lastErr
	return s.deferOK, s.deferErr
}

func (s *sweepTestStore) AbandonMessageRender(
	_ context.Context, claim store.MessageClaim, token, lastErr string,
) (bool, error) {
	s.abandonCalls++
	s.abandonClaim = copyMessageClaim(claim)
	s.abandonToken = token
	s.abandonError = lastErr
	return s.abandonOK, s.abandonErr
}

func (s *sweepTestStore) recordClaim(token string, lease time.Duration) {
	s.claimTokens = append(s.claimTokens, token)
	s.claimLeases = append(s.claimLeases, lease)
}

type fakeCardUpdater struct {
	calls      int
	messageIDs []string
	cards      []any
	err        error
}

func (u *fakeCardUpdater) PatchCard(_ context.Context, messageID string, card any) error {
	u.calls++
	u.messageIDs = append(u.messageIDs, messageID)
	u.cards = append(u.cards, card)
	return u.err
}

func TestRunOnceUsesFreshTokensAndRunsAllSweepsAfterFailure(t *testing.T) {
	st := newSweepTestStore()
	st.inboxClaimErr = errors.New("inbox database down")
	s := &Sweeper{Store: st}

	err := s.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "inbox sweep") {
		t.Fatalf("RunOnce error = %v, want contextual inbox error", err)
	}
	if st.inboxSweepCalls != 1 || st.actionSweepCalls != 1 || st.cardSweepCalls != 1 {
		t.Fatalf("sweep calls = inbox:%d action:%d card:%d, want 1 each",
			st.inboxSweepCalls, st.actionSweepCalls, st.cardSweepCalls)
	}
	if len(st.claimTokens) != 3 || len(st.claimLeases) != 3 {
		t.Fatalf("claims = tokens:%d leases:%d, want 3 each",
			len(st.claimTokens), len(st.claimLeases))
	}
	seen := make(map[string]bool)
	for i, token := range st.claimTokens {
		raw, decodeErr := hex.DecodeString(token)
		if decodeErr != nil || len(raw) != 16 {
			t.Fatalf("claim %d token %q = %x, %v; want unpredictable 128-bit hex",
				i, token, raw, decodeErr)
		}
		if seen[token] {
			t.Fatalf("fencing token reused: %q", token)
		}
		seen[token] = true
		if st.claimLeases[i] != 120*time.Second {
			t.Fatalf("claim %d lease = %s, want 120s", i, st.claimLeases[i])
		}
	}
}

func TestStaleInboxConsumesAlreadyClaimedRowWithoutDoubleClaim(t *testing.T) {
	st := newSweepTestStore()
	st.inbox["evt-crashed-after-ack"] = store.InboxRow{
		EventID: "evt-crashed-after-ack", Disposition: "accepted",
		Action: "ignore", WorkflowID: "wf1", ActorOpenID: "ou_1",
		OpenMessageID: "om_1", PayloadDigest: "digest", State: "received",
	}
	resolution := fullResolution()
	resolver := &fakeConsumerResolver{
		failureRun: &rerun.FailureRun{Run: resolution.Run, Targets: resolution.Targets},
	}
	consumer := &Consumer{Store: st, Resolver: resolver}
	s := &Sweeper{Store: st, Consumer: consumer}

	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st.inboxSweepCalls != 1 || st.staleClaimCalls != 1 {
		t.Fatalf("stale claims = sweep:%d store:%d, want 1/1",
			st.inboxSweepCalls, st.staleClaimCalls)
	}
	if st.claimInboxCalls != 0 {
		t.Fatalf("ClaimInbox calls = %d, want 0 after ClaimStaleInbox", st.claimInboxCalls)
	}
	if st.acceptCalls != 1 || st.inbox["evt-crashed-after-ack"].State != "processed" {
		t.Fatalf("accept calls=%d row=%#v", st.acceptCalls, st.inbox["evt-crashed-after-ack"])
	}
}

func TestActionRecoveryUsesExactPersistedInputAndAlreadyStartedIsSuccess(t *testing.T) {
	st := newSweepTestStore()
	input := buildTargetInput(fullResolution(), 7)
	input.Packages[0].URL = "https://registry/persisted-exactly"
	raw := mustMarshal(t, input)
	st.actions["wf1"] = store.CardAction{
		WorkflowID: "wf1", Action: "retry", ActorOpenID: "ou_1",
		State: "pending", Attempt: input.Attempt, Revision: 1,
		TargetWorkflowID: input.WorkflowID(), TargetInput: raw,
	}
	starter := &fakeConsumerStarter{started: false}
	s := &Sweeper{Store: st, Starter: starter}

	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if starter.startCalls != 1 || len(starter.inputs) != 1 {
		t.Fatalf("start calls=%d inputs=%d, want 1/1", starter.startCalls, len(starter.inputs))
	}
	if !reflect.DeepEqual(starter.inputs[0], input) {
		t.Fatalf("started input = %#v, persisted = %#v", starter.inputs[0], input)
	}
	action := st.actions["wf1"]
	if action.State != "succeeded" || action.LastError != "" || st.finalizeCalls != 1 {
		t.Fatalf("action=%#v finalizeCalls=%d", action, st.finalizeCalls)
	}
}

func TestActionRecoveryInvalidPersistedInputFinalizesFailed(t *testing.T) {
	valid := buildTargetInput(fullResolution(), 3)
	tests := []struct {
		name   string
		raw    func(*testing.T) []byte
		mutate func(*store.CardAction)
	}{
		{"malformed JSON", func(*testing.T) []byte { return []byte("{") }, nil},
		{"attempt mismatch", func(t *testing.T) []byte {
			input := valid
			input.Attempt++
			return mustMarshal(t, input)
		}, nil},
		{"target mismatch", func(t *testing.T) []byte { return mustMarshal(t, valid) },
			func(action *store.CardAction) { action.TargetWorkflowID += "-other" }},
		{"source mismatch", func(t *testing.T) []byte {
			input := valid
			input.SourceWorkflowID = "wf-other"
			return mustMarshal(t, input)
		}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newSweepTestStore()
			action := store.CardAction{
				WorkflowID: "wf1", Action: "retry", ActorOpenID: "ou_1",
				State: "pending", Attempt: valid.Attempt, Revision: 1,
				TargetWorkflowID: valid.WorkflowID(), TargetInput: tt.raw(t),
			}
			if tt.mutate != nil {
				tt.mutate(&action)
			}
			st.actions["wf1"] = action
			starter := &fakeConsumerStarter{started: true}
			s := &Sweeper{Store: st, Starter: starter}

			if err := s.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			got := st.actions["wf1"]
			if got.State != "failed" ||
				!strings.Contains(got.LastError, "invalid persisted target input") {
				t.Fatalf("action = %#v, want permanent invalid-input failure", got)
			}
			if starter.startCalls != 0 || st.finalizeCalls != 1 {
				t.Fatalf("start=%d finalize=%d, want 0/1", starter.startCalls, st.finalizeCalls)
			}
		})
	}
}

func TestActionRecoveryPermanentAndTransientStartErrors(t *testing.T) {
	tests := []struct {
		name          string
		startErr      error
		wantState     string
		wantFinalize  int
		wantRunErr    bool
		wantLastError bool
	}{
		{
			name:          "permanent invalid argument",
			startErr:      serviceerror.NewInvalidArgument("invalid target"),
			wantState:     "failed",
			wantFinalize:  1,
			wantLastError: true,
		},
		{
			name:         "transient unavailable",
			startErr:     serviceerror.NewUnavailable("temporal unavailable"),
			wantState:    "pending",
			wantFinalize: 0,
			wantRunErr:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, input := storeWithPendingAction(t)
			starter := &fakeConsumerStarter{started: false, err: tt.startErr}
			s := &Sweeper{Store: st, Starter: starter}

			err := s.RunOnce(context.Background())
			if (err != nil) != tt.wantRunErr {
				t.Fatalf("RunOnce error = %v, want error=%v", err, tt.wantRunErr)
			}
			action := st.actions["wf1"]
			if action.State != tt.wantState || st.finalizeCalls != tt.wantFinalize {
				t.Fatalf("action=%#v finalize=%d", action, st.finalizeCalls)
			}
			if tt.wantLastError && !strings.Contains(action.LastError, tt.startErr.Error()) {
				t.Fatalf("last error = %q, want %q", action.LastError, tt.startErr)
			}
			if !reflect.DeepEqual(starter.inputs, []wf.DeviceTestInput{input}) {
				t.Fatalf("starter inputs = %#v, want exact persisted input", starter.inputs)
			}
		})
	}
}

func TestActionFinalizeFailureIsReturnedAndLostFenceHasNoCompensation(t *testing.T) {
	t.Run("store error", func(t *testing.T) {
		st, _ := storeWithPendingAction(t)
		st.finalizeErr = errors.New("finalize database down")
		s := &Sweeper{Store: st, Starter: &fakeConsumerStarter{started: true}}
		err := s.RunOnce(context.Background())
		if err == nil || !strings.Contains(err.Error(), "finalize action") {
			t.Fatalf("RunOnce error = %v", err)
		}
	})

	t.Run("lost fencing", func(t *testing.T) {
		st, _ := storeWithPendingAction(t)
		st.finalizeOK = false
		s := &Sweeper{Store: st, Starter: &fakeConsumerStarter{started: true}}
		if err := s.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if st.completeCalls != 0 || st.deferCalls != 0 || st.abandonCalls != 0 {
			t.Fatal("lost action fencing caused compensating card writes")
		}
	})
}

func TestCardRecoveryMissingOrInvalidSnapshotNeverPatches(t *testing.T) {
	tests := []struct {
		name        string
		snapshot    []byte
		snapshotErr error
	}{
		{"missing", nil, store.ErrCardSnapshotNotFound},
		{"malformed", []byte(`{"config":`), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newSweepTestStore()
			st.messageClaim = messageClaimForSweep()
			st.snapshot = tt.snapshot
			st.snapshotErr = tt.snapshotErr
			updater := &fakeCardUpdater{}
			s := &Sweeper{Store: st, Updater: updater}

			err := s.RunOnce(context.Background())
			if err == nil {
				t.Fatal("RunOnce must surface snapshot/render failure")
			}
			if updater.calls != 0 || st.completeCalls != 0 ||
				st.deferCalls != 0 || st.abandonCalls != 0 {
				t.Fatalf("patch=%d complete=%d defer=%d abandon=%d, want all zero",
					updater.calls, st.completeCalls, st.deferCalls, st.abandonCalls)
			}
		})
	}
}

func TestCardRecoveryPatchSuccessCompletesOriginalClaim(t *testing.T) {
	st := newSweepTestStore()
	claim := messageClaimForSweep()
	st.messageClaim = claim
	updater := &fakeCardUpdater{}
	s := &Sweeper{Store: st, Updater: updater}

	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if updater.calls != 1 || !reflect.DeepEqual(updater.messageIDs, []string{"om_1"}) {
		t.Fatalf("patch calls=%d message IDs=%v", updater.calls, updater.messageIDs)
	}
	raw, ok := updater.cards[0].(json.RawMessage)
	if !ok || !json.Valid(raw) {
		t.Fatalf("PatchCard payload = %#v, want complete JSON RawMessage", updater.cards[0])
	}
	if st.completeCalls != 1 || st.deferCalls != 0 || st.abandonCalls != 0 {
		t.Fatalf("complete=%d defer=%d abandon=%d", st.completeCalls, st.deferCalls, st.abandonCalls)
	}
	if !reflect.DeepEqual(st.completeClaim, *claim) {
		t.Fatalf("completion claim changed:\ngot=%#v\nwant=%#v", st.completeClaim, *claim)
	}
	if st.completeToken == "" {
		t.Fatal("completion did not use claim token")
	}
}

func TestCardRecoveryTimeoutDefersForSixtySeconds(t *testing.T) {
	st := newSweepTestStore()
	st.messageClaim = messageClaimForSweep()
	updater := &fakeCardUpdater{err: fmt.Errorf("patch timed out: %w", context.DeadlineExceeded)}
	s := &Sweeper{Store: st, Updater: updater}

	err := s.RunOnce(context.Background())
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunOnce error = %v, want wrapped deadline", err)
	}
	if st.deferCalls != 1 || st.deferAfter != 60*time.Second ||
		st.completeCalls != 0 || st.abandonCalls != 0 {
		t.Fatalf("defer=%d after=%s complete=%d abandon=%d",
			st.deferCalls, st.deferAfter, st.completeCalls, st.abandonCalls)
	}
	if !strings.Contains(st.deferError, context.DeadlineExceeded.Error()) {
		t.Fatalf("defer last error = %q", st.deferError)
	}
}

func TestCardRecoveryClassifiesRealFeishuPatchErrors(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          string
		wantAbandon   int
		wantRunErr    bool
		wantTransient bool
	}{
		{
			name:        "message missing business error is permanent",
			status:      http.StatusBadRequest,
			body:        `{"code":230001,"msg":"message not found"}`,
			wantAbandon: 1,
		},
		{
			name:          "business rate limit is transient",
			status:        http.StatusBadRequest,
			body:          `{"code":230020,"msg":"frequency limit"}`,
			wantRunErr:    true,
			wantTransient: true,
		},
		{
			name:          "HTTP rate limit is transient",
			status:        http.StatusTooManyRequests,
			body:          `{"code":0,"msg":"rate limited"}`,
			wantRunErr:    true,
			wantTransient: true,
		},
		{
			name:          "HTTP 5xx is transient",
			status:        http.StatusServiceUnavailable,
			body:          `{"code":230001,"msg":"stale intermediary body"}`,
			wantRunErr:    true,
			wantTransient: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newSweepTestStore()
			st.messageClaim = messageClaimForSweep()
			updater := realPatchUpdater(t, tt.status, tt.body)
			s := &Sweeper{Store: st, Updater: updater}

			err := s.RunOnce(context.Background())
			if (err != nil) != tt.wantRunErr {
				t.Fatalf("RunOnce error = %v, want error=%v", err, tt.wantRunErr)
			}
			if st.abandonCalls != tt.wantAbandon {
				t.Fatalf("abandon calls = %d, want %d", st.abandonCalls, tt.wantAbandon)
			}
			if tt.wantTransient &&
				(st.completeCalls != 0 || st.deferCalls != 0 || st.abandonCalls != 0) {
				t.Fatalf("transient result wrote completion: complete=%d defer=%d abandon=%d",
					st.completeCalls, st.deferCalls, st.abandonCalls)
			}
		})
	}
}

func TestCardRecoveryUnknownErrorIsTransient(t *testing.T) {
	st := newSweepTestStore()
	st.messageClaim = messageClaimForSweep()
	updater := &fakeCardUpdater{err: errors.New("opaque patch failure")}
	s := &Sweeper{Store: st, Updater: updater}

	if err := s.RunOnce(context.Background()); err == nil {
		t.Fatal("unknown patch error must surface")
	}
	if st.completeCalls != 0 || st.deferCalls != 0 || st.abandonCalls != 0 {
		t.Fatal("unknown patch error must fail safe as transient")
	}
}

func TestCardRecoveryLostCompletionDoesNotFallback(t *testing.T) {
	st := newSweepTestStore()
	st.messageClaim = messageClaimForSweep()
	st.completeOK = false
	s := &Sweeper{Store: st, Updater: &fakeCardUpdater{}}

	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st.completeCalls != 1 || st.deferCalls != 0 || st.abandonCalls != 0 {
		t.Fatalf("complete=%d defer=%d abandon=%d, want 1/0/0",
			st.completeCalls, st.deferCalls, st.abandonCalls)
	}
}

func TestCardRecoveryNeverMutatesActionState(t *testing.T) {
	st := newSweepTestStore()
	claim := succeededRetryClaim("ou_1", "wf-target")
	st.messageClaim = &claim
	before := *claim.Action
	before.TargetInput = append([]byte(nil), claim.Action.TargetInput...)
	s := &Sweeper{Store: st, Updater: &fakeCardUpdater{}}

	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !reflect.DeepEqual(*claim.Action, before) {
		t.Fatalf("card patch mutated action:\ngot=%#v\nwant=%#v", *claim.Action, before)
	}
	if st.finalizeCalls != 0 {
		t.Fatalf("card sweep finalized action %d times", st.finalizeCalls)
	}
}

func TestRunStartsImmediatelyAndStopsOnCancellation(t *testing.T) {
	st := newSweepTestStore()
	st.passDone = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s := &Sweeper{Store: st}
	go func() {
		defer close(done)
		s.Run(ctx)
	}()

	select {
	case <-st.passDone:
	case <-time.After(time.Second):
		t.Fatal("Run did not execute an immediate startup pass")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
	if st.inboxSweepCalls != 1 || st.actionSweepCalls != 1 || st.cardSweepCalls != 1 {
		t.Fatalf("startup sweep calls = %d/%d/%d, want 1/1/1",
			st.inboxSweepCalls, st.actionSweepCalls, st.cardSweepCalls)
	}
}

func storeWithPendingAction(t *testing.T) (*sweepTestStore, wf.DeviceTestInput) {
	t.Helper()
	st := newSweepTestStore()
	input := buildTargetInput(fullResolution(), 4)
	st.actions["wf1"] = store.CardAction{
		WorkflowID: "wf1", Action: "retry", ActorOpenID: "ou_1",
		State: "pending", Attempt: input.Attempt, Revision: 1,
		TargetWorkflowID: input.WorkflowID(), TargetInput: mustMarshal(t, input),
	}
	return st, input
}

func messageClaimForSweep() *store.MessageClaim {
	claim := rejectionClaim("workflow 尚未结束", "none")
	return &claim
}

func cloneMessageClaim(claim *store.MessageClaim) *store.MessageClaim {
	if claim == nil {
		return nil
	}
	copy := copyMessageClaim(*claim)
	return &copy
}

func copyMessageClaim(claim store.MessageClaim) store.MessageClaim {
	if claim.Action != nil {
		action := *claim.Action
		action.TargetInput = append([]byte(nil), claim.Action.TargetInput...)
		claim.Action = &action
	}
	return claim
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func realPatchUpdater(t *testing.T, status int, body string) feishu.CardUpdater {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/tenant_access_token/internal"):
			_, _ = io.WriteString(w,
				`{"code":0,"tenant_access_token":"token","expire":7200}`)
		case strings.Contains(r.URL.Path, "/im/v1/messages/"):
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	sender, mode := feishu.NewSender(feishu.Config{
		AppID: "cli_a1", AppSecret: "secret", ReceiveID: "oc_chat",
		BaseURL: srv.URL, HTTP: srv.Client(),
	})
	if mode != "app" {
		t.Fatalf("NewSender mode = %q, want app", mode)
	}
	updater, ok := sender.(feishu.CardUpdater)
	if !ok {
		t.Fatal("app sender does not implement CardUpdater")
	}
	return updater
}
