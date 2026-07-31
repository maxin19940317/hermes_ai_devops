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
	"testing"
	"time"

	"github.com/rs/zerolog"
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
	claimTrace       []string
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

	passNotify chan struct{}
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
	s.claimTrace = append(s.claimTrace, "inbox")
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
	s.claimTrace = append(s.claimTrace, "action")
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
	s.claimTrace = append(s.claimTrace, "card")
	s.recordClaim(token, lease)
	if s.passNotify != nil {
		s.passNotify <- struct{}{}
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

func TestRunOnceOrdersAndContinuesAllSweepsAfterEachClaimFailure(t *testing.T) {
	tests := []struct {
		name   string
		pass   string
		setErr func(*sweepTestStore, error)
	}{
		{
			name: "inbox",
			pass: "inbox",
			setErr: func(st *sweepTestStore, err error) {
				st.inboxClaimErr = err
			},
		},
		{
			name: "action",
			pass: "action",
			setErr: func(st *sweepTestStore, err error) {
				st.actionClaimErr = err
			},
		},
		{
			name: "card",
			pass: "card",
			setErr: func(st *sweepTestStore, err error) {
				st.cardClaimErr = err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newSweepTestStore()
			passErr := errors.New(tt.pass + " database down")
			tt.setErr(st, passErr)
			s := &Sweeper{Store: st}

			err := s.RunOnce(context.Background())
			if err == nil || !errors.Is(err, passErr) ||
				!strings.Contains(err.Error(), tt.pass+" sweep") {
				t.Fatalf("RunOnce error = %v, want joined contextual %s error", err, tt.pass)
			}
			if !reflect.DeepEqual(st.claimTrace, []string{"inbox", "action", "card"}) {
				t.Fatalf("claim trace = %v, want [inbox action card]", st.claimTrace)
			}
			if st.inboxSweepCalls != 1 || st.actionSweepCalls != 1 || st.cardSweepCalls != 1 {
				t.Fatalf("sweep calls = inbox:%d action:%d card:%d, want 1 each",
					st.inboxSweepCalls, st.actionSweepCalls, st.cardSweepCalls)
			}
			assertFreshSweepClaims(t, st.claimTokens, st.claimLeases)
		})
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

func TestCardRecoveryDeterministicSnapshotFailuresAbandonOriginalClaim(t *testing.T) {
	tests := []struct {
		name          string
		snapshot      []byte
		snapshotErr   error
		wantLastError string
	}{
		{
			name:          "missing snapshot",
			snapshotErr:   store.ErrCardSnapshotNotFound,
			wantLastError: "get card snapshot wf-source: card action snapshot not found",
		},
		{
			name:          "malformed snapshot",
			snapshot:      []byte(`{"config":`),
			wantLastError: "render message om_1: render card: parse snapshot: invalid JSON",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newSweepTestStore()
			claim := richMessageClaimForSweep()
			st.messageClaim = &claim
			original := copyMessageClaim(claim)
			st.snapshot = tt.snapshot
			st.snapshotErr = tt.snapshotErr
			updater := &fakeCardUpdater{}
			s := &Sweeper{Store: st, Updater: updater}

			if err := s.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if updater.calls != 0 || st.completeCalls != 0 || st.deferCalls != 0 ||
				st.abandonCalls != 1 {
				t.Fatalf("patch=%d complete=%d defer=%d abandon=%d, want 0/0/0/1",
					updater.calls, st.completeCalls, st.deferCalls, st.abandonCalls)
			}
			assertExactMessageCompletion(
				t, st.abandonClaim, st.abandonToken, original, st.claimTokens[2],
			)
			if st.abandonError != tt.wantLastError {
				t.Fatalf("abandon last error = %q, want %q",
					st.abandonError, tt.wantLastError)
			}
			if !reflect.DeepEqual(claim, original) {
				t.Fatalf("deterministic failure mutated action:\ngot=%#v\nwant=%#v",
					claim, original)
			}
		})
	}
}

func TestCardRecoveryTransientSnapshotFailureRemainsRetryable(t *testing.T) {
	st := newSweepTestStore()
	claim := richMessageClaimForSweep()
	st.messageClaim = &claim
	snapshotErr := errors.New("snapshot database unavailable")
	st.snapshotErr = snapshotErr
	updater := &fakeCardUpdater{}
	s := &Sweeper{Store: st, Updater: updater}

	err := s.RunOnce(context.Background())
	if !errors.Is(err, snapshotErr) ||
		!strings.Contains(err.Error(), "get card snapshot wf-source") {
		t.Fatalf("RunOnce error = %v, want wrapped transient snapshot error", err)
	}
	if updater.calls != 0 || st.completeCalls != 0 ||
		st.deferCalls != 0 || st.abandonCalls != 0 {
		t.Fatalf("patch=%d complete=%d defer=%d abandon=%d, want all zero",
			updater.calls, st.completeCalls, st.deferCalls, st.abandonCalls)
	}
}

func TestCardRecoveryDeterministicFailureAbandonErrorsJoinBothCauses(t *testing.T) {
	tests := []struct {
		name              string
		snapshot          []byte
		snapshotErr       error
		wantOriginalCause error
		wantText          string
	}{
		{
			name:              "missing snapshot",
			snapshotErr:       store.ErrCardSnapshotNotFound,
			wantOriginalCause: store.ErrCardSnapshotNotFound,
			wantText:          "get card snapshot wf-source",
		},
		{
			name:     "malformed snapshot",
			snapshot: []byte(`{"config":`),
			wantText: "render message om_1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newSweepTestStore()
			claim := richMessageClaimForSweep()
			st.messageClaim = &claim
			st.snapshot = tt.snapshot
			st.snapshotErr = tt.snapshotErr
			abandonErr := errors.New("abandon database unavailable")
			st.abandonErr = abandonErr
			s := &Sweeper{Store: st, Updater: &fakeCardUpdater{}}

			err := s.RunOnce(context.Background())
			if !errors.Is(err, abandonErr) {
				t.Fatalf("RunOnce error = %v, want abandon store error", err)
			}
			if tt.wantOriginalCause != nil && !errors.Is(err, tt.wantOriginalCause) {
				t.Fatalf("RunOnce error = %v, want original cause %v",
					err, tt.wantOriginalCause)
			}
			if !strings.Contains(err.Error(), tt.wantText) ||
				!strings.Contains(err.Error(), "abandon message render om_1") {
				t.Fatalf("RunOnce error = %v, want deterministic and abandon context", err)
			}
			if st.abandonCalls != 1 || st.completeCalls != 0 || st.deferCalls != 0 {
				t.Fatalf("complete/defer/abandon = %d/%d/%d, want 0/0/1",
					st.completeCalls, st.deferCalls, st.abandonCalls)
			}
		})
	}
}

func TestCardRecoveryDeterministicFailureLostFenceHasNoCompensation(t *testing.T) {
	st := newSweepTestStore()
	claim := richMessageClaimForSweep()
	st.messageClaim = &claim
	st.snapshotErr = store.ErrCardSnapshotNotFound
	st.abandonOK = false
	s := &Sweeper{Store: st, Updater: &fakeCardUpdater{}}

	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st.abandonCalls != 1 || st.completeCalls != 0 || st.deferCalls != 0 ||
		st.finalizeCalls != 0 {
		t.Fatalf("complete/defer/abandon/finalize = %d/%d/%d/%d, want 0/0/1/0",
			st.completeCalls, st.deferCalls, st.abandonCalls, st.finalizeCalls)
	}
}

func TestCardRecoveryCompletionsForwardExactClaimAndThirdToken(t *testing.T) {
	ambiguousErr := fmt.Errorf("patch timed out: %w", context.DeadlineExceeded)
	tests := []struct {
		name          string
		claim         func() store.MessageClaim
		updater       func(*testing.T) feishu.CardUpdater
		wantRunErr    error
		wantComplete  int
		wantDefer     int
		wantAbandon   int
		wantLastError string
	}{
		{
			name:         "success completes",
			claim:        richMessageClaimForSweep,
			updater:      func(*testing.T) feishu.CardUpdater { return &fakeCardUpdater{} },
			wantComplete: 1,
		},
		{
			name: "ambiguous defers",
			claim: func() store.MessageClaim {
				claim := rejectionClaim("workflow 尚未结束 exact snapshot", "both")
				claim.DesiredRevision = 7
				return claim
			},
			updater: func(*testing.T) feishu.CardUpdater {
				return &fakeCardUpdater{err: ambiguousErr}
			},
			wantDefer:     1,
			wantLastError: ambiguousErr.Error(),
		},
		{
			name:  "unknown acknowledgement defers",
			claim: richMessageClaimForSweep,
			updater: func(t *testing.T) feishu.CardUpdater {
				return realPatchUpdater(t, http.StatusOK, `{}`)
			},
			wantDefer:     1,
			wantLastError: "feishu: patch result unknown: code field is missing",
		},
		{
			name:  "permanent abandons",
			claim: richMessageClaimForSweep,
			updater: func(t *testing.T) feishu.CardUpdater {
				return realPatchUpdater(
					t, http.StatusBadRequest,
					`{"code":230001,"msg":"message not found"}`,
				)
			},
			wantAbandon:   1,
			wantLastError: "feishu api: code 230001: message not found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newSweepTestStore()
			claim := tt.claim()
			st.messageClaim = &claim
			original := copyMessageClaim(claim)
			updater := tt.updater(t)
			s := &Sweeper{Store: st, Updater: updater}

			err := s.RunOnce(context.Background())
			if tt.wantRunErr == nil && err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if tt.wantRunErr != nil && !errors.Is(err, tt.wantRunErr) {
				t.Fatalf("RunOnce error = %v, want %v", err, tt.wantRunErr)
			}
			if st.completeCalls != tt.wantComplete ||
				st.deferCalls != tt.wantDefer ||
				st.abandonCalls != tt.wantAbandon {
				t.Fatalf("complete/defer/abandon = %d/%d/%d, want %d/%d/%d",
					st.completeCalls, st.deferCalls, st.abandonCalls,
					tt.wantComplete, tt.wantDefer, tt.wantAbandon)
			}
			if len(st.claimTokens) != 3 {
				t.Fatalf("claim tokens = %v, want exactly three", st.claimTokens)
			}
			token := st.claimTokens[2]
			switch {
			case tt.wantComplete == 1:
				fake := updater.(*fakeCardUpdater)
				if fake.calls != 1 ||
					!reflect.DeepEqual(fake.messageIDs, []string{"om_1"}) {
					t.Fatalf("patch calls=%d message IDs=%v", fake.calls, fake.messageIDs)
				}
				raw, ok := fake.cards[0].(json.RawMessage)
				if !ok || !json.Valid(raw) {
					t.Fatalf("PatchCard payload = %#v, want complete JSON RawMessage",
						fake.cards[0])
				}
				assertExactMessageCompletion(
					t, st.completeClaim, st.completeToken, original, token,
				)
			case tt.wantDefer == 1:
				assertExactMessageCompletion(
					t, st.deferClaim, st.deferToken, original, token,
				)
				if st.deferAfter != 60*time.Second {
					t.Fatalf("defer after = %s, want 60s", st.deferAfter)
				}
				if st.deferError != tt.wantLastError {
					t.Fatalf("defer error = %q, want %q", st.deferError, tt.wantLastError)
				}
			case tt.wantAbandon == 1:
				assertExactMessageCompletion(
					t, st.abandonClaim, st.abandonToken, original, token,
				)
				if st.abandonError != tt.wantLastError {
					t.Fatalf("abandon error = %q, want %q", st.abandonError, tt.wantLastError)
				}
			}
			if !reflect.DeepEqual(claim, original) {
				t.Fatalf("claimed snapshot mutated:\ngot=%#v\nwant=%#v", claim, original)
			}
		})
	}
}

func TestCardRecoveryAmbiguousDeferFailureJoinsPatchAndStoreErrors(t *testing.T) {
	st := newSweepTestStore()
	claim := richMessageClaimForSweep()
	st.messageClaim = &claim
	patchErr := fmt.Errorf("patch timed out: %w", context.DeadlineExceeded)
	deferErr := errors.New("defer database unavailable")
	st.deferErr = deferErr
	s := &Sweeper{Store: st, Updater: &fakeCardUpdater{err: patchErr}}

	err := s.RunOnce(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, deferErr) {
		t.Fatalf("RunOnce error = %v, want joined patch and defer errors", err)
	}
	if !strings.Contains(err.Error(), "patch message om_1 was ambiguous") ||
		!strings.Contains(err.Error(), "defer message render om_1") {
		t.Fatalf("RunOnce error = %v, want patch and defer context", err)
	}
	if st.deferCalls != 1 || st.completeCalls != 0 || st.abandonCalls != 0 {
		t.Fatalf("complete/defer/abandon = %d/%d/%d, want 0/1/0",
			st.completeCalls, st.deferCalls, st.abandonCalls)
	}
}

func TestCardRecoverySuccessfulAmbiguityDeferralLogsWarningNotSweepFailure(t *testing.T) {
	st := newSweepTestStore()
	claim := richMessageClaimForSweep()
	st.messageClaim = &claim
	patchErr := fmt.Errorf("patch timed out: %w", context.DeadlineExceeded)
	var logs strings.Builder
	logger := zerolog.New(&logs)
	s := &Sweeper{
		Store: st, Updater: &fakeCardUpdater{err: patchErr}, Log: &logger,
	}

	s.runAndLog(context.Background())

	got := logs.String()
	if !strings.Contains(got, "card patch result is ambiguous; render deferred") {
		t.Fatalf("logs = %s, want ambiguity warning", got)
	}
	if strings.Contains(got, "card action recovery sweep failed") {
		t.Fatalf("logs = %s, successful deferral must not log sweep failure", got)
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
		wantLastError string
	}{
		{
			name:          "message missing business error is permanent",
			status:        http.StatusBadRequest,
			body:          `{"code":230001,"msg":"message not found"}`,
			wantAbandon:   1,
			wantLastError: "feishu api: code 230001: message not found",
		},
		{
			name:          "permission denied business error is permanent",
			status:        http.StatusForbidden,
			body:          `{"code":230027,"msg":"permission denied"}`,
			wantAbandon:   1,
			wantLastError: "feishu api: code 230027: permission denied",
		},
		{
			name:          "message outside update window is permanent",
			status:        http.StatusBadRequest,
			body:          `{"code":230031,"msg":"message older than 14 days"}`,
			wantAbandon:   1,
			wantLastError: "feishu api: code 230031: message older than 14 days",
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
			name:          "unknown business error is transient",
			status:        http.StatusBadRequest,
			body:          `{"code":239999,"msg":"unknown business failure"}`,
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
			claim := richMessageClaimForSweep()
			st.messageClaim = &claim
			original := copyMessageClaim(claim)
			updater := realPatchUpdater(t, tt.status, tt.body)
			s := &Sweeper{Store: st, Updater: updater}

			err := s.RunOnce(context.Background())
			if (err != nil) != tt.wantRunErr {
				t.Fatalf("RunOnce error = %v, want error=%v", err, tt.wantRunErr)
			}
			if st.abandonCalls != tt.wantAbandon {
				t.Fatalf("abandon calls = %d, want %d", st.abandonCalls, tt.wantAbandon)
			}
			if tt.wantAbandon == 1 {
				if st.completeCalls != 0 || st.deferCalls != 0 {
					t.Fatalf("permanent result wrote complete/defer = %d/%d",
						st.completeCalls, st.deferCalls)
				}
				assertExactMessageCompletion(
					t, st.abandonClaim, st.abandonToken, original, st.claimTokens[2],
				)
				if st.abandonError != tt.wantLastError {
					t.Fatalf("abandon error = %q, want %q", st.abandonError, tt.wantLastError)
				}
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

func TestCardRecoveryLostFencingNeverFallsBackOrMutatesAction(t *testing.T) {
	ambiguousErr := fmt.Errorf("patch timed out: %w", context.DeadlineExceeded)
	tests := []struct {
		name         string
		updater      func(*testing.T) feishu.CardUpdater
		loseFence    func(*sweepTestStore)
		wantRunErr   error
		wantComplete int
		wantDefer    int
		wantAbandon  int
	}{
		{
			name:    "complete",
			updater: func(*testing.T) feishu.CardUpdater { return &fakeCardUpdater{} },
			loseFence: func(st *sweepTestStore) {
				st.completeOK = false
			},
			wantComplete: 1,
		},
		{
			name: "defer",
			updater: func(*testing.T) feishu.CardUpdater {
				return &fakeCardUpdater{err: ambiguousErr}
			},
			loseFence: func(st *sweepTestStore) {
				st.deferOK = false
			},
			wantDefer: 1,
		},
		{
			name: "abandon",
			updater: func(t *testing.T) feishu.CardUpdater {
				return realPatchUpdater(
					t, http.StatusForbidden,
					`{"code":230027,"msg":"permission denied"}`,
				)
			},
			loseFence: func(st *sweepTestStore) {
				st.abandonOK = false
			},
			wantAbandon: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newSweepTestStore()
			claim := richMessageClaimForSweep()
			st.messageClaim = &claim
			before := copyMessageClaim(claim)
			tt.loseFence(st)
			s := &Sweeper{Store: st, Updater: tt.updater(t)}

			err := s.RunOnce(context.Background())
			if tt.wantRunErr == nil && err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if tt.wantRunErr != nil && !errors.Is(err, tt.wantRunErr) {
				t.Fatalf("RunOnce error = %v, want %v", err, tt.wantRunErr)
			}
			if st.completeCalls != tt.wantComplete ||
				st.deferCalls != tt.wantDefer ||
				st.abandonCalls != tt.wantAbandon {
				t.Fatalf("complete/defer/abandon = %d/%d/%d, want %d/%d/%d",
					st.completeCalls, st.deferCalls, st.abandonCalls,
					tt.wantComplete, tt.wantDefer, tt.wantAbandon)
			}
			if st.finalizeCalls != 0 {
				t.Fatalf("lost message fence finalized action %d times", st.finalizeCalls)
			}
			var gotClaim store.MessageClaim
			var gotToken string
			switch {
			case tt.wantComplete == 1:
				gotClaim, gotToken = st.completeClaim, st.completeToken
			case tt.wantDefer == 1:
				gotClaim, gotToken = st.deferClaim, st.deferToken
			case tt.wantAbandon == 1:
				gotClaim, gotToken = st.abandonClaim, st.abandonToken
			}
			assertExactMessageCompletion(
				t, gotClaim, gotToken, before, st.claimTokens[2],
			)
			if !reflect.DeepEqual(claim, before) {
				t.Fatalf("lost message fence mutated action:\ngot=%#v\nwant=%#v",
					claim, before)
			}
		})
	}
}

func TestCardRecoveryNeverMutatesActionState(t *testing.T) {
	st := newSweepTestStore()
	claim := succeededRetryClaim("ou_1", "wf-target")
	st.messageClaim = &claim
	beforeClaim := copyMessageClaim(claim)
	before := *claim.Action
	before.TargetInput = append([]byte(nil), claim.Action.TargetInput...)
	s := &Sweeper{Store: st, Updater: &fakeCardUpdater{}}

	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !reflect.DeepEqual(st.completeClaim, beforeClaim) {
		t.Fatalf("card patch completion mutated action:\ngot=%#v\nwant=%#v",
			st.completeClaim, beforeClaim)
	}
	if !reflect.DeepEqual(*claim.Action, before) {
		t.Fatalf("card patch mutated action:\ngot=%#v\nwant=%#v", *claim.Action, before)
	}
	if st.finalizeCalls != 0 {
		t.Fatalf("card sweep finalized action %d times", st.finalizeCalls)
	}
}

func TestSweeperRunIntervalDefaultsToThirtySeconds(t *testing.T) {
	if got := (&Sweeper{}).runInterval(); got != 30*time.Second {
		t.Fatalf("default run interval = %s, want 30s", got)
	}
	if got := (&Sweeper{interval: 7 * time.Millisecond}).runInterval(); got != 7*time.Millisecond {
		t.Fatalf("configured run interval = %s, want 7ms", got)
	}
}

func TestRunRepeatsAfterErrorsAndStopsPromptlyOnCancellation(t *testing.T) {
	st := newSweepTestStore()
	st.inboxClaimErr = errors.New("inbox database remains unavailable")
	st.passNotify = make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s := &Sweeper{Store: st, interval: 500 * time.Millisecond}
	go func() {
		defer close(done)
		s.Run(ctx)
	}()

	waitForSweepPass(t, st.passNotify, "immediate", 250*time.Millisecond)
	waitForSweepPass(t, st.passNotify, "timed", 2*time.Second)
	cancel()
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Run did not return after context cancellation")
	}
	if st.inboxSweepCalls < 2 || st.actionSweepCalls < 2 || st.cardSweepCalls < 2 {
		t.Fatalf("sweep calls = %d/%d/%d, want at least two complete passes",
			st.inboxSweepCalls, st.actionSweepCalls, st.cardSweepCalls)
	}
	wantPrefix := []string{"inbox", "action", "card", "inbox", "action", "card"}
	if len(st.claimTrace) < len(wantPrefix) ||
		!reflect.DeepEqual(st.claimTrace[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("claim trace prefix = %v, want %v", st.claimTrace, wantPrefix)
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

func richMessageClaimForSweep() store.MessageClaim {
	claim := succeededRetryClaim("ou_exact_actor", "wf-target-exact")
	claim.DesiredRevision = 7
	claim.Action.Revision = 7
	claim.Action.Owner = "persisted-action-owner"
	claim.Action.Attempt = 9
	claim.Action.TargetInput = []byte(
		`{"source_workflow_id":"wf-source","attempt":9,"nested":{"variant":"android"}}`,
	)
	return claim
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

func assertFreshSweepClaims(t *testing.T, tokens []string, leases []time.Duration) {
	t.Helper()
	if len(tokens) != 3 || len(leases) != 3 {
		t.Fatalf("claims = tokens:%d leases:%d, want 3 each", len(tokens), len(leases))
	}
	seen := make(map[string]bool)
	for i, token := range tokens {
		raw, decodeErr := hex.DecodeString(token)
		if decodeErr != nil || len(raw) != 16 {
			t.Fatalf("claim %d token %q = %x, %v; want unpredictable 128-bit hex",
				i, token, raw, decodeErr)
		}
		if seen[token] {
			t.Fatalf("fencing token reused: %q", token)
		}
		seen[token] = true
		if leases[i] != 120*time.Second {
			t.Fatalf("claim %d lease = %s, want 120s", i, leases[i])
		}
	}
}

func assertExactMessageCompletion(
	t *testing.T,
	gotClaim store.MessageClaim,
	gotToken string,
	wantClaim store.MessageClaim,
	wantToken string,
) {
	t.Helper()
	if !reflect.DeepEqual(gotClaim, wantClaim) {
		t.Fatalf("completion claim changed:\ngot=%#v\nwant=%#v", gotClaim, wantClaim)
	}
	if gotToken != wantToken {
		t.Fatalf("completion token = %q, want exact third claim token %q", gotToken, wantToken)
	}
}

func waitForSweepPass(
	t *testing.T, passes <-chan struct{}, name string, timeout time.Duration,
) {
	t.Helper()
	select {
	case <-passes:
	case <-time.After(timeout):
		t.Fatalf("Run did not complete %s sweep pass", name)
	}
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
