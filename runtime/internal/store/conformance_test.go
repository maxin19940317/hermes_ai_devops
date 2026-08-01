package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	wf "hermes-devops/runtime/internal/workflow"
)

var ctx = context.Background()

type asyncCompletionResult struct {
	out *AcceptOutcome
	err error
}

// fullStore 是 workflow 活动(internal/activity.Store)与回调服务
// (internal/callbacks.Store)所需持久层方法的并集;
// MemStore 与 PGStore 必须行为一致,由本套件保证。
type fullStore interface {
	RegisterArtifacts(ctx context.Context, arts []Artifact) error
	UpsertClientDevices(ctx context.Context, c Client, devs []Device) error
	AcquireDevice(ctx context.Context, sel wf.DeviceSelector, taskID string, leaseSeconds int) (*wf.Lease, error)
	ReleaseDevice(ctx context.Context, deviceID, taskID string, scope wf.FailScope, quarantineAfter int) error
	RenewLease(ctx context.Context, cred LeaseCredential, leaseSeconds int) (bool, error)
	VerifyLease(ctx context.Context, cred LeaseCredential) (bool, error)
	GetLeaseExpiry(ctx context.Context, taskID string) (*time.Time, error)
	HasCapableDevice(ctx context.Context, sel wf.DeviceSelector) (bool, error)
	CreateTask(ctx context.Context, row wf.TaskRow) error
	GetTask(ctx context.Context, taskID string) (*wf.TaskRow, error)
	SetTaskStatus(ctx context.Context, taskID, status string) error
	FinishTask(ctx context.Context, req wf.FinishRequest) error
	ConclusiveWorkflowIDs(ctx context.Context, workflowIDs []string) (map[string]bool, error)
	AppendTaskEvent(ctx context.Context, ev TaskEvent) (bool, error)
	SaveResult(ctx context.Context, rec wf.ResultRecord) (bool, error)
	SaveResultWithOutbox(ctx context.Context, rec wf.ResultRecord, ev OutboxEvent) (bool, error)
	GetResult(ctx context.Context, taskID string) (*wf.ResultRecord, error)
	ClaimUnpublished(ctx context.Context, limit int) ([]OutboxEvent, error)
	MarkPublished(ctx context.Context, id int64) error
	MarkFailed(ctx context.Context, id int64, cause string) error
	OutboxBacklog(ctx context.Context, stuckAttempts int) (*OutboxBacklog, error)
	SaveDecision(ctx context.Context, row wf.DecisionRow) error
	ListDecisions(ctx context.Context, taskID string) ([]wf.DecisionRow, error)
	NextWorkflowAttempt(ctx context.Context, project, commitSHA string, pipelineID int, variant string) (int, error)
	SaveEvidenceSnapshot(ctx context.Context, snap EvidenceSnapshot) error
	GetEvidenceSnapshot(ctx context.Context, evidenceID string) (*EvidenceSnapshot, error)
	FleetOverview(ctx context.Context) (*FleetOverview, error)
	UnquarantineDevice(ctx context.Context, deviceID string) (bool, error)
	ListArtifacts(ctx context.Context, project, commitSHA string, pipelineID int) ([]Artifact, error)
	NextWorkflowAttemptAll(ctx context.Context, project, commitSHA string, pipelineID int) (int, error)
	SaveCommandTranslation(ctx context.Context, row CommandTranslation) error
	ListCommandTranslations(ctx context.Context, openID string, limit int) ([]CommandTranslation, error)
	RecentRuns(ctx context.Context, limit int) ([]RecentRun, error)
	RecordWorkflowRun(ctx context.Context, run WorkflowRun) error
	GetWorkflowRun(ctx context.Context, workflowID string) (*WorkflowRun, error)
	PutInbox(ctx context.Context, row InboxRow, auditOnReject *AuditRow) (*InboxRow, bool, error)
	GetInbox(ctx context.Context, eventID string) (*InboxRow, error)
	ClaimInbox(ctx context.Context, eventID, token string, lease time.Duration) (*InboxRow, error)
	GetCardAction(ctx context.Context, workflowID string) (*CardAction, error)
	CompleteAccept(ctx context.Context, req AcceptRequest) (*AcceptOutcome, error)
	CompleteReject(ctx context.Context, eventID, token string, r RejectRender) error
	FinalizeAction(ctx context.Context, workflowID, token, state, lastErr string) (bool, error)
	PutCardSnapshot(ctx context.Context, workflowID string, cardJSON []byte) error
	GetCardSnapshot(ctx context.Context, workflowID string) ([]byte, error)
	ClaimMessage(ctx context.Context, token string, lease time.Duration) (*MessageClaim, error)
	CompleteMessageRender(ctx context.Context, claim MessageClaim, token string) (bool, error)
	DeferMessageRender(ctx context.Context, claim MessageClaim, token string, after time.Duration, lastErr string) (bool, error)
	AbandonMessageRender(ctx context.Context, claim MessageClaim, token, lastErr string) (bool, error)
	ClaimStaleAction(ctx context.Context, token string, lease time.Duration) (*CardAction, error)
	ClaimStaleInbox(ctx context.Context, token string, lease time.Duration) (*InboxRow, error)
}

func TestMemStoreConformance(t *testing.T) {
	runConformance(t, func(t *testing.T) fullStore { return NewMemStore() })
}

func TestPGStoreConformance(t *testing.T) {
	runConformance(t, func(t *testing.T) fullStore { return openTestPG(t) })
}

func TestPGCardActionRetryCheckRejectsIncompleteRow(t *testing.T) {
	s := openTestPG(t)
	seedRunAndArtifacts(t, s, "wf-check", "grp/p", "abcd1234", 42, "v1")
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO card_actions (workflow_id, action, actor_open_id, state)
		VALUES ('wf-check', 'retry', 'ou_x', 'pending')`); err == nil {
		t.Fatal("retry without attempt/target/target_input must fail the database CHECK")
	}
	if actionExists(t, s, "wf-check") {
		t.Fatal("failed incomplete insert left a card_actions row")
	}
}

func TestPGCompleteAcceptSerializesWithFinalize(t *testing.T) {
	s := openTestPG(t)
	seedAcceptedRetry(t, s, "wf1", "tokA")

	token := claimForTestMessage(t, s, "observed", "wf1", "retry", "om_race")
	out, err := s.CompleteAccept(
		ctx, acceptReqMessage("observed", token, "wf1", "retry", "om_race"),
	)
	if err != nil || out.Kind != "conflict" {
		t.Fatalf("register race message = (%#v, %v)", out, err)
	}

	token = claimForTestMessage(t, s, "racing", "wf1", "retry", "om_race")
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin finalize transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	var finalizerPID int
	if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&finalizerPID); err != nil {
		t.Fatalf("finalizer backend PID: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE card_actions
		   SET state='succeeded', revision=revision+1,
		       owner='', lease_expires_at=NULL, updated_at=now()
		 WHERE workflow_id='wf1'`); err != nil {
		t.Fatalf("advance action revision: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE card_action_messages
		   SET desired_revision=2, update_state='pending',
		       owner='', lease_expires_at=NULL, reconcile_after=NULL,
		       updated_at=now()
		 WHERE workflow_id='wf1'`); err != nil {
		t.Fatalf("reorder messages: %v", err)
	}

	type acceptResult struct {
		out *AcceptOutcome
		err error
	}
	result := make(chan acceptResult, 1)
	go func() {
		out, err := s.CompleteAccept(
			ctx, acceptReqMessage("racing", token, "wf1", "retry", "om_race"),
		)
		result <- acceptResult{out: out, err: err}
	}()

	waitForBlockedCardActionQuery(t, s, finalizerPID)

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit finalize transaction: %v", err)
	}
	got := <-result
	if got.err != nil || got.out == nil || got.out.Kind != "conflict" {
		t.Fatalf("racing CompleteAccept = (%#v, %v)", got.out, got.err)
	}
	message := mustGetActionMessage(t, s, "wf1", "om_race")
	if message.DesiredRevision != 2 {
		t.Fatalf("stale accept overwrote finalized revision: %#v", message)
	}
}

func TestPGCompletionRechecksLeaseAfterLockWait(t *testing.T) {
	t.Run("CompleteAccept", func(t *testing.T) {
		s := openTestPG(t)
		seedRunAndArtifacts(t, s, "wf1", "grp/p", "abcd1234", 42, "v1")
		token := claimForTest(t, s, "e1", "wf1", "retry")
		expires := shortenInboxLease(t, s, "e1")

		blocker, blockerPID := lockCardActionRow(
			t, s, `SELECT event_id FROM card_action_inbox WHERE event_id='e1' FOR UPDATE`,
		)
		type acceptResult struct {
			out *AcceptOutcome
			err error
		}
		result := make(chan acceptResult, 1)
		go func() {
			out, err := s.CompleteAccept(ctx, acceptReq("e1", token, "wf1", "retry"))
			result <- acceptResult{out: out, err: err}
		}()
		waitForBlockedCardActionQuery(t, s, blockerPID)
		sleepPast(t, expires)
		if err := blocker.Commit(); err != nil {
			t.Fatalf("release inbox lock: %v", err)
		}

		got := <-result
		if got.err != nil || got.out == nil || got.out.Kind != "lost" {
			t.Fatalf("CompleteAccept after lease expiry = (%#v, %v), want lost", got.out, got.err)
		}
		if actionExists(t, s, "wf1") || countAudit(t, s, "e1") != 0 ||
			mustGetInbox(t, s, "e1").State != "received" {
			t.Fatal("expired CompleteAccept wrote business state after lock wait")
		}
	})

	t.Run("CompleteReject", func(t *testing.T) {
		s := openTestPG(t)
		token := claimForTest(t, s, "e1", "wf1", "retry")
		expires := shortenInboxLease(t, s, "e1")

		blocker, blockerPID := lockCardActionRow(
			t, s, `SELECT event_id FROM card_action_inbox WHERE event_id='e1' FOR UPDATE`,
		)
		result := make(chan error, 1)
		go func() {
			result <- s.CompleteReject(ctx, "e1", token, RejectRender{
				Code: "StillRunning", RejectionReason: "still running",
			})
		}()
		waitForBlockedCardActionQuery(t, s, blockerPID)
		sleepPast(t, expires)
		if err := blocker.Commit(); err != nil {
			t.Fatalf("release inbox lock: %v", err)
		}

		if err := <-result; err != nil {
			t.Fatalf("CompleteReject after lease expiry: %v", err)
		}
		if actionMessageExists(t, s, "wf1", "om_shared") || countAudit(t, s, "e1") != 0 ||
			mustGetInbox(t, s, "e1").State != "received" {
			t.Fatal("expired CompleteReject wrote business state after lock wait")
		}
	})

	t.Run("FinalizeAction", func(t *testing.T) {
		s := openTestPG(t)
		seedAcceptedRetry(t, s, "wf1", "tokA")

		blocker, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin action blocker: %v", err)
		}
		defer func() { _ = blocker.Rollback() }()
		var blockerPID int
		var expires time.Time
		if err := blocker.QueryRowContext(ctx, `
			UPDATE card_actions
			   SET lease_expires_at=clock_timestamp()+interval '250 milliseconds'
			 WHERE workflow_id='wf1'
			RETURNING pg_backend_pid(), lease_expires_at`).Scan(&blockerPID, &expires); err != nil {
			t.Fatalf("shorten and lock action lease: %v", err)
		}

		type finalizeResult struct {
			ok  bool
			err error
		}
		result := make(chan finalizeResult, 1)
		go func() {
			ok, err := s.FinalizeAction(ctx, "wf1", "tokA", "succeeded", "")
			result <- finalizeResult{ok: ok, err: err}
		}()
		waitForBlockedCardActionQuery(t, s, blockerPID)
		sleepPast(t, expires)
		if err := blocker.Commit(); err != nil {
			t.Fatalf("release action lock: %v", err)
		}

		got := <-result
		if got.err != nil || got.ok {
			t.Fatalf("FinalizeAction after lease expiry = (%v, %v), want false", got.ok, got.err)
		}
		action := mustGetAction(t, s, "wf1")
		if action.State != "pending" || action.Revision != 1 {
			t.Fatalf("expired FinalizeAction changed action after lock wait: %#v", action)
		}
	})
}

func TestPGCompletionRechecksInboxLeaseAfterWorkflowWait(t *testing.T) {
	cases := []struct {
		name string
		call func(*PGStore, string) (*AcceptOutcome, error)
	}{
		{name: "CompleteAccept", call: func(s *PGStore, token string) (*AcceptOutcome, error) {
			return s.CompleteAccept(ctx, acceptReq("e1", token, "wf1", "retry"))
		}},
		{name: "CompleteReject", call: func(s *PGStore, token string) (*AcceptOutcome, error) {
			err := s.CompleteReject(ctx, "e1", token, RejectRender{
				Code: "StillRunning", RejectionReason: "still running",
			})
			return nil, err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestPG(t)
			seedRunAndArtifacts(t, s, "wf1", "grp/p", "abcd1234", 42, "v1")
			token := claimForTest(t, s, "e1", "wf1", "retry")
			expires := shortenInboxLease(t, s, "e1")

			blocker, err := s.DB.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin workflow blocker: %v", err)
			}
			defer func() { _ = blocker.Rollback() }()
			var blockerPID int
			if err := blocker.QueryRowContext(ctx, `
				SELECT pg_backend_pid()
				  FROM workflow_runs
				 WHERE workflow_id='wf1'
				 FOR UPDATE`).Scan(&blockerPID); err != nil {
				t.Fatalf("lock workflow run: %v", err)
			}

			result := make(chan asyncCompletionResult, 1)
			go func() {
				out, err := tc.call(s, token)
				result <- asyncCompletionResult{out: out, err: err}
			}()
			waitForBlockedWorkflowQuery(t, s, blockerPID, result)
			sleepPast(t, expires)
			if err := blocker.Commit(); err != nil {
				t.Fatalf("release workflow lock: %v", err)
			}

			got := <-result
			if got.err != nil {
				t.Fatalf("%s: %v", tc.name, got.err)
			}
			if tc.name == "CompleteAccept" && (got.out == nil || got.out.Kind != "lost") {
				t.Fatalf("CompleteAccept = %#v, want lost", got.out)
			}
			if actionExists(t, s, "wf1") || actionMessageExists(t, s, "wf1", "om_shared") ||
				countAudit(t, s, "e1") != 0 || mustGetInbox(t, s, "e1").State != "received" {
				t.Fatalf("%s committed after inbox lease expired", tc.name)
			}
		})
	}
}

func TestPGMessageCompletionRechecksLeaseAfterLockWait(t *testing.T) {
	cases := []struct {
		name string
		call func(*PGStore, MessageClaim) (bool, error)
	}{
		{name: "CompleteMessageRender", call: func(s *PGStore, c MessageClaim) (bool, error) {
			return s.CompleteMessageRender(ctx, c, "renderer")
		}},
		{name: "DeferMessageRender", call: func(s *PGStore, c MessageClaim) (bool, error) {
			return s.DeferMessageRender(ctx, c, "renderer", time.Minute, "timeout")
		}},
		{name: "AbandonMessageRender", call: func(s *PGStore, c MessageClaim) (bool, error) {
			return s.AbandonMessageRender(ctx, c, "renderer", "gone")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestPG(t)
			seedAcceptedRetry(t, s, "wf1", "tokA")
			claim, err := s.ClaimMessage(ctx, "renderer", 120*time.Second)
			if err != nil || claim == nil {
				t.Fatalf("ClaimMessage: (%#v, %v)", claim, err)
			}

			blocker, err := s.DB.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin message blocker: %v", err)
			}
			defer func() { _ = blocker.Rollback() }()
			var blockerPID int
			var expires time.Time
			if err := blocker.QueryRowContext(ctx, `
				UPDATE card_action_messages
				   SET lease_expires_at=clock_timestamp()+interval '250 milliseconds'
				 WHERE workflow_id='wf1' AND open_message_id='om_shared'
				RETURNING pg_backend_pid(), lease_expires_at`).Scan(
				&blockerPID, &expires,
			); err != nil {
				t.Fatalf("shorten and lock message lease: %v", err)
			}

			type renderResult struct {
				ok  bool
				err error
			}
			result := make(chan renderResult, 1)
			go func() {
				ok, err := tc.call(s, *claim)
				result <- renderResult{ok: ok, err: err}
			}()
			waitForBlockedCardActionQuery(t, s, blockerPID)
			sleepPast(t, expires)
			if err := blocker.Commit(); err != nil {
				t.Fatalf("release message lock: %v", err)
			}

			got := <-result
			if got.err != nil || got.ok {
				t.Fatalf("%s after lease expiry = (%v, %v), want false", tc.name, got.ok, got.err)
			}
			message := mustGetActionMessage(t, s, "wf1", "om_shared")
			if message.UpdateState != "pending" || message.RenderedRevision != 0 ||
				message.ReconcileAfter != nil || message.LastError != "" ||
				message.Owner != "renderer" || message.Attempts != 1 {
				t.Fatalf("expired %s changed message: %#v", tc.name, message)
			}
		})
	}
}

func waitForBlockedWorkflowQuery(
	t *testing.T,
	s *PGStore,
	blockerPID int,
	result <-chan asyncCompletionResult,
) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case got := <-result:
			t.Fatalf("completion bypassed workflow lock: (%#v, %v)", got.out, got.err)
		default:
		}
		var waiting int
		if err := s.DB.QueryRowContext(ctx, `
			SELECT count(*)
			  FROM pg_stat_activity
			 WHERE datname=current_database()
			   AND pid <> $1
			   AND wait_event_type='Lock'
			   AND query LIKE '%workflow_runs%'`, blockerPID).Scan(&waiting); err != nil {
			t.Fatalf("inspect blocked workflow query: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("completion did not wait on the workflow_runs row")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPGConcurrentStaleClaimsAreExclusive(t *testing.T) {
	type claimResult struct {
		id  string
		err error
	}
	run := func(t *testing.T, claim func(string) (string, error)) {
		t.Helper()
		start := make(chan struct{})
		results := make(chan claimResult, 2)
		var wg sync.WaitGroup
		for _, token := range []string{"sweeper-a", "sweeper-b"} {
			token := token
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				id, err := claim(token)
				results <- claimResult{id: id, err: err}
			}()
		}
		close(start)
		wg.Wait()
		close(results)

		claimed := 0
		for result := range results {
			if result.err != nil {
				t.Fatalf("concurrent claim: %v", result.err)
			}
			if result.id != "" {
				claimed++
			}
		}
		if claimed != 1 {
			t.Fatalf("claimed=%d, want exactly one", claimed)
		}
	}

	t.Run("Message", func(t *testing.T) {
		s := openTestPG(t)
		seedAcceptedRetry(t, s, "wf1", "tokA")
		run(t, func(token string) (string, error) {
			claim, err := s.ClaimMessage(ctx, token, 120*time.Second)
			if claim == nil {
				return "", err
			}
			return claim.WorkflowID + "/" + claim.OpenMessageID, err
		})
	})

	t.Run("Action", func(t *testing.T) {
		s := openTestPG(t)
		seedAcceptedRetry(t, s, "wf1", "tokA")
		expireActionLease(t, s, "wf1")
		run(t, func(token string) (string, error) {
			claim, err := s.ClaimStaleAction(ctx, token, 120*time.Second)
			if claim == nil {
				return "", err
			}
			return claim.WorkflowID, err
		})
	})

	t.Run("Inbox", func(t *testing.T) {
		s := openTestPG(t)
		if _, inserted, err := s.PutInbox(ctx, InboxRow{
			EventID: "e1", Disposition: "accepted", AckToast: "ack",
			Action: "retry", WorkflowID: "wf1", ActorOpenID: "ou_x",
			OpenMessageID: "om_1", State: "received",
		}, nil); err != nil || !inserted {
			t.Fatalf("PutInbox = (%v, %v)", inserted, err)
		}
		run(t, func(token string) (string, error) {
			claim, err := s.ClaimStaleInbox(ctx, token, 120*time.Second)
			if claim == nil {
				return "", err
			}
			return claim.EventID, err
		})
	})
}

func TestPGMessageClaimActionSnapshotMatchesDesiredRevisionDuringFinalize(t *testing.T) {
	s := openTestPG(t)
	for i := 0; i < 12; i++ {
		workflowID := fmt.Sprintf("wf-race-%02d", i)
		seedAcceptedRetry(t, s, workflowID, "tokA")

		start := make(chan struct{})
		type claimResult struct {
			claim *MessageClaim
			err   error
		}
		claimed := make(chan claimResult, 1)
		finalized := make(chan error, 1)
		go func() {
			<-start
			claim, err := s.ClaimMessage(ctx, "renderer-"+workflowID, 120*time.Second)
			claimed <- claimResult{claim: claim, err: err}
		}()
		go func() {
			<-start
			ok, err := s.FinalizeAction(ctx, workflowID, "tokA", "succeeded", "")
			if err == nil && !ok {
				err = errors.New("FinalizeAction lost live action claim")
			}
			finalized <- err
		}()
		close(start)

		got := <-claimed
		finalizeErr := <-finalized
		claimToken := "renderer-" + workflowID
		if got.err == nil && got.claim == nil {
			claimToken = "renderer-retry-" + workflowID
			got.claim, got.err = s.ClaimMessage(
				ctx, claimToken, 120*time.Second,
			)
		}
		if got.err != nil || got.claim == nil || got.claim.Action == nil {
			t.Fatalf("iteration %d ClaimMessage = (%#v, %v)", i, got.claim, got.err)
		}
		if got.claim.WorkflowID != workflowID ||
			got.claim.Action.Revision != got.claim.DesiredRevision {
			t.Fatalf("iteration %d observed mixed revisions: %#v", i, got.claim)
		}
		switch got.claim.DesiredRevision {
		case 1:
			if got.claim.Action.State != "pending" {
				t.Fatalf("revision 1 snapshot state=%q, want pending", got.claim.Action.State)
			}
		case 2:
			if got.claim.Action.State != "succeeded" {
				t.Fatalf("revision 2 snapshot state=%q, want succeeded", got.claim.Action.State)
			}
		default:
			t.Fatalf("unexpected claim revision %#v", got.claim)
		}
		if finalizeErr != nil {
			t.Fatalf("iteration %d finalize: %v", i, finalizeErr)
		}
		if got.claim.DesiredRevision == 1 {
			if ok, err := s.CompleteMessageRender(ctx, *got.claim, claimToken); err != nil || ok {
				t.Fatalf("iteration %d stale rev1 completion = (%v, %v)", i, ok, err)
			}
			claimToken = "renderer-final-" + workflowID
			got.claim, got.err = s.ClaimMessage(ctx, claimToken, 120*time.Second)
			if got.err != nil || got.claim == nil || got.claim.WorkflowID != workflowID ||
				got.claim.DesiredRevision != 2 || got.claim.Action == nil ||
				got.claim.Action.Revision != 2 {
				t.Fatalf("iteration %d final rev2 claim = (%#v, %v)", i, got.claim, got.err)
			}
		}
		if ok, err := s.CompleteMessageRender(ctx, *got.claim, claimToken); err != nil || !ok {
			t.Fatalf("iteration %d final completion = (%v, %v)", i, ok, err)
		}
	}
}

func TestPGConcurrentRetryAndIgnoreHaveSingleWinner(t *testing.T) {
	s := openTestPG(t)
	seedRunAndArtifacts(t, s, "wf1", "grp/p", "abcd1234", 42, "v1")
	retryToken := claimForTestMessage(t, s, "retry", "wf1", "retry", "om_retry")
	ignoreToken := claimForTestMessage(t, s, "ignore", "wf1", "ignore", "om_ignore")

	requests := []AcceptRequest{
		acceptReqMessage("retry", retryToken, "wf1", "retry", "om_retry"),
		acceptReqMessage("ignore", ignoreToken, "wf1", "ignore", "om_ignore"),
	}
	type acceptResult struct {
		out *AcceptOutcome
		err error
	}
	results := make(chan acceptResult, len(requests))
	var wg sync.WaitGroup
	for _, request := range requests {
		request := request
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := s.CompleteAccept(ctx, request)
			results <- acceptResult{out: out, err: err}
		}()
	}
	wg.Wait()
	close(results)

	accepted, conflicts := 0, 0
	for result := range results {
		if result.err != nil || result.out == nil {
			t.Fatalf("concurrent accept = (%#v, %v)", result.out, result.err)
		}
		switch result.out.Kind {
		case "accepted":
			accepted++
		case "conflict":
			conflicts++
		default:
			t.Fatalf("unexpected outcome %#v", result.out)
		}
	}
	if accepted != 1 || conflicts != 1 {
		t.Fatalf("accepted=%d conflicts=%d, want 1/1", accepted, conflicts)
	}

	action := mustGetAction(t, s, "wf1")
	switch action.Action {
	case "retry":
		if action.State != "pending" || artifactAttempt(t, s, "grp/p") != 1 {
			t.Fatalf("retry winner = %#v", action)
		}
	case "ignore":
		if action.State != "succeeded" || artifactAttempt(t, s, "grp/p") != 0 {
			t.Fatalf("ignore winner = %#v", action)
		}
	default:
		t.Fatalf("unexpected winning action %#v", action)
	}
}

func TestPGCompleteRejectSerializesWithActionAcceptance(t *testing.T) {
	s := openTestPG(t)
	seedRunAndArtifacts(t, s, "wf1", "grp/p", "abcd1234", 42, "v1")
	token := claimForTestMessage(t, s, "rejected", "wf1", "retry", "om_rejected")

	acceptance, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin acceptance transaction: %v", err)
	}
	defer func() { _ = acceptance.Rollback() }()
	var blockerPID int
	if err := acceptance.QueryRowContext(ctx, `
		SELECT pg_backend_pid()
		  FROM workflow_runs
		 WHERE workflow_id='wf1'
		 FOR UPDATE`).Scan(&blockerPID); err != nil {
		t.Fatalf("lock workflow run: %v", err)
	}
	if _, err := acceptance.ExecContext(ctx, `
		INSERT INTO card_actions
			(workflow_id, action, actor_open_id, state, revision)
		VALUES ('wf1','ignore','ou_winner','succeeded',1)`); err != nil {
		t.Fatalf("insert uncommitted action: %v", err)
	}

	result := make(chan error, 1)
	go func() {
		result <- s.CompleteReject(ctx, "rejected", token, RejectRender{
			Code: "StillRunning", RejectionReason: "still running",
		})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-result:
			t.Fatalf("CompleteReject bypassed workflow serialization: %v", err)
		default:
		}
		var waiting int
		if err := s.DB.QueryRowContext(ctx, `
			SELECT count(*)
			  FROM pg_stat_activity
			 WHERE datname=current_database()
			   AND pid <> $1
			   AND wait_event_type='Lock'
			   AND query LIKE '%workflow_runs%'`, blockerPID).Scan(&waiting); err != nil {
			t.Fatalf("inspect blocked rejection: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("CompleteReject did not wait on the workflow_runs row")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := acceptance.Commit(); err != nil {
		t.Fatalf("commit acceptance transaction: %v", err)
	}
	if err := <-result; err != nil {
		t.Fatalf("CompleteReject: %v", err)
	}
	message := mustGetActionMessage(t, s, "wf1", "om_rejected")
	if message.RenderKind != "action" || message.RejectionReason != "" ||
		message.ButtonsMode != "none" || message.DesiredRevision != 1 {
		t.Fatalf("rejection did not converge to winning action: %#v", message)
	}
}

func waitForBlockedCardActionQuery(t *testing.T, s *PGStore, blockerPID int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		err := s.DB.QueryRowContext(ctx, `
			SELECT count(*)
			  FROM pg_stat_activity
			 WHERE datname=current_database()
			   AND pid <> $1
			   AND wait_event_type='Lock'
			   AND query LIKE '%card_action%'`, blockerPID).Scan(&waiting)
		if err != nil {
			t.Fatalf("inspect blocked card action query: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("card action query did not block on the expected row lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func shortenInboxLease(t *testing.T, s *PGStore, eventID string) time.Time {
	t.Helper()
	var expires time.Time
	if err := s.DB.QueryRowContext(ctx, `
		UPDATE card_action_inbox
		   SET lease_expires_at=clock_timestamp()+interval '250 milliseconds'
		 WHERE event_id=$1
		RETURNING lease_expires_at`, eventID).Scan(&expires); err != nil {
		t.Fatalf("shorten inbox lease: %v", err)
	}
	return expires
}

func lockCardActionRow(t *testing.T, s *PGStore, query string) (*sql.Tx, int) {
	t.Helper()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin row blocker: %v", err)
	}
	var ignored string
	if err := tx.QueryRowContext(ctx, query).Scan(&ignored); err != nil {
		_ = tx.Rollback()
		t.Fatalf("lock card action row: %v", err)
	}
	var pid int
	if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		_ = tx.Rollback()
		t.Fatalf("row blocker backend PID: %v", err)
	}
	return tx, pid
}

func sleepPast(t *testing.T, deadline time.Time) {
	t.Helper()
	if wait := time.Until(deadline.Add(50 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
}

func TestPGRecentRunsUsesOneSnapshot(t *testing.T) {
	s := openTestPG(t)
	artifact := Artifact{
		Project: "snapshot/project", CommitSHA: "abcd1234", PipelineID: 42,
		Variant: "v1", URL: "u", SHA256: "s",
	}
	if err := s.RegisterArtifacts(ctx, []Artifact{artifact}); err != nil {
		t.Fatalf("RegisterArtifacts: %v", err)
	}

	got, err := s.recentRuns(ctx, 1, func() error {
		return s.RecordWorkflowRun(ctx, WorkflowRun{
			WorkflowID: "inserted-between-recent-run-queries",
			Project:    artifact.Project, CommitSHA: artifact.CommitSHA,
			PipelineID: artifact.PipelineID, Version: "1.0.0",
			RuleVersion: "rules-v1", Variants: []string{artifact.Variant},
		})
	})
	if err != nil {
		t.Fatalf("RecentRuns: %v", err)
	}
	if len(got) != 1 || got[0].Authoritative ||
		got[0].Project != artifact.Project || got[0].Variant != artifact.Variant {
		t.Fatalf("runs = %#v, want legacy row from the call's initial snapshot", got)
	}
	if _, err := s.GetWorkflowRun(ctx, "inserted-between-recent-run-queries"); err != nil {
		t.Fatalf("concurrent workflow run was not committed: %v", err)
	}
}

func artifactAttempts(t *testing.T, s fullStore) map[string]int {
	t.Helper()
	out := map[string]int{}
	switch st := s.(type) {
	case *MemStore:
		for _, row := range st.Artifacts() {
			if row.CommitSHA == "abcd1234" && row.PipelineID == 42 && row.Variant == "v1" {
				out[row.Project] = row.WorkflowAttempt
			}
		}
	case *PGStore:
		rows, err := st.DB.QueryContext(ctx, `
			SELECT project, workflow_attempt FROM artifacts
			WHERE commit_sha = 'abcd1234' AND pipeline_id = 42 AND variant = 'v1'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var project string
			var attempt int
			if err := rows.Scan(&project, &attempt); err != nil {
				t.Fatal(err)
			}
			out[project] = attempt
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unsupported store type %T", s)
	}
	return out
}

// mustGetInbox 是 s.GetInbox 的测试便捷包装:找不到即 Fatal,省得每个用例
// 重复写错误处理。
func mustGetInbox(t *testing.T, s fullStore, eventID string) *InboxRow {
	t.Helper()
	row, err := s.GetInbox(ctx, eventID)
	if err != nil {
		t.Fatalf("GetInbox(%s): %v", eventID, err)
	}
	return row
}

// countAudit 统计 audit_log 中 inbox_event_id = eventID 的行数,用来断言
// "一个 inbox 事件恰好一条审计"(§3.5)。audit_log 目前只服务测试可见性,
// 没有生产 API,所以像 artifactAttempts 一样按具体类型直接查底层存储。
func countAudit(t *testing.T, s fullStore, eventID string) int {
	t.Helper()
	switch st := s.(type) {
	case *MemStore:
		st.mu.Lock()
		defer st.mu.Unlock()
		n := 0
		for _, a := range st.auditLog {
			if a.InboxEventID == eventID {
				n++
			}
		}
		return n
	case *PGStore:
		var n int
		if err := st.DB.QueryRowContext(ctx,
			`SELECT count(*) FROM audit_log WHERE inbox_event_id = $1`, eventID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	default:
		t.Fatalf("unsupported store type %T", s)
		return 0
	}
}

func seedRunAndArtifacts(
	t *testing.T, s fullStore, workflowID, project, commitSHA string, pipelineID int, variants ...string,
) {
	t.Helper()
	if err := s.RecordWorkflowRun(ctx, WorkflowRun{
		WorkflowID: workflowID, Project: project, CommitSHA: commitSHA, PipelineID: pipelineID,
		Version: "1.2.3", RuleVersion: "rules-v1", Variants: variants,
	}); err != nil {
		t.Fatalf("RecordWorkflowRun: %v", err)
	}
	arts := make([]Artifact, 0, len(variants))
	for _, variant := range variants {
		arts = append(arts, Artifact{
			Project: project, CommitSHA: commitSHA, PipelineID: pipelineID, Variant: variant,
			BuildType: "Release", URL: "https://registry/" + variant, SHA256: "sha-" + variant,
			Size: 1, ManifestDigest: "manifest-" + variant,
		})
	}
	if err := s.RegisterArtifacts(ctx, arts); err != nil {
		t.Fatalf("RegisterArtifacts: %v", err)
	}
}

func claimForTest(t *testing.T, s fullStore, eventID, workflowID, action string) string {
	return claimForTestMessage(t, s, eventID, workflowID, action, "om_shared")
}

func claimForTestMessage(
	t *testing.T, s fullStore, eventID, workflowID, action, openMessageID string,
) string {
	t.Helper()
	token := "inbox-" + eventID
	_, inserted, err := s.PutInbox(ctx, InboxRow{
		EventID: eventID, Disposition: "accepted", AckToast: "已收到，正在处理",
		Action: action, WorkflowID: workflowID, ActorOpenID: "ou_x",
		OpenMessageID: openMessageID, PayloadDigest: "digest-" + eventID, State: "received",
	}, nil)
	if err != nil || !inserted {
		t.Fatalf("PutInbox(%s) = (%v, %v)", eventID, inserted, err)
	}
	if _, err := s.ClaimInbox(ctx, eventID, token, 120*time.Second); err != nil {
		t.Fatalf("ClaimInbox(%s): %v", eventID, err)
	}
	return token
}

func acceptReq(eventID, token, workflowID, action string) AcceptRequest {
	return acceptReqMessage(eventID, token, workflowID, action, "om_shared")
}

func acceptReqMessage(eventID, token, workflowID, action, openMessageID string) AcceptRequest {
	req := AcceptRequest{
		EventID: eventID, Token: token, WorkflowID: workflowID, Action: action,
		ActorOpenID: "ou_x", OpenMessageID: openMessageID, PayloadDigest: "digest-" + eventID,
		ActionToken: "action-" + eventID, Project: "grp/p", CommitSHA: "abcd1234", PipelineID: 42,
	}
	if action == "retry" {
		req.BuildTarget = func(attempt int) ([]byte, string, error) {
			in := wf.DeviceTestInput{
				Project: "grp/p", Commit: "abcd1234", PipelineID: 42,
				Version: "1.2.3", RuleVersion: "rules-v1",
				Packages: []wf.PackageRef{{
					Variant: "v1", PackageFile: "v1.tar.gz", URL: "https://registry/v1",
					SHA256: "sha-v1", Size: 1, ManifestDigest: "manifest-v1",
				}},
				Scope: "v1", Attempt: attempt, SourceWorkflowID: workflowID,
			}
			raw, err := json.Marshal(in)
			return raw, in.WorkflowID(), err
		}
	}
	return req
}

func mustGetAction(t *testing.T, s fullStore, workflowID string) CardAction {
	t.Helper()
	switch st := s.(type) {
	case *MemStore:
		st.mu.Lock()
		defer st.mu.Unlock()
		row, ok := st.cardActions[workflowID]
		if !ok {
			t.Fatalf("card action %s not found", workflowID)
		}
		row.TargetInput = append([]byte(nil), row.TargetInput...)
		return row
	case *PGStore:
		var row CardAction
		var lease sql.NullTime
		var target []byte
		if err := st.DB.QueryRowContext(ctx, `
			SELECT workflow_id, action, actor_open_id, state, owner, lease_expires_at,
			       target_workflow_id, attempt, target_input, last_error, revision
			  FROM card_actions WHERE workflow_id=$1`, workflowID).Scan(
			&row.WorkflowID, &row.Action, &row.ActorOpenID, &row.State, &row.Owner, &lease,
			&row.TargetWorkflowID, &row.Attempt, &target, &row.LastError, &row.Revision,
		); err != nil {
			t.Fatalf("get card action %s: %v", workflowID, err)
		}
		if lease.Valid {
			expires := lease.Time.UTC()
			row.LeaseExpiresAt = &expires
		}
		row.TargetInput = append([]byte(nil), target...)
		return row
	default:
		t.Fatalf("unsupported store type %T", s)
		return CardAction{}
	}
}

func actionExists(t *testing.T, s fullStore, workflowID string) bool {
	t.Helper()
	switch st := s.(type) {
	case *MemStore:
		st.mu.Lock()
		defer st.mu.Unlock()
		_, ok := st.cardActions[workflowID]
		return ok
	case *PGStore:
		var exists bool
		if err := st.DB.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM card_actions WHERE workflow_id=$1)`, workflowID).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		return exists
	default:
		t.Fatalf("unsupported store type %T", s)
		return false
	}
}

type actionMessageState struct {
	RenderKind, RejectionReason, ButtonsMode, UpdateState, Owner string
	LastError                                                    string
	DesiredRevision, RenderedRevision, Attempts                  int
	LeaseExpiresAt, ReconcileAfter                               *time.Time
}

func mustGetActionMessage(t *testing.T, s fullStore, workflowID, openMessageID string) actionMessageState {
	t.Helper()
	switch st := s.(type) {
	case *MemStore:
		st.mu.Lock()
		defer st.mu.Unlock()
		row, ok := st.cardActionMessages[workflowID+"\x00"+openMessageID]
		if !ok {
			t.Fatalf("card action message %s/%s not found", workflowID, openMessageID)
		}
		return actionMessageState{
			RenderKind: row.RenderKind, RejectionReason: row.RejectionReason,
			ButtonsMode: row.ButtonsMode, UpdateState: row.UpdateState, Owner: row.Owner,
			LastError: row.LastError, DesiredRevision: row.DesiredRevision,
			RenderedRevision: row.RenderedRevision, Attempts: row.Attempts,
			LeaseExpiresAt: row.LeaseExpiresAt, ReconcileAfter: row.ReconcileAfter,
		}
	case *PGStore:
		var row actionMessageState
		var lease, reconcile sql.NullTime
		if err := st.DB.QueryRowContext(ctx, `
			SELECT render_kind, rejection_reason, buttons_mode, desired_revision,
			       rendered_revision, update_state, owner, lease_expires_at,
			       reconcile_after, attempts, last_error
			  FROM card_action_messages
			 WHERE workflow_id=$1 AND open_message_id=$2`, workflowID, openMessageID).Scan(
			&row.RenderKind, &row.RejectionReason, &row.ButtonsMode,
			&row.DesiredRevision, &row.RenderedRevision, &row.UpdateState,
			&row.Owner, &lease, &reconcile, &row.Attempts, &row.LastError,
		); err != nil {
			t.Fatalf("get card action message %s/%s: %v", workflowID, openMessageID, err)
		}
		if lease.Valid {
			v := lease.Time.UTC()
			row.LeaseExpiresAt = &v
		}
		if reconcile.Valid {
			v := reconcile.Time.UTC()
			row.ReconcileAfter = &v
		}
		return row
	default:
		t.Fatalf("unsupported store type %T", s)
		return actionMessageState{}
	}
}

func actionMessageExists(t *testing.T, s fullStore, workflowID, openMessageID string) bool {
	t.Helper()
	switch st := s.(type) {
	case *MemStore:
		st.mu.Lock()
		defer st.mu.Unlock()
		_, ok := st.cardActionMessages[workflowID+"\x00"+openMessageID]
		return ok
	case *PGStore:
		var exists bool
		if err := st.DB.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM card_action_messages
				 WHERE workflow_id=$1 AND open_message_id=$2)`,
			workflowID, openMessageID).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		return exists
	default:
		t.Fatalf("unsupported store type %T", s)
		return false
	}
}

func countAuditByAction(t *testing.T, s fullStore, action string) int {
	t.Helper()
	switch st := s.(type) {
	case *MemStore:
		st.mu.Lock()
		defer st.mu.Unlock()
		n := 0
		for _, row := range st.auditLog {
			if row.Action == action {
				n++
			}
		}
		return n
	case *PGStore:
		var n int
		if err := st.DB.QueryRowContext(ctx,
			`SELECT count(*) FROM audit_log WHERE action=$1`, action).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	default:
		t.Fatalf("unsupported store type %T", s)
		return 0
	}
}

func artifactAttempt(t *testing.T, s fullStore, project string) int {
	t.Helper()
	attempts := artifactAttempts(t, s)
	return attempts[project]
}

func expireInboxLease(t *testing.T, s fullStore, eventID string) {
	t.Helper()
	past := time.Now().UTC().Add(-time.Minute)
	switch st := s.(type) {
	case *MemStore:
		st.mu.Lock()
		row := st.inbox[eventID]
		row.LeaseExpiresAt = &past
		st.inbox[eventID] = row
		st.mu.Unlock()
	case *PGStore:
		if _, err := st.DB.ExecContext(ctx,
			`UPDATE card_action_inbox SET lease_expires_at=$2 WHERE event_id=$1`, eventID, past); err != nil {
			t.Fatal(err)
		}
	}
}

func expireActionLease(t *testing.T, s fullStore, workflowID string) {
	t.Helper()
	past := time.Now().UTC().Add(-time.Minute)
	switch st := s.(type) {
	case *MemStore:
		st.mu.Lock()
		row := st.cardActions[workflowID]
		row.LeaseExpiresAt = &past
		st.cardActions[workflowID] = row
		st.mu.Unlock()
	case *PGStore:
		if _, err := st.DB.ExecContext(ctx,
			`UPDATE card_actions SET lease_expires_at=$2 WHERE workflow_id=$1`, workflowID, past); err != nil {
			t.Fatal(err)
		}
	}
}

func clearActionLease(t *testing.T, s fullStore, workflowID string) {
	t.Helper()
	switch st := s.(type) {
	case *MemStore:
		st.mu.Lock()
		row := st.cardActions[workflowID]
		row.Owner = ""
		row.LeaseExpiresAt = nil
		st.cardActions[workflowID] = row
		st.mu.Unlock()
	case *PGStore:
		if _, err := st.DB.ExecContext(ctx, `
			UPDATE card_actions
			   SET owner='', lease_expires_at=NULL
			 WHERE workflow_id=$1`, workflowID); err != nil {
			t.Fatal(err)
		}
	}
}

func expireMessageLease(t *testing.T, s fullStore, workflowID, openMessageID string) {
	t.Helper()
	past := time.Now().UTC().Add(-time.Minute)
	switch st := s.(type) {
	case *MemStore:
		st.mu.Lock()
		key := messageKey(workflowID, openMessageID)
		row := st.cardActionMessages[key]
		row.LeaseExpiresAt = &past
		st.cardActionMessages[key] = row
		st.mu.Unlock()
	case *PGStore:
		if _, err := st.DB.ExecContext(ctx, `
			UPDATE card_action_messages
			   SET lease_expires_at=$3
			 WHERE workflow_id=$1 AND open_message_id=$2`,
			workflowID, openMessageID, past); err != nil {
			t.Fatal(err)
		}
	}
}

func makeMessageReconcileDue(t *testing.T, s fullStore, workflowID, openMessageID string) {
	t.Helper()
	past := time.Now().UTC().Add(-time.Minute)
	switch st := s.(type) {
	case *MemStore:
		st.mu.Lock()
		key := messageKey(workflowID, openMessageID)
		row := st.cardActionMessages[key]
		row.ReconcileAfter = &past
		st.cardActionMessages[key] = row
		st.mu.Unlock()
	case *PGStore:
		if _, err := st.DB.ExecContext(ctx, `
			UPDATE card_action_messages
			   SET reconcile_after=$3
			 WHERE workflow_id=$1 AND open_message_id=$2`,
			workflowID, openMessageID, past); err != nil {
			t.Fatal(err)
		}
	}
}

func seedAcceptedRetry(t *testing.T, s fullStore, workflowID, actionToken string) {
	t.Helper()
	seedRunAndArtifacts(t, s, workflowID, "grp/p", "abcd1234", 42, "v1")
	token := claimForTest(t, s, "seed-"+workflowID, workflowID, "retry")
	req := acceptReq("seed-"+workflowID, token, workflowID, "retry")
	req.ActionToken = actionToken
	out, err := s.CompleteAccept(ctx, req)
	if err != nil || out.Kind != "accepted" {
		t.Fatalf("seed CompleteAccept = (%#v, %v)", out, err)
	}
}

func setMessageBusyForTest(t *testing.T, s fullStore, workflowID, openMessageID string) {
	t.Helper()
	future := time.Now().UTC().Add(time.Minute)
	reconcile := future.Add(time.Minute)
	switch st := s.(type) {
	case *MemStore:
		st.mu.Lock()
		key := workflowID + "\x00" + openMessageID
		row := st.cardActionMessages[key]
		row.Owner = "renderer"
		row.LeaseExpiresAt = &future
		row.ReconcileAfter = &reconcile
		row.UpdateState = "succeeded"
		st.cardActionMessages[key] = row
		st.mu.Unlock()
	case *PGStore:
		if _, err := st.DB.ExecContext(ctx, `
			UPDATE card_action_messages
			   SET owner='renderer', lease_expires_at=$3, reconcile_after=$4, update_state='succeeded'
			 WHERE workflow_id=$1 AND open_message_id=$2`,
			workflowID, openMessageID, future, reconcile); err != nil {
			t.Fatal(err)
		}
	}
}

// runConformance 对一个空 store 实例跑全部行为断言;
// newStore 每个子测试调用一次,必须返回干净实例。
func runConformance(t *testing.T, newStore func(t *testing.T) fullStore) {
	seed := func(t *testing.T, s fullStore) {
		t.Helper()
		err := s.UpsertClientDevices(ctx,
			Client{ClientID: "c1", Host: "SH-D-007631A", Version: "0.1.0", BaseURL: "https://client:8443"},
			[]Device{
				{DeviceID: "513cd3de", Serial: "513cd3de", ClientID: "c1", SOC: "trinket",
					ABI: "arm64-v8a", Capabilities: []string{"hexagon"}},
			})
		if err != nil {
			t.Fatal(err)
		}
	}

	workflowRunBase := func() WorkflowRun {
		return WorkflowRun{
			WorkflowID: "device-test-grp/p-gabcd1234-p42",
			Project:    "grp/p", CommitSHA: "abcd1234", PipelineID: 42,
			Version: "1.2.3", RuleVersion: "verdict-rules-v1",
			Variants: []string{"v2", "", "v1", "v2"},
		}
	}

	t.Run("WorkflowRunRecordGetCanonical", func(t *testing.T) {
		s := newStore(t)
		run := workflowRunBase()
		if err := s.RecordWorkflowRun(ctx, run); err != nil {
			t.Fatalf("RecordWorkflowRun: %v", err)
		}
		got, err := s.GetWorkflowRun(ctx, run.WorkflowID)
		if err != nil {
			t.Fatalf("GetWorkflowRun: %v", err)
		}
		if got == nil {
			t.Fatal("GetWorkflowRun returned nil")
		}
		want := run
		want.Variants = []string{"v1", "v2"}
		want.CreatedAt = got.CreatedAt
		if !reflect.DeepEqual(*got, want) {
			t.Fatalf("run = %#v, want %#v", *got, want)
		}
		if got.CreatedAt.IsZero() {
			t.Fatal("CreatedAt must be populated")
		}
		if _, err := s.GetWorkflowRun(ctx, "missing"); !errors.Is(err, ErrWorkflowRunNotFound) {
			t.Fatalf("missing error = %v, want ErrWorkflowRunNotFound", err)
		}
	})

	t.Run("WorkflowRunIdempotent", func(t *testing.T) {
		s := newStore(t)
		run := workflowRunBase()
		if err := s.RecordWorkflowRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		first, err := s.GetWorkflowRun(ctx, run.WorkflowID)
		if err != nil {
			t.Fatal(err)
		}
		replay := run
		replay.Variants = []string{"v2", "v1", "v1"}
		replay.CreatedAt = first.CreatedAt.Add(24 * time.Hour)
		if err := s.RecordWorkflowRun(ctx, replay); err != nil {
			t.Fatalf("canonical replay: %v", err)
		}
		got, err := s.GetWorkflowRun(ctx, run.WorkflowID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("idempotent replay changed row: got %#v, first %#v", got, first)
		}

		empty := workflowRunBase()
		empty.WorkflowID = "empty-variants"
		empty.Variants = nil
		if err := s.RecordWorkflowRun(ctx, empty); err != nil {
			t.Fatalf("record empty variants: %v", err)
		}
		if err := s.RecordWorkflowRun(ctx, empty); err != nil {
			t.Fatalf("replay empty variants: %v", err)
		}
	})

	t.Run("WorkflowRunConflictEveryField", func(t *testing.T) {
		s := newStore(t)
		parentA := workflowRunBase()
		parentA.WorkflowID = "parent-a"
		parentA.Variants = []string{"parent"}
		parentB := parentA
		parentB.WorkflowID = "parent-b"
		for _, parent := range []WorkflowRun{parentA, parentB} {
			if err := s.RecordWorkflowRun(ctx, parent); err != nil {
				t.Fatalf("record %s: %v", parent.WorkflowID, err)
			}
		}
		base := workflowRunBase()
		base.SourceWorkflowID = parentA.WorkflowID
		if err := s.RecordWorkflowRun(ctx, base); err != nil {
			t.Fatalf("record base: %v", err)
		}
		cases := []struct {
			name   string
			change func(*WorkflowRun)
		}{
			{"Project", func(r *WorkflowRun) { r.Project = "grp/other" }},
			{"CommitSHA", func(r *WorkflowRun) { r.CommitSHA = "deadbeef" }},
			{"PipelineID", func(r *WorkflowRun) { r.PipelineID++ }},
			{"Version", func(r *WorkflowRun) { r.Version = "2.0.0" }},
			{"RuleVersion", func(r *WorkflowRun) { r.RuleVersion = "rules-v2" }},
			{"Scope", func(r *WorkflowRun) { r.Scope = "nightly" }},
			{"Attempt", func(r *WorkflowRun) { r.Attempt++ }},
			{"Variants", func(r *WorkflowRun) { r.Variants = []string{"v1", "v3"} }},
			{"SourceWorkflowID", func(r *WorkflowRun) { r.SourceWorkflowID = parentB.WorkflowID }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				changed := base
				changed.Variants = append([]string(nil), base.Variants...)
				tc.change(&changed)
				if err := s.RecordWorkflowRun(ctx, changed); !errors.Is(err, ErrWorkflowRunConflict) {
					t.Fatalf("error = %v, want ErrWorkflowRunConflict", err)
				}
			})
		}
	})

	t.Run("WorkflowRunRejectsMissingSource", func(t *testing.T) {
		s := newStore(t)
		run := workflowRunBase()
		run.SourceWorkflowID = "does-not-exist"
		if err := s.RecordWorkflowRun(ctx, run); !errors.Is(err, ErrWorkflowRunPermanent) {
			t.Fatalf("missing source error = %v, want ErrWorkflowRunPermanent", err)
		}
		run.SourceWorkflowID = run.WorkflowID
		if err := s.RecordWorkflowRun(ctx, run); !errors.Is(err, ErrWorkflowRunPermanent) {
			t.Fatalf("self source error = %v, want ErrWorkflowRunPermanent", err)
		}

		invalid := []struct {
			name   string
			change func(*WorkflowRun)
		}{
			{"WorkflowID", func(r *WorkflowRun) { r.WorkflowID = "" }},
			{"Project", func(r *WorkflowRun) { r.Project = "" }},
			{"CommitSHA", func(r *WorkflowRun) { r.CommitSHA = "" }},
			{"PipelineID", func(r *WorkflowRun) { r.PipelineID = 0 }},
			{"Version", func(r *WorkflowRun) { r.Version = "" }},
			{"RuleVersion", func(r *WorkflowRun) { r.RuleVersion = "" }},
			{"Attempt", func(r *WorkflowRun) { r.Attempt = -1 }},
		}
		for _, tc := range invalid {
			t.Run(tc.name, func(t *testing.T) {
				bad := workflowRunBase()
				tc.change(&bad)
				if err := s.RecordWorkflowRun(ctx, bad); !errors.Is(err, ErrWorkflowRunPermanent) {
					t.Fatalf("error = %v, want ErrWorkflowRunPermanent", err)
				}
			})
		}
	})

	t.Run("WorkflowRunDefensiveCopy", func(t *testing.T) {
		s := newStore(t)
		run := workflowRunBase()
		callerVariants := append([]string(nil), run.Variants...)
		if err := s.RecordWorkflowRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(run.Variants, callerVariants) {
			t.Fatalf("caller variants mutated: got %v, want %v", run.Variants, callerVariants)
		}
		first, err := s.GetWorkflowRun(ctx, run.WorkflowID)
		if err != nil {
			t.Fatal(err)
		}
		first.Variants[0] = "mutated"
		second, err := s.GetWorkflowRun(ctx, run.WorkflowID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(second.Variants, []string{"v1", "v2"}) {
			t.Fatalf("stored variants mutated through result: %v", second.Variants)
		}
	})

	t.Run("HasCapableDeviceIgnoresStatus", func(t *testing.T) {
		s := newStore(t)
		// 空 fleet:任何 selector 都无匹配
		if ok, err := s.HasCapableDevice(ctx, wf.DeviceSelector{}); err != nil || ok {
			t.Fatalf("empty fleet: ok=%v err=%v, want false", ok, err)
		}
		seed(t, s)
		// 大小写不敏感 + capabilities 子集,与 AcquireDevice 同一匹配语义
		if ok, _ := s.HasCapableDevice(ctx, wf.DeviceSelector{SOC: []string{"TRINKET"}, Capabilities: []string{"hexagon"}}); !ok {
			t.Error("trinket+hexagon 应匹配")
		}
		if ok, _ := s.HasCapableDevice(ctx, wf.DeviceSelector{SOC: []string{"RK3588"}}); ok {
			t.Error("RK3588 不应匹配")
		}
		if ok, _ := s.HasCapableDevice(ctx, wf.DeviceSelector{Capabilities: []string{"npu"}}); ok {
			t.Error("npu 不应匹配(capabilities 非子集)")
		}
		// BUSY/QUARANTINED 也算 fleet 有能力("设备在但暂不可用"由 acquire 等待处理)
		l, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t1", 120)
		if err != nil || l == nil {
			t.Fatal(err)
		}
		if ok, _ := s.HasCapableDevice(ctx, wf.DeviceSelector{SOC: []string{"trinket"}}); !ok {
			t.Error("BUSY 设备仍应报告 fleet 有能力")
		}
	})

	t.Run("AcquireMatchesSelectorAndLocks", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		// soc 不匹配 → 无设备
		if l, err := s.AcquireDevice(ctx, wf.DeviceSelector{SOC: []string{"RK3588"}}, "t1", 120); err != nil || l != nil {
			t.Fatalf("lease=%v err=%v, want nil(soc 不匹配)", l, err)
		}
		// capabilities 非子集 → 无设备
		if l, err := s.AcquireDevice(ctx, wf.DeviceSelector{Capabilities: []string{"npu"}}, "t1", 120); err != nil || l != nil {
			t.Fatalf("lease=%v err=%v, want nil(capabilities 不满足)", l, err)
		}
		// 大小写不敏感匹配 + capabilities 子集
		l, err := s.AcquireDevice(ctx, wf.DeviceSelector{SOC: []string{"TRINKET"}, Capabilities: []string{"hexagon"}}, "t1", 120)
		if err != nil || l == nil {
			t.Fatalf("lease=%v err=%v", l, err)
		}
		if l.DeviceID != "513cd3de" || l.Serial != "513cd3de" || l.ClientID != "c1" ||
			l.ClientBaseURL != "https://client:8443" {
			t.Errorf("lease = %+v", l)
		}
		// 已占用 → 二次获取无设备
		if l2, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t2", 120); l2 != nil {
			t.Errorf("BUSY 设备不得重复出租: %+v", l2)
		}
		// 释放后可再获取
		if err := s.ReleaseDevice(ctx, l.DeviceID, "t1", wf.FailScopeOK, 3); err != nil {
			t.Fatal(err)
		}
		if l3, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t3", 120); l3 == nil {
			t.Error("释放后应可再次获取")
		}
	})

	t.Run("UpsertAcceptsNilCapabilities", func(t *testing.T) {
		// 心跳的 props.capabilities 可能整体缺省(JSON 省略字段 → Go nil slice);
		// 没有特殊能力的板子是完全正常的情况,不得导致整条心跳失败。
		s := newStore(t)
		err := s.UpsertClientDevices(ctx,
			Client{ClientID: "c1", BaseURL: "https://client:8443"},
			[]Device{{DeviceID: "d-nilcaps", Serial: "d-nilcaps", ClientID: "c1", SOC: "plain"}})
		if err != nil {
			t.Fatalf("nil capabilities 心跳不应报错: %v", err)
		}
		l, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t1", 120)
		if err != nil || l == nil || l.DeviceID != "d-nilcaps" {
			t.Fatalf("lease=%v err=%v", l, err)
		}
	})

	t.Run("HeartbeatMustNotResetBusyState", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		l, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t1", 120)
		if err != nil || l == nil {
			t.Fatalf("lease=%v err=%v", l, err)
		}
		seed(t, s) // 心跳重注册:只刷新属性,不得把 BUSY 刷回 IDLE
		if l2, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t2", 120); l2 != nil {
			t.Errorf("心跳后 BUSY 设备被重新出租: %+v", l2)
		}
	})

	// clients.fail_streak 的姊妹陷阱(差距 #10):心跳只应刷新设备属性,
	// 不得顺带清空 client 级计数器——否则 client 侧的连续失败永远数不到 2。
	t.Run("HeartbeatMustNotResetClientFailStreak", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		l, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t1", 120)
		if err != nil || l == nil {
			t.Fatalf("lease=%v err=%v", l, err)
		}
		if err := s.ReleaseDevice(ctx, l.DeviceID, "t1", wf.FailScopeClient, 3); err != nil {
			t.Fatal(err)
		}
		seed(t, s) // 心跳重注册:只刷新属性,不得把 clients.fail_streak 清零
		ov, err := s.FleetOverview(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ov.Devices[0].ClientFailStreak != 1 {
			t.Errorf("心跳后 client fail_streak = %d, want 1(心跳不得清空该计数器)", ov.Devices[0].ClientFailStreak)
		}
	})

	t.Run("ReleaseIsIdempotentAndOwnerChecked", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		l, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t1", 120)
		if l == nil {
			t.Fatal("no lease")
		}
		// 非持有者释放:无副作用
		if err := s.ReleaseDevice(ctx, l.DeviceID, "other-task", wf.FailScopeDevice, 3); err != nil {
			t.Fatal(err)
		}
		if l2, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t2", 120); l2 != nil {
			t.Fatalf("非持有者释放不得生效: %+v", l2)
		}
		// 持有者释放 + 重复释放幂等
		if err := s.ReleaseDevice(ctx, l.DeviceID, "t1", wf.FailScopeOK, 3); err != nil {
			t.Fatal(err)
		}
		if err := s.ReleaseDevice(ctx, l.DeviceID, "t1", wf.FailScopeDevice, 3); err != nil {
			t.Fatal(err) // 重复释放(infraFail=true)不得计入 fail_streak
		}
		l3, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t3", 120)
		if l3 == nil {
			t.Fatal("释放后应可获取")
		}
	})

	t.Run("QuarantineAfterConsecutiveInfraFailures", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		for i := 0; i < 3; i++ { // §10:连续 3 次 INFRA → QUARANTINED
			l, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t", 120)
			if l == nil {
				t.Fatalf("第 %d 次应能获取", i+1)
			}
			_ = s.ReleaseDevice(ctx, l.DeviceID, "t", wf.FailScopeDevice, 3)
		}
		if l, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t", 120); l != nil {
			t.Error("QUARANTINED 设备不得出租")
		}
		seed(t, s) // 心跳不得解除隔离(§11 devices.status 语义)
		if l, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t", 120); l != nil {
			t.Error("心跳后 QUARANTINED 设备被出租")
		}
	})

	t.Run("SuccessResetsFailStreak", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		for i := 0; i < 2; i++ {
			l, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t", 120)
			_ = s.ReleaseDevice(ctx, l.DeviceID, "t", wf.FailScopeDevice, 3)
		}
		l, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t", 120)
		_ = s.ReleaseDevice(ctx, l.DeviceID, "t", wf.FailScopeOK, 3) // 成功:清零
		for i := 0; i < 2; i++ {
			l, _ = s.AcquireDevice(ctx, wf.DeviceSelector{}, "t", 120)
			if l == nil {
				t.Fatal("fail_streak 清零后 2 次 INFRA 不应隔离")
			}
			_ = s.ReleaseDevice(ctx, l.DeviceID, "t", wf.FailScopeDevice, 3)
		}
	})

	// 归因记账(差距 #10):四个 scope 各记各的账,互不串味。
	//
	// none 与 ok 的关键区别只有在计数器非零时才可观察(0 不动 vs 0 清零看起来
	// 一样),所以每个子用例先用 device/client scope 把两个计数器都垫到 1,
	// 再对种子后的状态跑被测 scope。quarantineAfter 取 5:种子的 1 次 device
	// 释放 + device 用例自身再 1 次 = 2,远低于阈值,不会把设备提前隔离而
	// 搅乱断言。
	t.Run("ReleaseDeviceFailScopes", func(t *testing.T) {
		const quarantineAfter = 5
		cases := []struct {
			name           string
			scope          wf.FailScope
			wantDeviceFail int
			wantClientFail int
			wantStatus     string
		}{
			{"device 只增设备计数", wf.FailScopeDevice, 2, 1, "IDLE"},
			{"client 只增 client 计数", wf.FailScopeClient, 1, 2, "IDLE"},
			{"none 两个都不动", wf.FailScopeNone, 1, 1, "IDLE"},
			{"ok 两个都清零", wf.FailScopeOK, 0, 0, "IDLE"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s := newStore(t)
				seed(t, s)

				// 种子:设备计数、client 计数各垫到 1。
				seedDev, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:seed-device:a1", 120)
				if err != nil || seedDev == nil {
					t.Fatalf("seed device acquire = %+v err=%v", seedDev, err)
				}
				if err := s.ReleaseDevice(ctx, seedDev.DeviceID, "w:seed-device:a1", wf.FailScopeDevice, quarantineAfter); err != nil {
					t.Fatalf("seed device release: %v", err)
				}
				seedClient, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:seed-client:a1", 120)
				if err != nil || seedClient == nil {
					t.Fatalf("seed client acquire = %+v err=%v", seedClient, err)
				}
				if err := s.ReleaseDevice(ctx, seedClient.DeviceID, "w:seed-client:a1", wf.FailScopeClient, quarantineAfter); err != nil {
					t.Fatalf("seed client release: %v", err)
				}

				lease, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", 120)
				if err != nil || lease == nil {
					t.Fatalf("acquire = %+v err=%v", lease, err)
				}
				if err := s.ReleaseDevice(ctx, lease.DeviceID, "w:t1:a1", tc.scope, quarantineAfter); err != nil {
					t.Fatalf("release: %v", err)
				}
				ov, err := s.FleetOverview(ctx)
				if err != nil {
					t.Fatal(err)
				}
				d := ov.Devices[0]
				if d.FailStreak != tc.wantDeviceFail {
					t.Errorf("device fail_streak = %d, want %d", d.FailStreak, tc.wantDeviceFail)
				}
				if d.Status != tc.wantStatus {
					t.Errorf("status = %q, want %q", d.Status, tc.wantStatus)
				}
				if d.ClientFailStreak != tc.wantClientFail {
					t.Errorf("client fail_streak = %d, want %d", d.ClientFailStreak, tc.wantClientFail)
				}
			})
		}
	})

	// 只有 device scope 触发隔离;client/none 累积再多也不隔离设备
	// ——这正是差距 #10 要消灭的误伤。
	t.Run("ReleaseDeviceOnlyDeviceScopeQuarantines", func(t *testing.T) {
		for _, scope := range []wf.FailScope{wf.FailScopeClient, wf.FailScopeNone} {
			t.Run(string(scope), func(t *testing.T) {
				s := newStore(t)
				seed(t, s)
				for i := 1; i <= 5; i++ {
					taskID := fmt.Sprintf("w:t%d:a1", i)
					lease, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, taskID, 120)
					if err != nil || lease == nil {
						t.Fatalf("acquire %d = %+v err=%v", i, lease, err)
					}
					if err := s.ReleaseDevice(ctx, lease.DeviceID, taskID, scope, 3); err != nil {
						t.Fatal(err)
					}
				}
				ov, err := s.FleetOverview(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if ov.Devices[0].Status == "QUARANTINED" {
					t.Errorf("%s 连续 5 次仍不得隔离设备(差距 #10 的误伤)", scope)
				}
			})
		}
	})

	// device scope 达阈值才隔离,且 ok 能把计数清回去。
	t.Run("ReleaseDeviceQuarantineAndReset", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		for i := 1; i <= 2; i++ {
			taskID := fmt.Sprintf("w:t%d:a1", i)
			lease, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, taskID, 120)
			if err != nil || lease == nil {
				t.Fatalf("acquire %d: %+v %v", i, lease, err)
			}
			if err := s.ReleaseDevice(ctx, lease.DeviceID, taskID, wf.FailScopeDevice, 3); err != nil {
				t.Fatal(err)
			}
		}
		// 第 3 次成功 → 清零,不该隔离
		lease, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t3:a1", 120)
		if err != nil || lease == nil {
			t.Fatalf("acquire3: %+v %v", lease, err)
		}
		if err := s.ReleaseDevice(ctx, lease.DeviceID, "w:t3:a1", wf.FailScopeOK, 3); err != nil {
			t.Fatal(err)
		}
		ov, err := s.FleetOverview(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ov.Devices[0].FailStreak != 0 || ov.Devices[0].Status == "QUARANTINED" {
			t.Errorf("ok 应清零且不隔离, got %+v", ov.Devices[0])
		}
	})

	// 幂等:重复释放/非持有者释放不得重复计数(既有语义,加 scope 后必须保持)。
	t.Run("ReleaseDeviceScopeIdempotent", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		lease, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", 120)
		if err != nil || lease == nil {
			t.Fatalf("acquire: %+v %v", lease, err)
		}
		for i := 0; i < 3; i++ {
			if err := s.ReleaseDevice(ctx, lease.DeviceID, "w:t1:a1", wf.FailScopeClient, 3); err != nil {
				t.Fatal(err)
			}
		}
		// 非持有者释放
		if err := s.ReleaseDevice(ctx, lease.DeviceID, "w:other:a1", wf.FailScopeClient, 3); err != nil {
			t.Fatal(err)
		}
		ov, err := s.FleetOverview(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ov.Devices[0].ClientFailStreak != 1 {
			t.Errorf("client fail_streak = %d, want 1(只第一次生效)", ov.Devices[0].ClientFailStreak)
		}
	})

	t.Run("ConcurrentAcquireGrantsSingleLease", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		const n = 8
		var wg sync.WaitGroup
		leases := make([]*wf.Lease, n)
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				leases[i], errs[i] = s.AcquireDevice(ctx, wf.DeviceSelector{}, "t", 120)
			}(i)
		}
		wg.Wait()
		granted := 0
		for i := 0; i < n; i++ {
			if errs[i] != nil {
				t.Fatalf("acquire #%d: %v", i, errs[i])
			}
			if leases[i] != nil {
				granted++
			}
		}
		if granted != 1 {
			t.Errorf("granted = %d, want 1(租约独占,§11 行锁)", granted)
		}
	})

	// 租约生命周期状态机(§10 租约 120s 心跳续期):BUSY 设备在租约过期后被
	// AcquireDevice 懒回收——这是 workflow 被 Terminate/进程死亡等绕过
	// ReleaseDevice 场景的唯一恢复路径,必须有表驱动覆盖。
	t.Run("LeaseLifecycleRenewAndReclaim", func(t *testing.T) {
		cases := []struct {
			name          string
			leaseSeconds  int  // t1 初始租约时长;0 = 立即过期(模拟持有者失联)
			renew         bool // 租约过期后持有者是否凭所有权凭据续租
			renewSeconds  int
			wantReclaimed bool // t2 随后能否取得设备
		}{
			{"有效租约不得回收", 120, false, 0, false},
			{"过期租约懒回收", 0, false, 0, true},
			{"持有者续期阻止回收", 0, true, 120, false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s := newStore(t)
				seed(t, s)
				l, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", tc.leaseSeconds)
				if err != nil || l == nil {
					t.Fatalf("t1 acquire: lease=%v err=%v", l, err)
				}
				if tc.renew {
					ok, err := s.RenewLease(ctx, LeaseCredential{
						DeviceID: l.DeviceID, ClientID: l.ClientID, TaskID: "w:t1:a1",
						Attempt: 1, LeaseID: l.LeaseID, Generation: l.Generation,
					}, tc.renewSeconds)
					if err != nil || !ok {
						t.Fatalf("持有者续租应成功: ok=%v err=%v", ok, err)
					}
				}
				l2, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t2:a1", 120)
				if err != nil {
					t.Fatalf("t2 acquire: %v", err)
				}
				if (l2 != nil) != tc.wantReclaimed {
					t.Errorf("reclaimed = %v, want %v", l2 != nil, tc.wantReclaimed)
				}
			})
		}
	})

	// 租约所有权凭据(§10/差距 #15):续租是条件更新,任何一项失配都返回
	// false(LEASE_NOT_OWNED),旧持有者不得再续已易主/已释放的租约。
	t.Run("LeaseOwnershipCredentials", func(t *testing.T) {
		cases := []struct {
			name         string
			mutate       func(c *LeaseCredential)
			releaseFirst bool
			want         bool
		}{
			{"正确凭据续租成功", func(*LeaseCredential) {}, false, true},
			{"错client不得续租", func(c *LeaseCredential) { c.ClientID = "other" }, false, false},
			{"错task不得续租", func(c *LeaseCredential) { c.TaskID = "w:t9:a1" }, false, false},
			{"错lease_id不得续租", func(c *LeaseCredential) { c.LeaseID = "forged" }, false, false},
			{"错generation不得续租", func(c *LeaseCredential) { c.Generation++ }, false, false},
			{"attempt与task_id不一致不得续租", func(c *LeaseCredential) { c.Attempt = 2 }, false, false},
			{"已释放租约不得续租", func(*LeaseCredential) {}, true, false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s := newStore(t)
				seed(t, s)
				l, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", 120)
				if err != nil || l == nil {
					t.Fatalf("acquire: lease=%v err=%v", l, err)
				}
				cred := LeaseCredential{
					DeviceID: l.DeviceID, ClientID: l.ClientID, TaskID: "w:t1:a1",
					Attempt: 1, LeaseID: l.LeaseID, Generation: l.Generation,
				}
				tc.mutate(&cred)
				if tc.releaseFirst {
					if err := s.ReleaseDevice(ctx, l.DeviceID, "w:t1:a1", wf.FailScopeOK, 3); err != nil {
						t.Fatal(err)
					}
				}
				ok, err := s.RenewLease(ctx, cred, 120)
				if err != nil {
					t.Fatalf("renew: %v", err)
				}
				if ok != tc.want {
					t.Errorf("renewed = %v, want %v", ok, tc.want)
				}
			})
		}
		// 懒回收易主:generation 递增,旧持有者凭据全部失效,新持有者可续
		s := newStore(t)
		seed(t, s)
		old, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", 0) // 立即过期
		if err != nil || old == nil {
			t.Fatalf("acquire: %v %v", old, err)
		}
		newL, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t2:a1", 120)
		if err != nil || newL == nil {
			t.Fatalf("reclaim: %v %v", newL, err)
		}
		if newL.Generation != old.Generation+1 {
			t.Errorf("generation = %d → %d, want +1", old.Generation, newL.Generation)
		}
		if ok, _ := s.RenewLease(ctx, LeaseCredential{
			DeviceID: old.DeviceID, ClientID: old.ClientID, TaskID: "w:t1:a1",
			Attempt: 1, LeaseID: old.LeaseID, Generation: old.Generation,
		}, 120); ok {
			t.Error("旧持有者凭据在易主后不得续租")
		}
		if ok, _ := s.RenewLease(ctx, LeaseCredential{
			DeviceID: newL.DeviceID, ClientID: newL.ClientID, TaskID: "w:t2:a1",
			Attempt: 1, LeaseID: newL.LeaseID, Generation: newL.Generation,
		}, 120); !ok {
			t.Error("新持有者凭据应可续租")
		}
	})

	// GetLeaseExpiry:CheckLease 活动的数据源(原则 6)——持有中返回到期时刻,
	// 释放后/未知任务返回 nil(未续期)。
	t.Run("GetLeaseExpiryLifecycle", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		if exp, err := s.GetLeaseExpiry(ctx, "w:t1:a1"); err != nil || exp != nil {
			t.Errorf("未知任务: exp=%v err=%v, want (nil, nil)", exp, err)
		}
		l, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", 120)
		if err != nil || l == nil {
			t.Fatalf("acquire: %v %v", l, err)
		}
		exp, err := s.GetLeaseExpiry(ctx, "w:t1:a1")
		if err != nil || exp == nil {
			t.Fatalf("持有中: exp=%v err=%v", exp, err)
		}
		if time.Until(*exp) < 100*time.Second {
			t.Errorf("expiry = %v, want ~120s 后", exp)
		}
		if err := s.ReleaseDevice(ctx, l.DeviceID, "w:t1:a1", wf.FailScopeOK, 3); err != nil {
			t.Fatal(err)
		}
		if exp, err := s.GetLeaseExpiry(ctx, "w:t1:a1"); err != nil || exp != nil {
			t.Errorf("释放后: exp=%v err=%v, want (nil, nil)", exp, err)
		}
	})

	// lease_id 必须含不可猜的秘密材料(差距 #8 final-review)。它是 upload-requests
	// 端点唯一的鉴权依据,而该端点签发往证据桶写入的 URL、callbacks 又无其他鉴权。
	// 若 lease_id 等于 task_id(旧实现),凭据的全部成分都可猜:task_id 有规律、
	// client_id 可猜、device_id 就是 serial、attempt 编码在 task_id 里、
	// lease_generation 是每设备小计数——同网段主机试几次就能换到写入 URL。
	t.Run("LeaseIDCarriesEntropy", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		const taskID = "w:t1:a1"
		lease, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, taskID, 120)
		if err != nil || lease == nil {
			t.Fatalf("acquire: %+v %v", lease, err)
		}
		if lease.LeaseID == taskID {
			t.Fatal("lease_id 等于 task_id:凭据没有任何秘密材料,端点鉴权形同虚设")
		}
		// 前缀保留 task_id 便于排查;后缀是随机十六进制。
		suffix, ok := strings.CutPrefix(lease.LeaseID, taskID+":")
		if !ok {
			t.Fatalf("lease_id = %q, want %q 前缀", lease.LeaseID, taskID+":")
		}
		if len(suffix) != 32 { // 16 字节 hex
			t.Errorf("随机后缀长度 = %d, want 32(16 字节 hex)", len(suffix))
		}
		if _, err := hex.DecodeString(suffix); err != nil {
			t.Errorf("随机后缀不是十六进制: %q", suffix)
		}
		// 同一 task 重新获取(懒回收/重试)必须换新值,否则旧凭据仍然有效。
		if err := s.ReleaseDevice(ctx, lease.DeviceID, taskID, wf.FailScopeOK, 3); err != nil {
			t.Fatal(err)
		}
		again, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, taskID, 120)
		if err != nil || again == nil {
			t.Fatalf("re-acquire: %+v %v", again, err)
		}
		if again.LeaseID == lease.LeaseID {
			t.Error("同一 task 重新获取应生成新 lease_id,否则旧凭据不失效")
		}
	})

	// 只读租约校验(差距 #8 的签发端点鉴权依据):校验通过不得有任何副作用,
	// 尤其不得像 RenewLease 那样续期——签一次 URL 不等于任务还活着。
	t.Run("VerifyLeaseIsReadOnly", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		lease, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", 120)
		if err != nil || lease == nil {
			t.Fatalf("acquire: %+v %v", lease, err)
		}
		cred := LeaseCredential{
			DeviceID: lease.DeviceID, ClientID: lease.ClientID, TaskID: "w:t1:a1",
			Attempt: 1, LeaseID: lease.LeaseID, Generation: lease.Generation,
		}
		before, err := s.GetLeaseExpiry(ctx, "w:t1:a1")
		if err != nil || before == nil {
			t.Fatalf("expiry before: %v %v", before, err)
		}
		ok, err := s.VerifyLease(ctx, cred)
		if err != nil || !ok {
			t.Fatalf("VerifyLease = %v, %v; want true, nil", ok, err)
		}
		after, err := s.GetLeaseExpiry(ctx, "w:t1:a1")
		if err != nil || after == nil {
			t.Fatalf("expiry after: %v %v", after, err)
		}
		if !after.Equal(*before) {
			t.Errorf("校验不得续期: %v → %v", before, after)
		}
	})

	// 凭据任一项失配都必须判否——这是端点唯一的鉴权依据。
	t.Run("VerifyLeaseRejectsMismatch", func(t *testing.T) {
		base := func(l *wf.Lease) LeaseCredential {
			return LeaseCredential{
				DeviceID: l.DeviceID, ClientID: l.ClientID, TaskID: "w:t1:a1",
				Attempt: 1, LeaseID: l.LeaseID, Generation: l.Generation,
			}
		}
		cases := []struct {
			name   string
			mutate func(c *LeaseCredential)
		}{
			{"错 lease_id", func(c *LeaseCredential) { c.LeaseID = "bogus" }},
			{"错 generation", func(c *LeaseCredential) { c.Generation += 1 }},
			{"错 client_id", func(c *LeaseCredential) { c.ClientID = "other" }},
			{"错 task_id", func(c *LeaseCredential) { c.TaskID = "w:other:a1" }},
			{"错 device_id", func(c *LeaseCredential) { c.DeviceID = "no-such-device" }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s := newStore(t)
				seed(t, s)
				lease, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", 120)
				if err != nil || lease == nil {
					t.Fatalf("acquire: %+v %v", lease, err)
				}
				cred := base(lease)
				tc.mutate(&cred)
				ok, err := s.VerifyLease(ctx, cred)
				if err != nil {
					t.Fatalf("VerifyLease err = %v", err)
				}
				if ok {
					t.Errorf("%s 应判否", tc.name)
				}
			})
		}
	})

	// 已释放的租约不再是持有者(任务结束后不得继续换 URL)。
	t.Run("VerifyLeaseRejectsReleased", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		lease, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", 120)
		if err != nil || lease == nil {
			t.Fatalf("acquire: %+v %v", lease, err)
		}
		cred := LeaseCredential{
			DeviceID: lease.DeviceID, ClientID: lease.ClientID, TaskID: "w:t1:a1",
			Attempt: 1, LeaseID: lease.LeaseID, Generation: lease.Generation,
		}
		if err := s.ReleaseDevice(ctx, lease.DeviceID, "w:t1:a1", wf.FailScopeOK, 3); err != nil {
			t.Fatal(err)
		}
		ok, err := s.VerifyLease(ctx, cred)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Error("已释放的租约不得通过校验")
		}
	})

	// 从未被 AcquireDevice 过的设备(仅心跳注册)不得通过零值凭据校验——
	// MemStore 的 UpsertClientDevices 为新设备写入零值 deviceRow(LeaseTaskID/
	// LeaseID/Generation 均为 Go 零值),若 VerifyLease 不要求 status=BUSY,
	// 零值凭据会与零值行"巧合匹配"而被判真,陌生人即可对任意从未跑过任务的
	// 设备换到写入 URL。
	t.Run("VerifyLeaseRejectsNeverLeasedDevice", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		ok, err := s.VerifyLease(ctx, LeaseCredential{
			DeviceID: "513cd3de", ClientID: "c1", TaskID: "", LeaseID: "", Generation: 0,
		})
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Error("从未 AcquireDevice 过的设备不得通过零值凭据校验")
		}
	})

	// attempt 是端点唯一的鉴权依据之一:即便 device/client/task/lease_id/
	// generation 全部匹配一个真实活跃的租约,attempt 对不上也必须判否。
	t.Run("VerifyLeaseRejectsAttemptMismatch", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		lease, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", 120)
		if err != nil || lease == nil {
			t.Fatalf("acquire: %+v %v", lease, err)
		}
		ok, err := s.VerifyLease(ctx, LeaseCredential{
			DeviceID: lease.DeviceID, ClientID: lease.ClientID, TaskID: "w:t1:a1",
			Attempt: 99, LeaseID: lease.LeaseID, Generation: lease.Generation,
		})
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Error("attempt 与 task_id 后缀不一致应判否")
		}
	})

	// NextWorkflowAttempt(差距 #11):显式 retry 计数按逻辑键原子单调递增;
	// 未登记的键报错。
	t.Run("NextWorkflowAttemptMonotonic", func(t *testing.T) {
		s := newStore(t)
		art := Artifact{Project: "grp/p", CommitSHA: "abcd1234", PipelineID: 42,
			Variant: "v1", BuildType: "Release", URL: "u", SHA256: "s", Size: 1, ManifestDigest: "m"}
		if err := s.RegisterArtifacts(ctx, []Artifact{art}); err != nil {
			t.Fatal(err)
		}
		for want := 1; want <= 3; want++ {
			n, err := s.NextWorkflowAttempt(ctx, "grp/p", "abcd1234", 42, "v1")
			if err != nil || n != want {
				t.Fatalf("attempt = %d err=%v, want %d", n, err, want)
			}
		}
		if _, err := s.NextWorkflowAttempt(ctx, "grp/p", "abcd1234", 42, "ghost"); err == nil {
			t.Error("未登记的键应报错")
		}
	})

	// 懒回收后租约易主:旧持有者的 ReleaseDevice 必须幂等空转,
	// 不得把新持有者的设备释放掉(§3 规则 7 幂等)。
	t.Run("ReclaimTransfersOwnership", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		l, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "dead-task", 0) // 立即过期
		if err != nil || l == nil {
			t.Fatalf("acquire: lease=%v err=%v", l, err)
		}
		l2, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t2", 120) // 回收
		if err != nil || l2 == nil {
			t.Fatalf("reclaim: lease=%v err=%v", l2, err)
		}
		if err := s.ReleaseDevice(ctx, l.DeviceID, "dead-task", wf.FailScopeDevice, 3); err != nil {
			t.Fatal(err)
		}
		if l3, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t3", 120); l3 != nil {
			t.Errorf("旧持有者释放不得影响新租约: %+v", l3)
		}
	})

	t.Run("TaskLifecycleAndEventDedup", func(t *testing.T) {
		s := newStore(t)
		row := wf.TaskRow{TaskID: "w:t1:a1", WorkflowID: "w", TestID: "t1", Attempt: 1,
			IdempotencyKey: "w:t1:a1", ClientID: "c1", DeviceID: "d1", Status: "DISPATCHING"}
		if err := s.CreateTask(ctx, row); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateTask(ctx, row); err != nil { // 同幂等键重复创建:无副作用
			t.Fatalf("重复创建应幂等: %v", err)
		}
		got, err := s.GetTask(ctx, "w:t1:a1")
		if err != nil || got == nil {
			t.Fatalf("get task = %+v, err=%v", got, err)
		}
		if got.WorkflowID != "w" || got.TestID != "t1" || got.Attempt != 1 ||
			got.ClientID != "c1" || got.DeviceID != "d1" || got.Status != "DISPATCHING" {
			t.Errorf("task row = %+v", got)
		}
		if missing, err := s.GetTask(ctx, "no-such"); err != nil || missing != nil {
			t.Errorf("未知任务应返回 (nil, nil): %+v %v", missing, err)
		}

		ins, err := s.AppendTaskEvent(ctx, TaskEvent{TaskID: "w:t1:a1", Seq: 1, From: "ACCEPTED", To: "RUNNING"})
		if err != nil || !ins {
			t.Fatalf("first event: ins=%v err=%v", ins, err)
		}
		ins, err = s.AppendTaskEvent(ctx, TaskEvent{TaskID: "w:t1:a1", Seq: 1, From: "ACCEPTED", To: "RUNNING"})
		if err != nil || ins {
			t.Fatalf("重复 seq 应去重: ins=%v err=%v", ins, err)
		}
		if err := s.SetTaskStatus(ctx, "w:t1:a1", "RUNNING"); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishTask(ctx, wf.FinishRequest{TaskID: "w:t1:a1", Status: "COMPLETED",
			Verdict: "PASSED", Category: "", Reason: "ok"}); err != nil {
			t.Fatal(err)
		}
		got, _ = s.GetTask(ctx, "w:t1:a1")
		if got.Status != "COMPLETED" {
			t.Errorf("status = %s", got.Status)
		}
	})

	t.Run("SaveResultDedup", func(t *testing.T) {
		s := newStore(t)
		_ = s.CreateTask(ctx, wf.TaskRow{TaskID: "w:t1:a1", IdempotencyKey: "w:t1:a1"})
		rec := wf.ResultRecord{TaskID: "w:t1:a1", Result: wf.TaskResultSignal{
			TaskID: "w:t1:a1", Status: "COMPLETED", ExitCode: 0, DurationSec: 412,
			CasesTotal: 38, CasesFailed: 0, SignaturesHit: []string{},
			Metrics:     map[string]float64{"latency_ms_p50": 12.4},
			Attachments: []wf.Attachment{{Name: "logcat.txt", ObjectKey: "runs/x/logcat.txt", SHA256: "s", Size: 9}},
		}}
		ins, err := s.SaveResult(ctx, rec)
		if err != nil || !ins {
			t.Fatalf("first save: ins=%v err=%v", ins, err)
		}
		ins, err = s.SaveResult(ctx, rec) // 回调重发
		if err != nil || ins {
			t.Fatalf("重复结果应去重: ins=%v err=%v", ins, err)
		}
	})

	// 事务性 Outbox(原则 3):results + outbox 单事务写入,两侧各自幂等。
	t.Run("SaveResultWithOutboxIdempotent", func(t *testing.T) {
		s := newStore(t)
		_ = s.CreateTask(ctx, wf.TaskRow{TaskID: "w:t1:a1", IdempotencyKey: "w:t1:a1"})
		rec := wf.ResultRecord{TaskID: "w:t1:a1", Result: wf.TaskResultSignal{
			TaskID: "w:t1:a1", Status: "COMPLETED", ExitCode: 0, CasesTotal: 38,
		}}
		payload := json.RawMessage(`{"workflow_id":"w","result":{"task_id":"w:t1:a1"}}`)
		ev := OutboxEvent{AggregateType: "task", AggregateID: "w:t1:a1",
			EventType: EventTypeTaskResult, EventKey: "w:t1:a1:result", Payload: payload}
		ins, err := s.SaveResultWithOutbox(ctx, rec, ev)
		if err != nil || !ins {
			t.Fatalf("first save: ins=%v err=%v", ins, err)
		}
		// 回调重发:同 task_id 结果去重、同 event_key 不产生第二行、不报错
		ins, err = s.SaveResultWithOutbox(ctx, rec, ev)
		if err != nil || ins {
			t.Fatalf("重复写入应去重: ins=%v err=%v", ins, err)
		}
		rows, err := s.ClaimUnpublished(ctx, 10)
		if err != nil || len(rows) != 1 {
			t.Fatalf("claim = %+v err=%v, want 单行", rows, err)
		}
		got := rows[0]
		if got.AggregateType != "task" || got.AggregateID != "w:t1:a1" ||
			got.EventType != EventTypeTaskResult || got.EventKey != "w:t1:a1:result" ||
			got.Attempts != 0 || got.ID == 0 {
			t.Errorf("outbox row = %+v", got)
		}
		// GetResult 权威读(LoadResult 活动,差距 #2)
		loaded, err := s.GetResult(ctx, "w:t1:a1")
		if err != nil || loaded == nil {
			t.Fatalf("get result = %+v err=%v", loaded, err)
		}
		if loaded.Result.Status != "COMPLETED" || loaded.Result.CasesTotal != 38 {
			t.Errorf("loaded result = %+v", loaded.Result)
		}
		if missing, err := s.GetResult(ctx, "no-such"); err != nil || missing != nil {
			t.Errorf("未知任务应返回 (nil, nil): %+v %v", missing, err)
		}
	})

	// outbox 投递生命周期状态机:unpublished →(MarkFailed 累计 attempts,行保持
	// 未投递)→ MarkPublished 终态;两个 Mark* 都只作用于未投递行(表驱动)。
	t.Run("OutboxLifecycle", func(t *testing.T) {
		type op struct {
			fail    string // 非空 → MarkFailed(id, 该错误)
			publish bool   // → MarkPublished(id)
		}
		cases := []struct {
			name          string
			ops           []op
			wantPending   bool // 最终仍待投递
			wantAttempts  int
			wantLastError string
		}{
			{"直接投递成功", []op{{publish: true}}, false, 0, ""},
			{"失败后重投成功", []op{{fail: "boom"}, {publish: true}}, false, 1, "boom"},
			{"连续失败累积attempts", []op{{fail: "e1"}, {fail: "e2"}}, true, 2, "e2"},
			{"重复MarkPublished幂等", []op{{publish: true}, {publish: true}}, false, 0, ""},
			{"已投递后MarkFailed无副作用", []op{{publish: true}, {fail: "late"}}, false, 0, ""},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s := newStore(t)
				_ = s.CreateTask(ctx, wf.TaskRow{TaskID: "w:t1:a1", IdempotencyKey: "w:t1:a1"})
				_, err := s.SaveResultWithOutbox(ctx,
					wf.ResultRecord{TaskID: "w:t1:a1", Result: wf.TaskResultSignal{TaskID: "w:t1:a1"}},
					OutboxEvent{AggregateType: "task", AggregateID: "w:t1:a1",
						EventType: EventTypeTaskResult, EventKey: "w:t1:a1:result",
						Payload: json.RawMessage(`{}`)})
				if err != nil {
					t.Fatal(err)
				}
				rows, err := s.ClaimUnpublished(ctx, 10)
				if err != nil || len(rows) != 1 {
					t.Fatalf("claim = %+v err=%v", rows, err)
				}
				id := rows[0].ID
				for _, o := range tc.ops {
					if o.fail != "" {
						if err := s.MarkFailed(ctx, id, o.fail); err != nil {
							t.Fatalf("mark failed: %v", err)
						}
					}
					if o.publish {
						if err := s.MarkPublished(ctx, id); err != nil {
							t.Fatalf("mark published: %v", err)
						}
					}
				}
				rows, err = s.ClaimUnpublished(ctx, 10)
				if err != nil {
					t.Fatal(err)
				}
				if (len(rows) == 1) != tc.wantPending {
					t.Fatalf("pending rows = %d, wantPending=%v", len(rows), tc.wantPending)
				}
				if tc.wantPending {
					if rows[0].Attempts != tc.wantAttempts || rows[0].LastError != tc.wantLastError {
						t.Errorf("row = %+v, want attempts=%d last_error=%q",
							rows[0], tc.wantAttempts, tc.wantLastError)
					}
				}
			})
		}
	})

	// 积压监控(第四批):pending/stuck 计数、最老行定位、诊断用 last_error 采样。
	// 两套实现必须给出同样的结论,否则内存模式下的告警演练不能代表生产。
	t.Run("OutboxBacklog", func(t *testing.T) {
		s := newStore(t)
		seed := func(taskID string) int64 {
			t.Helper()
			if err := s.CreateTask(ctx, wf.TaskRow{TaskID: taskID, IdempotencyKey: taskID}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SaveResultWithOutbox(ctx,
				wf.ResultRecord{TaskID: taskID, Result: wf.TaskResultSignal{TaskID: taskID}},
				OutboxEvent{AggregateType: "task", AggregateID: taskID,
					EventType: EventTypeTaskResult, EventKey: taskID + ":result",
					Payload: json.RawMessage(`{}`)}); err != nil {
				t.Fatal(err)
			}
			rows, err := s.ClaimUnpublished(ctx, 100)
			if err != nil {
				t.Fatal(err)
			}
			return rows[len(rows)-1].ID // 最新插入的一行
		}

		// 空 outbox:全零,且不得把"没有行"报成年龄非零。
		b, err := s.OutboxBacklog(ctx, 3)
		if err != nil {
			t.Fatal(err)
		}
		if b.Pending != 0 || b.Stuck != 0 || b.OldestAge != 0 || b.OldestID != 0 {
			t.Fatalf("空 outbox backlog = %+v, want 全零", b)
		}

		oldest := seed("w:t1:a1")
		newest := seed("w:t2:a1")

		// 两行待投,都没失败过 → 不算卡住;最老行是先插入的那条。
		b, err = s.OutboxBacklog(ctx, 3)
		if err != nil {
			t.Fatal(err)
		}
		if b.Pending != 2 || b.Stuck != 0 {
			t.Errorf("backlog = %+v, want pending=2 stuck=0", b)
		}
		if b.OldestID != oldest {
			t.Errorf("OldestID = %d, want %d(先入库的那行)", b.OldestID, oldest)
		}

		// 让新的那行失败 3 次 → 达到阈值算卡住;采样的 last_error 取尝试最多的行。
		for i := 0; i < 3; i++ {
			if err := s.MarkFailed(ctx, newest, "boom"); err != nil {
				t.Fatal(err)
			}
		}
		b, err = s.OutboxBacklog(ctx, 3)
		if err != nil {
			t.Fatal(err)
		}
		if b.Pending != 2 || b.Stuck != 1 {
			t.Errorf("backlog = %+v, want pending=2 stuck=1", b)
		}
		if b.SampleError != "boom" {
			t.Errorf("SampleError = %q, want boom(尝试次数最多那行的 last_error)", b.SampleError)
		}
		// 阈值可调:抬到 4 就不该再算卡住。
		b, err = s.OutboxBacklog(ctx, 4)
		if err != nil {
			t.Fatal(err)
		}
		if b.Stuck != 0 {
			t.Errorf("阈值 4 时 stuck = %d, want 0", b.Stuck)
		}

		// 投递掉最老一行 → pending 递减,最老行前移。
		if err := s.MarkPublished(ctx, oldest); err != nil {
			t.Fatal(err)
		}
		b, err = s.OutboxBacklog(ctx, 3)
		if err != nil {
			t.Fatal(err)
		}
		if b.Pending != 1 || b.OldestID != newest {
			t.Errorf("backlog = %+v, want pending=1 oldest=%d", b, newest)
		}
	})

	// 结论性判定边界(bundle webhook 跳过已测变体):
	// status='COMPLETED' 且 verdict ∈ {PASSED, TEST_FAILED} 才算结论;
	// INFRA_ERROR/TIMEOUT(测试未必真实执行)、非终态、无记录均需重测。
	t.Run("ConclusiveWorkflowIDsBoundary", func(t *testing.T) {
		cases := []struct {
			name       string
			status     string // FinishTask 落库的 status
			verdict    string
			conclusive bool
		}{
			{"PASSED 结论", "COMPLETED", "PASSED", true},
			{"TEST_FAILED 结论(测试真实跑完)", "COMPLETED", "TEST_FAILED", true},
			{"INFRA_ERROR 非结论", "FAILED", "INFRA_ERROR", false},
			{"TIMEOUT 非结论", "TIMEOUT", "INFRA_ERROR", false},
			{"status 非 COMPLETED 即使 verdict 通过也不算", "FAILED", "PASSED", false},
			{"COMPLETED 但 verdict 未判定", "COMPLETED", "", false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s := newStore(t)
				const wfID = "device-test-grp/p-gabcd1234-p42-v1"
				if err := s.CreateTask(ctx, wf.TaskRow{TaskID: wfID + ":t1:a1",
					WorkflowID: wfID, IdempotencyKey: wfID + ":t1:a1"}); err != nil {
					t.Fatal(err)
				}
				if err := s.FinishTask(ctx, wf.FinishRequest{TaskID: wfID + ":t1:a1",
					Status: tc.status, Verdict: tc.verdict}); err != nil {
					t.Fatal(err)
				}
				got, err := s.ConclusiveWorkflowIDs(ctx, []string{wfID, "device-test-grp/p-gabcd1234-p42-v2"})
				if err != nil {
					t.Fatal(err)
				}
				if got[wfID] != tc.conclusive {
					t.Errorf("conclusive = %v, want %v", got[wfID], tc.conclusive)
				}
				if got["device-test-grp/p-gabcd1234-p42-v2"] {
					t.Error("无记录的 workflow 不得判结论")
				}
			})
		}
	})

	// evidence_snapshots(差距 #6,决策可回放):幂等登记 + 读回;未知 id (nil,nil)。
	t.Run("EvidenceSnapshotIdempotentRoundTrip", func(t *testing.T) {
		s := newStore(t)
		snap := EvidenceSnapshot{
			EvidenceID: "w:t1:a1", TaskID: "w:t1:a1", Attempt: 1,
			ObjectKey: "evidence/w:t1:a1/evidence.json",
			SHA256:    "deadbeef", ExtractorVersion: "1",
		}
		if err := s.SaveEvidenceSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
		// 重复登记(activity 重试/重复提取):无副作用,保留首次内容
		dup := snap
		dup.SHA256 = "changed"
		if err := s.SaveEvidenceSnapshot(ctx, dup); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetEvidenceSnapshot(ctx, "w:t1:a1")
		if err != nil || got == nil {
			t.Fatalf("get = %+v err=%v", got, err)
		}
		if *got != snap {
			t.Errorf("snapshot = %+v, want %+v(首次内容,幂等)", *got, snap)
		}
		if missing, err := s.GetEvidenceSnapshot(ctx, "no-such"); err != nil || missing != nil {
			t.Errorf("未知 id 应返回 (nil, nil): %+v %v", missing, err)
		}
	})

	// 飞书指令查询面:FleetOverview 汇总 + UnquarantineDevice 解隔离。
	t.Run("FleetOverviewAndUnquarantine", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		// 一台 BUSY 带活跃租约 + 一台任务非终态的 workflow
		l, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", 120)
		if err != nil || l == nil {
			t.Fatalf("acquire: %v %v", l, err)
		}
		_ = s.CreateTask(ctx, wf.TaskRow{TaskID: "w:t1:a1", WorkflowID: "w",
			IdempotencyKey: "w:t1:a1", Status: "RUNNING"})
		ov, err := s.FleetOverview(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ov.InflightWorkflows != 1 || ov.ActiveLeases != 1 || len(ov.Devices) != 1 {
			t.Fatalf("overview = %+v", ov)
		}
		d := ov.Devices[0]
		if d.DeviceID != "513cd3de" || d.Status != "BUSY" || d.LeaseTaskID != "w:t1:a1" ||
			d.SOC != "trinket" {
			t.Errorf("device = %+v", d)
		}
		// 任务终态后运行中数归零(租约释放后活跃租约归零)
		if err := s.FinishTask(ctx, wf.FinishRequest{TaskID: "w:t1:a1", Status: "COMPLETED", Verdict: "PASSED"}); err != nil {
			t.Fatal(err)
		}
		if err := s.ReleaseDevice(ctx, l.DeviceID, "w:t1:a1", wf.FailScopeOK, 3); err != nil {
			t.Fatal(err)
		}
		ov, _ = s.FleetOverview(ctx)
		if ov.InflightWorkflows != 0 || ov.ActiveLeases != 0 {
			t.Errorf("终态后 overview = %+v", ov)
		}
		// 隔离 → 解隔离循环(3 次 INFRA → QUARANTINED,§10)
		for i := 0; i < 3; i++ {
			l2, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t2:a1", 120)
			if l2 == nil {
				t.Fatalf("第 %d 次 acquire", i+1)
			}
			_ = s.ReleaseDevice(ctx, l2.DeviceID, "w:t2:a1", wf.FailScopeDevice, 3)
		}
		ok, err := s.UnquarantineDevice(ctx, l.DeviceID)
		if err != nil || !ok {
			t.Fatalf("unquarantine: ok=%v err=%v", ok, err)
		}
		ov, _ = s.FleetOverview(ctx)
		if ov.Devices[0].Status != "IDLE" || ov.Devices[0].FailStreak != 0 {
			t.Errorf("解隔离后 device = %+v", ov.Devices[0])
		}
		if ok, _ := s.UnquarantineDevice(ctx, "ghost"); ok {
			t.Error("未知设备应返回 false")
		}
	})

	// 飞书指令 rerun 的数据面:ListArtifacts 按逻辑键取包,
	// NextWorkflowAttemptAll 把全键推进到同一个新水位(变体行可能因 kick retry 发散)。
	t.Run("ListArtifactsAndAttemptAll", func(t *testing.T) {
		s := newStore(t)
		arts := []Artifact{
			{Project: "grp/p", CommitSHA: "abcd1234", PipelineID: 42, Variant: "v1",
				BuildType: "Release", URL: "u1", SHA256: "s1", Size: 1, ManifestDigest: "m1"},
			{Project: "grp/p", CommitSHA: "abcd1234", PipelineID: 42, Variant: "v2",
				BuildType: "Release", URL: "u2", SHA256: "s2", Size: 2, ManifestDigest: "m2"},
			{Project: "grp/p", CommitSHA: "abcd1234", PipelineID: 43, Variant: "v1",
				BuildType: "Release", URL: "u3", SHA256: "s3", Size: 3, ManifestDigest: "m3"},
			{Project: "grp/other", CommitSHA: "abcd1234", PipelineID: 42, Variant: "v1",
				BuildType: "Release", URL: "other1", SHA256: "so1", Size: 4, ManifestDigest: "mo1"},
			{Project: "grp/other", CommitSHA: "abcd1234", PipelineID: 42, Variant: "v2",
				BuildType: "Release", URL: "other2", SHA256: "so2", Size: 5, ManifestDigest: "mo2"},
		}
		if err := s.RegisterArtifacts(ctx, arts); err != nil {
			t.Fatal(err)
		}
		got, err := s.ListArtifacts(ctx, "grp/p", "abcd1234", 42)
		if err != nil || len(got) != 2 {
			t.Fatalf("list = %+v err=%v, want 2 行", got, err)
		}
		if got[0].Project != "grp/p" || got[0].URL == "" || got[0].ManifestDigest == "" {
			t.Errorf("artifact 字段不全: %+v", got[0])
		}
		if none, _ := s.ListArtifacts(ctx, "grp/p", "abcd1234", 99); len(none) != 0 {
			t.Errorf("无记录键应返回空: %+v", none)
		}
		other, err := s.ListArtifacts(ctx, "grp/other", "abcd1234", 42)
		if err != nil || len(other) != 2 || other[0].Project != "grp/other" {
			t.Fatalf("other project list = %+v err=%v, want isolated rows", other, err)
		}
		// 变体级 retry 使 v2 行先发散到 3;全组分配必须把两行都对齐到 4。
		for want := 1; want <= 3; want++ {
			if n, err := s.NextWorkflowAttempt(ctx, "grp/p", "abcd1234", 42, "v2"); err != nil || n != want {
				t.Fatalf("variant attempt = %d err=%v, want %d", n, err, want)
			}
		}
		n, err := s.NextWorkflowAttemptAll(ctx, "grp/p", "abcd1234", 42)
		if err != nil || n != 4 {
			t.Fatalf("attempt all = %d err=%v, want 4(max 发散值+1)", n, err)
		}
		for _, variant := range []string{"v1", "v2"} {
			n, err := s.NextWorkflowAttempt(ctx, "grp/p", "abcd1234", 42, variant)
			if err != nil || n != 5 {
				t.Fatalf("%s attempt after all = %d err=%v, want 5", variant, n, err)
			}
		}
		if n, err := s.NextWorkflowAttempt(ctx, "grp/other", "abcd1234", 42, "v1"); err != nil || n != 1 {
			t.Fatalf("other variant attempt = %d err=%v, want 1", n, err)
		}
		if n, err := s.NextWorkflowAttemptAll(ctx, "grp/other", "abcd1234", 42); err != nil || n != 2 {
			t.Fatalf("other attempt all = %d err=%v, want 2", n, err)
		}
		if _, err := s.NextWorkflowAttemptAll(ctx, "grp/p", "abcd1234", 99); err == nil {
			t.Error("无记录键应报错")
		}
		attempts := artifactAttempts(t, s)
		if attempts["grp/p"] != 5 {
			t.Errorf("grp/p v1 workflow attempt = %d, want 5", attempts["grp/p"])
		}
		if attempts["grp/other"] != 2 {
			t.Errorf("grp/other v1 workflow attempt = %d, want independently incremented to 2",
				attempts["grp/other"])
		}
	})

	t.Run("NextWorkflowAttemptAllConcurrentWaterlines", func(t *testing.T) {
		s := newStore(t)
		if err := s.RegisterArtifacts(ctx, []Artifact{
			{Project: "grp/p", CommitSHA: "feed1234", PipelineID: 7, Variant: "v1",
				BuildType: "Release", URL: "u1", SHA256: "s1"},
			{Project: "grp/p", CommitSHA: "feed1234", PipelineID: 7, Variant: "v2",
				BuildType: "Release", URL: "u2", SHA256: "s2"},
		}); err != nil {
			t.Fatal(err)
		}
		for want := 1; want <= 3; want++ {
			if n, err := s.NextWorkflowAttempt(ctx, "grp/p", "feed1234", 7, "v2"); err != nil || n != want {
				t.Fatalf("skew v2 = %d err=%v, want %d", n, err, want)
			}
		}

		const calls = 8
		results := make(chan int, calls)
		errs := make(chan error, calls)
		var wg sync.WaitGroup
		for range calls {
			wg.Add(1)
			go func() {
				defer wg.Done()
				n, err := s.NextWorkflowAttemptAll(ctx, "grp/p", "feed1234", 7)
				results <- n
				errs <- err
			}()
		}
		wg.Wait()
		close(results)
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent attempt all: %v", err)
			}
		}
		got := make([]int, 0, calls)
		for n := range results {
			got = append(got, n)
		}
		sort.Ints(got)
		want := []int{4, 5, 6, 7, 8, 9, 10, 11}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("concurrent waterlines = %v, want %v", got, want)
		}
		for _, variant := range []string{"v1", "v2"} {
			n, err := s.NextWorkflowAttempt(ctx, "grp/p", "feed1234", 7, variant)
			if err != nil || n != 12 {
				t.Fatalf("%s after concurrent all = %d err=%v, want 12", variant, n, err)
			}
		}
	})

	t.Run("NextWorkflowAttemptMixedConcurrentNamespace", func(t *testing.T) {
		s := newStore(t)
		if err := s.RegisterArtifacts(ctx, []Artifact{
			{Project: "grp/p", CommitSHA: "face1234", PipelineID: 8, Variant: "v1",
				BuildType: "Release", URL: "u1", SHA256: "s1"},
			{Project: "grp/p", CommitSHA: "face1234", PipelineID: 8, Variant: "v2",
				BuildType: "Release", URL: "u2", SHA256: "s2"},
		}); err != nil {
			t.Fatal(err)
		}

		const callsPerKind = 8
		results := make(chan int, callsPerKind*2)
		errs := make(chan error, callsPerKind*2)
		var wg sync.WaitGroup
		for range callsPerKind {
			wg.Add(2)
			go func() {
				defer wg.Done()
				n, err := s.NextWorkflowAttemptAll(ctx, "grp/p", "face1234", 8)
				results <- n
				errs <- err
			}()
			go func() {
				defer wg.Done()
				n, err := s.NextWorkflowAttempt(ctx, "grp/p", "face1234", 8, "v1")
				results <- n
				errs <- err
			}()
		}
		wg.Wait()
		close(results)
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("mixed concurrent attempt: %v", err)
			}
		}
		got := make([]int, 0, callsPerKind*2)
		for n := range results {
			got = append(got, n)
		}
		sort.Ints(got)
		want := make([]int, callsPerKind*2)
		for i := range want {
			want[i] = i + 1
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("mixed shared-namespace attempts = %v, want %v", got, want)
		}
	})

	t.Run("DecisionsRoundTripInOrder", func(t *testing.T) {
		s := newStore(t)
		_ = s.CreateTask(ctx, wf.TaskRow{TaskID: "w:t1:a1", IdempotencyKey: "w:t1:a1"})
		rule := wf.DecisionRow{TaskID: "w:t1:a1", Actor: "rule",
			Output: json.RawMessage(`{"verdict":"PASS","rule":"exit_code"}`)}
		llm := wf.DecisionRow{TaskID: "w:t1:a1", Actor: "hermes",
			InputDigest: "sha256:abc123", Model: "kimi-for-coding", PromptVersion: "analyzer-v3",
			Output:             json.RawMessage(`{"category":"PRODUCT","confidence":0.9}`),
			EvidenceSnapshotID: "w:t1:a1"}
		if err := s.SaveDecision(ctx, rule); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveDecision(ctx, llm); err != nil {
			t.Fatal(err)
		}
		got, err := s.ListDecisions(ctx, "w:t1:a1")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("decisions = %d, want 2", len(got))
		}
		if got[0].Actor != "rule" || got[1].Actor != "hermes" {
			t.Errorf("顺序应为 rule → hermes: %+v", got)
		}
		// JSONB 回读会做规范化(key 排序/空白),output 按语义比较而非字节比较
		assertJSONEqual := func(want json.RawMessage, got json.RawMessage) {
			t.Helper()
			var w, g any
			if err := json.Unmarshal(want, &w); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(got, &g); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(w, g) {
				t.Errorf("output = %s, want %s(语义)", got, want)
			}
		}
		assertJSONEqual(rule.Output, got[0].Output)
		if got[1].InputDigest != "sha256:abc123" || got[1].Model != "kimi-for-coding" ||
			got[1].PromptVersion != "analyzer-v3" || got[1].EvidenceSnapshotID != "w:t1:a1" {
			t.Errorf("hermes decision 字段不完整: %+v", got[1])
		}
		// rule 裁决不带快照引用(基于 result,不基于 evidence)
		if got[0].EvidenceSnapshotID != "" {
			t.Errorf("rule decision 不应带 evidence_snapshot_id: %+v", got[0])
		}
		assertJSONEqual(llm.Output, got[1].Output)
		if none, err := s.ListDecisions(ctx, "no-such"); err != nil || len(none) != 0 {
			t.Errorf("未知任务应返回空: %v %v", none, err)
		}
	})

	// 飞书指令层自然语言翻译审计(设计文档 §4.3):追加式,确认流程不更新
	// 已有行,只追加新行;同 open_id 按时序读就是完整证据链,最新在前。
	t.Run("CommandTranslationsAppendOnly", func(t *testing.T) {
		s := newStore(t)
		rows := []CommandTranslation{
			{OpenID: "ou_1", RawText: "看下设备状态", PromptVersion: "cmd_translate_v1",
				Model: "m", ContextDigest: "abc", Output: []byte(`{"command":"devices"}`),
				Rendered: "devices", Outcome: OutcomeExecuted},
			{OpenID: "ou_1", RawText: "重跑昨天那个", ContextDigest: "def",
				Output: []byte(`{"command":"rerun"}`), Rendered: "rerun 9da3b9d9 56",
				Outcome: OutcomePendingConfirm},
			{OpenID: "ou_1", RawText: "y", Rendered: "rerun 9da3b9d9 56", Outcome: OutcomeConfirmed},
		}
		for _, r := range rows {
			if err := s.SaveCommandTranslation(ctx, r); err != nil {
				t.Fatalf("SaveCommandTranslation: %v", err)
			}
		}
		// 追加式审计:三行都在,顺序即时序
		got, err := s.ListCommandTranslations(ctx, "ou_1", 10)
		if err != nil {
			t.Fatalf("ListCommandTranslations: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("len = %d, want 3", len(got))
		}
		if got[0].Outcome != OutcomeConfirmed {
			t.Errorf("最新一行 outcome = %q, want %q", got[0].Outcome, OutcomeConfirmed)
		}
	})

	t.Run("CommandTranslationTruncatesOutput", func(t *testing.T) {
		s := newStore(t)
		big := append([]byte(`{"junk":"`), bytes.Repeat([]byte("x"), 8000)...)
		big = append(big, []byte(`"}`)...)
		if err := s.SaveCommandTranslation(ctx, CommandTranslation{
			OpenID: "ou_2", RawText: "x", Output: big, Outcome: OutcomeRejectedSchema,
		}); err != nil {
			t.Fatalf("SaveCommandTranslation: %v", err)
		}
		got, err := s.ListCommandTranslations(ctx, "ou_2", 1)
		if err != nil {
			t.Fatalf("ListCommandTranslations: %v", err)
		}
		// 断言"确实截断了",而非仅仅"没有超过某个宽松上限"(原始 8011 字节本身
		// 就合法 JSON 且小于 outputLimit*2,松散上限无法区分"正确截断"与"完全
		// 没截断")。两个后端都应满足:落库字节数严格小于原始输入,且带尾标记。
		stored := string(got[0].Output)
		if len(stored) >= len(big) {
			t.Errorf("output 未截断: %d 字节(原始 %d)", len(stored), len(big))
		}
		if !strings.Contains(stored, truncatedMark) {
			n := 80
			if len(stored) < n {
				n = len(stored)
			}
			t.Errorf("截断后应带尾标记 %q, got %q...", truncatedMark, stored[:n])
		}
	})

	t.Run("RecentRunsFiltersByTestID", func(t *testing.T) {
		s := newStore(t)
		const proj, sha, iid = "Algo_Super_SDK", "9da3b9d9", 56 // 项目名含下划线:通配符地雷
		v1, v2, v3 := "aarch64_Android_SNPE_1.68", "aarch64_Android_SNPE_2.21", "aarch64_Android_RKNN_2.3.2"
		base := wf.BaseWorkflowID(proj, sha, iid)

		if err := s.RegisterArtifacts(ctx, []Artifact{
			{Project: proj, CommitSHA: sha, PipelineID: iid, Variant: v1, URL: "u1", SHA256: "s1"},
			{Project: proj, CommitSHA: sha, PipelineID: iid, Variant: v2, URL: "u2", SHA256: "s2"},
			{Project: proj, CommitSHA: sha, PipelineID: iid, Variant: v3, URL: "u3", SHA256: "s3"},
		}); err != nil {
			t.Fatalf("RegisterArtifacts: %v", err)
		}

		// bundle workflow:两个变体的 task 挂在同一个 workflow_id 上,靠 test_id 区分
		for _, tc := range []struct{ variant, verdict string }{{v1, "TEST_FAILED"}, {v2, "PASSED"}} {
			taskID := base + ":" + tc.variant + ":a1"
			if err := s.CreateTask(ctx, wf.TaskRow{
				TaskID: taskID, WorkflowID: base, TestID: tc.variant, Attempt: 1,
				IdempotencyKey: taskID, Status: "RUNNING",
			}); err != nil {
				t.Fatalf("CreateTask: %v", err)
			}
			if err := s.FinishTask(ctx, wf.FinishRequest{
				TaskID: taskID, Status: "COMPLETED", Verdict: tc.verdict,
			}); err != nil {
				t.Fatalf("FinishTask: %v", err)
			}
		}

		// 纯变体级 workflow(无 bundle、无 retry 后缀):kick 单变体触发的最常见形态,
		// Attempt=0 且 workflow_id = base-{variant},不带 -r{N}。
		plainWF := base + "-" + v3
		plainTask := plainWF + ":" + v3 + ":a1"
		if err := s.CreateTask(ctx, wf.TaskRow{
			TaskID: plainTask, WorkflowID: plainWF, TestID: v3, Attempt: 0,
			IdempotencyKey: plainTask, Status: "RUNNING",
		}); err != nil {
			t.Fatalf("CreateTask plain variant-level: %v", err)
		}
		if err := s.FinishTask(ctx, wf.FinishRequest{
			TaskID: plainTask, Status: "COMPLETED", Verdict: "PASSED",
		}); err != nil {
			t.Fatalf("FinishTask plain variant-level: %v", err)
		}

		runs, err := s.RecentRuns(ctx, 10)
		if err != nil {
			t.Fatalf("RecentRuns: %v", err)
		}
		byVariant := map[string]RecentRun{}
		for _, r := range runs {
			byVariant[r.Variant] = r
		}
		if got := byVariant[v1].Verdict; got != "TEST_FAILED" {
			t.Errorf("%s verdict = %q, want TEST_FAILED(bundle 下必须按 test_id 过滤,不得串变体)", v1, got)
		}
		if got := byVariant[v2].Verdict; got != "PASSED" {
			t.Errorf("%s verdict = %q, want PASSED", v2, got)
		}
		if got := byVariant[v3].Verdict; got != "PASSED" {
			t.Errorf("%s verdict = %q, want PASSED(纯变体级 workflow,无 bundle/retry 后缀)", v3, got)
		}

		// 变体级 retry workflow:更晚的行应覆盖 bundle 的结论
		retryWF := base + "-" + v1 + "-r2"
		retryTask := retryWF + ":" + v1 + ":a1"
		if err := s.CreateTask(ctx, wf.TaskRow{
			TaskID: retryTask, WorkflowID: retryWF, TestID: v1, Attempt: 1,
			IdempotencyKey: retryTask, Status: "RUNNING",
		}); err != nil {
			t.Fatalf("CreateTask retry: %v", err)
		}
		if err := s.FinishTask(ctx, wf.FinishRequest{
			TaskID: retryTask, Status: "COMPLETED", Verdict: "PASSED",
		}); err != nil {
			t.Fatalf("FinishTask retry: %v", err)
		}
		runs, err = s.RecentRuns(ctx, 10)
		if err != nil {
			t.Fatalf("RecentRuns after retry: %v", err)
		}
		for _, r := range runs {
			if r.Variant == v1 && r.Verdict != "PASSED" {
				t.Errorf("retry 后 %s verdict = %q, want PASSED(应取最新一条)", v1, r.Verdict)
			}
		}

		// 对抗性项目:与 proj 等长,仅在下划线位置替换为普通字符
		// (Algo_Super_SDK → AlgoXSuper_SDK)。若查询实现从 starts_with 退化为
		// LIKE,base 拼出的模式里那个下划线会退化成单字符通配符,adversary 的
		// 变体级 workflow 就会被误判为 proj 的前缀匹配,顶掉 proj/v1 刚判定
		// 出的 PASSED。
		const advProj = "AlgoXSuper_SDK"
		if len(advProj) != len(proj) {
			t.Fatalf("adversary 项目名长度必须与 proj 一致才能对齐下划线位置: %d vs %d", len(advProj), len(proj))
		}
		advBase := wf.BaseWorkflowID(advProj, sha, iid)
		// 复用与 proj 完全相同的 (commit, pipeline, variant) 三元组注册;
		// project 已纳入 artifact identity,两个项目都必须保留独立行和结论。
		if err := s.RegisterArtifacts(ctx, []Artifact{
			{Project: advProj, CommitSHA: sha, PipelineID: iid, Variant: v1, URL: "adv", SHA256: "adv"},
		}); err != nil {
			t.Fatalf("RegisterArtifacts adversary: %v", err)
		}
		advWF := advBase + "-" + v1
		advTask := advWF + ":" + v1 + ":a1"
		if err := s.CreateTask(ctx, wf.TaskRow{
			TaskID: advTask, WorkflowID: advWF, TestID: v1, Attempt: 1,
			IdempotencyKey: advTask, Status: "RUNNING",
		}); err != nil {
			t.Fatalf("CreateTask adversary: %v", err)
		}
		if err := s.FinishTask(ctx, wf.FinishRequest{
			TaskID: advTask, Status: "COMPLETED", Verdict: "INFRA_ERROR",
		}); err != nil {
			t.Fatalf("FinishTask adversary: %v", err)
		}
		runs, err = s.RecentRuns(ctx, 10)
		if err != nil {
			t.Fatalf("RecentRuns after adversary: %v", err)
		}
		byProjectVariant := map[string]RecentRun{}
		for _, r := range runs {
			byProjectVariant[r.Project+"|"+r.Variant] = r
		}
		if got := byProjectVariant[proj+"|"+v1].Verdict; got != "PASSED" {
			t.Errorf("%s verdict = %q, want PASSED(不得被下划线位置不同的对抗项目 %s 抢走结论)", v1, got, advProj)
		}
		if got := byProjectVariant[advProj+"|"+v1].Verdict; got != "INFRA_ERROR" {
			t.Errorf("%s/%s verdict = %q, want INFRA_ERROR", advProj, v1, got)
		}
	})

	t.Run("RecentRunsRespectsLimit", func(t *testing.T) {
		s := newStore(t)
		arts := []Artifact{}
		for i := 0; i < 5; i++ {
			arts = append(arts, Artifact{
				// 每个产物的 variant 各异,避免与 (commit_sha, pipeline_id, variant)
				// 唯一键无关地互相覆盖——同时也让"是哪一行"可以只凭 Commit 辨认。
				Project: "p", CommitSHA: fmt.Sprintf("sha%d", i), PipelineID: i + 1,
				Variant: fmt.Sprintf("v%d", i), URL: "u", SHA256: "s",
			})
		}
		if err := s.RegisterArtifacts(ctx, arts); err != nil {
			t.Fatalf("RegisterArtifacts: %v", err)
		}
		runs, err := s.RecentRuns(ctx, 3)
		if err != nil {
			t.Fatalf("RecentRuns: %v", err)
		}
		if len(runs) != 3 {
			t.Fatalf("len = %d, want 3", len(runs))
		}
		// 断言具体是"哪" 3 条、且新到旧排序——只查 count 时,从错误的一端截断
		// (返回最旧的 3 条,或返回正确数量但顺序颠倒)也能通过。
		wantCommits := []string{"sha4", "sha3", "sha2"} // 最后注册的 3 条,新→旧
		for i, want := range wantCommits {
			if runs[i].Commit != want {
				t.Errorf("runs[%d].Commit = %q, want %q(应为最近注册的 3 条,新→旧排序)",
					i, runs[i].Commit, want)
			}
		}
	})

	t.Run("RecentRunsAuthoritativeFirst", func(t *testing.T) {
		s := newStore(t)
		if err := s.RegisterArtifacts(ctx, []Artifact{{
			Project: "legacy/project", CommitSHA: "legacy", PipelineID: 1,
			Variant: "legacy-v", URL: "u", SHA256: "s",
		}}); err != nil {
			t.Fatalf("RegisterArtifacts: %v", err)
		}
		run := WorkflowRun{
			WorkflowID: "authoritative-run", Project: "grp/project",
			CommitSHA: "abcd1234", PipelineID: 42, Version: "1.2.3",
			RuleVersion: "rules-v7", Scope: "bundle", Attempt: 2,
			Variants: []string{"v2", "v1"},
		}
		if err := s.RecordWorkflowRun(ctx, run); err != nil {
			t.Fatalf("RecordWorkflowRun: %v", err)
		}

		got, err := s.RecentRuns(ctx, 3)
		if err != nil {
			t.Fatalf("RecentRuns: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("runs = %#v, want 3", got)
		}
		for i, variant := range []string{"v1", "v2"} {
			want := RecentRun{
				WorkflowID: "authoritative-run", Project: "grp/project",
				Commit: "abcd1234", PipelineID: 42, Version: "1.2.3",
				RuleVersion: "rules-v7", Variant: variant, Authoritative: true,
			}
			if !reflect.DeepEqual(got[i], want) {
				t.Errorf("runs[%d] = %#v, want %#v", i, got[i], want)
			}
		}
		if got[2].Project != "legacy/project" || got[2].Variant != "legacy-v" ||
			got[2].Authoritative || got[2].WorkflowID != "" || got[2].Version != "" ||
			got[2].RuleVersion != "" {
			t.Errorf("legacy fallback = %#v, want non-authoritative legacy row", got[2])
		}
	})

	t.Run("RecentRunsExpandsVariantsBeforeLimit", func(t *testing.T) {
		s := newStore(t)
		old := WorkflowRun{
			WorkflowID: "run-old", Project: "p", CommitSHA: "old", PipelineID: 1,
			Version: "1", RuleVersion: "r", Variants: []string{"old-v"},
		}
		newest := WorkflowRun{
			WorkflowID: "run-new", Project: "p", CommitSHA: "new", PipelineID: 2,
			Version: "2", RuleVersion: "r2",
			Variants: []string{"v4", "v2", "v1", "v3"},
		}
		for _, run := range []WorkflowRun{old, newest} {
			if err := s.RecordWorkflowRun(ctx, run); err != nil {
				t.Fatalf("RecordWorkflowRun %s: %v", run.WorkflowID, err)
			}
		}

		got, err := s.RecentRuns(ctx, 3)
		if err != nil {
			t.Fatalf("RecentRuns: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("runs = %#v, want 3", got)
		}
		for i, variant := range []string{"v1", "v2", "v3"} {
			if got[i].WorkflowID != newest.WorkflowID || got[i].Variant != variant ||
				!got[i].Authoritative {
				t.Errorf("runs[%d] = %#v, want newest/%s authoritative", i, got[i], variant)
			}
		}
	})

	t.Run("RecentRunsExactTaskAssociation", func(t *testing.T) {
		s := newStore(t)
		run := WorkflowRun{
			WorkflowID: "run-exact", Project: "p", CommitSHA: "sha", PipelineID: 7,
			Version: "1", RuleVersion: "r", Variants: []string{"v1", "v2"},
		}
		if err := s.RecordWorkflowRun(ctx, run); err != nil {
			t.Fatalf("RecordWorkflowRun: %v", err)
		}
		tasks := []wf.TaskRow{
			{TaskID: "exact-v1-a0", WorkflowID: run.WorkflowID, TestID: "v1", Attempt: 0, IdempotencyKey: "exact-v1-a0", Status: "RUNNING"},
			{TaskID: "exact-v1-a2", WorkflowID: run.WorkflowID, TestID: "v1", Attempt: 2, IdempotencyKey: "exact-v1-a2", Status: "RUNNING"},
			{TaskID: "exact-v2", WorkflowID: run.WorkflowID, TestID: "v2", Attempt: 0, IdempotencyKey: "exact-v2", Status: "RUNNING"},
			{TaskID: "prefix", WorkflowID: "prefix-" + run.WorkflowID, TestID: "v1", Attempt: 99, IdempotencyKey: "prefix", Status: "RUNNING"},
			{TaskID: "suffix", WorkflowID: run.WorkflowID + "-suffix", TestID: "v1", Attempt: 100, IdempotencyKey: "suffix", Status: "RUNNING"},
			{TaskID: "wrong-test", WorkflowID: run.WorkflowID, TestID: "v10", Attempt: 101, IdempotencyKey: "wrong-test", Status: "RUNNING"},
		}
		for _, row := range tasks {
			if err := s.CreateTask(ctx, row); err != nil {
				t.Fatalf("CreateTask %s: %v", row.TaskID, err)
			}
		}
		for taskID, verdict := range map[string]string{
			"exact-v1-a0": "TEST_FAILED",
			"exact-v1-a2": "PASSED",
			"prefix":      "INFRA_ERROR",
			"suffix":      "INFRA_ERROR",
			"wrong-test":  "INFRA_ERROR",
		} {
			if err := s.FinishTask(ctx, wf.FinishRequest{
				TaskID: taskID, Status: "COMPLETED", Verdict: verdict,
			}); err != nil {
				t.Fatalf("FinishTask %s: %v", taskID, err)
			}
		}

		got, err := s.RecentRuns(ctx, 10)
		if err != nil {
			t.Fatalf("RecentRuns: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("runs = %#v, want 2", got)
		}
		if got[0].Variant != "v1" || got[0].Verdict != "PASSED" ||
			got[0].EndedAt.IsZero() {
			t.Errorf("v1 = %#v, want exact workflow/test latest attempt PASSED", got[0])
		}
		if got[1].Variant != "v2" || got[1].Verdict != "" ||
			!got[1].EndedAt.IsZero() {
			t.Errorf("v2 = %#v, want running task without verdict/end", got[1])
		}
	})

	t.Run("RecentRunsLegacyFallbackAndGlobalDedup", func(t *testing.T) {
		s := newStore(t)
		run := WorkflowRun{
			WorkflowID: "registered-run", Project: "p", CommitSHA: "same", PipelineID: 1,
			Version: "1", RuleVersion: "r", Variants: []string{"v1"},
		}
		if err := s.RecordWorkflowRun(ctx, run); err != nil {
			t.Fatalf("RecordWorkflowRun: %v", err)
		}
		// The duplicate is deliberately newer than the unique fallback. Deduplication
		// must happen before the fallback LIMIT or it crowds the unique row out.
		if err := s.RegisterArtifacts(ctx, []Artifact{
			{Project: "legacy", CommitSHA: "unique", PipelineID: 2, Variant: "v2", URL: "u", SHA256: "s"},
			{Project: "p", CommitSHA: "same", PipelineID: 1, Variant: "v1", URL: "u", SHA256: "s"},
		}); err != nil {
			t.Fatalf("RegisterArtifacts: %v", err)
		}

		got, err := s.RecentRuns(ctx, 2)
		if err != nil {
			t.Fatalf("RecentRuns: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("runs = %#v, want authoritative plus unique fallback", got)
		}
		if !got[0].Authoritative || got[0].WorkflowID != run.WorkflowID {
			t.Errorf("runs[0] = %#v, want authoritative run", got[0])
		}
		if got[1].Authoritative || got[1].Commit != "unique" || got[1].Variant != "v2" {
			t.Errorf("runs[1] = %#v, want unique legacy fallback", got[1])
		}
	})

	t.Run("RecentRunsReturnsDefensiveCopies", func(t *testing.T) {
		s := newStore(t)
		run := WorkflowRun{
			WorkflowID: "defensive-run", Project: "p", CommitSHA: "sha", PipelineID: 1,
			Version: "1", RuleVersion: "r", Variants: []string{"v1"},
		}
		if err := s.RecordWorkflowRun(ctx, run); err != nil {
			t.Fatalf("RecordWorkflowRun: %v", err)
		}
		first, err := s.RecentRuns(ctx, 1)
		if err != nil {
			t.Fatalf("RecentRuns first: %v", err)
		}
		first[0].Project = "mutated"
		first[0].Verdict = "mutated"
		second, err := s.RecentRuns(ctx, 1)
		if err != nil {
			t.Fatalf("RecentRuns second: %v", err)
		}
		if second[0].Project != "p" || second[0].Verdict != "" {
			t.Fatalf("stored result mutated through caller slice: %#v", second[0])
		}
	})

	t.Run("AcceptWritesCompleteRetryRowInOneStatement", func(t *testing.T) {
		s := newStore(t)
		seedRunAndArtifacts(t, s, "wf1", "grp/p", "abcd1234", 42, "v1")
		tok := claimForTest(t, s, "e1", "wf1", "retry")

		var builtAttempt int
		req := acceptReq("e1", tok, "wf1", "retry")
		build := req.BuildTarget
		req.BuildTarget = func(attempt int) ([]byte, string, error) {
			builtAttempt = attempt
			return build(attempt)
		}
		out, err := s.CompleteAccept(ctx, req)
		if err != nil || out == nil || out.Kind != "accepted" {
			t.Fatalf("CompleteAccept = (%#v, %v)", out, err)
		}
		if builtAttempt != out.Attempt || out.Attempt != 1 {
			t.Fatalf("BuildTarget attempt=%d outcome=%#v", builtAttempt, out)
		}
		got := mustGetAction(t, s, "wf1")
		if got.Attempt != out.Attempt || got.TargetWorkflowID == "" || len(got.TargetInput) == 0 {
			t.Fatalf("retry 行必须落库即钉死: %#v", got)
		}
		if out.TargetWorkflowID != got.TargetWorkflowID ||
			!bytes.Equal(out.TargetInput, got.TargetInput) {
			t.Fatalf("accepted outcome pins=%#v, persisted=%#v", out, got)
		}
		out.TargetInput[0] ^= 0xff
		if again := mustGetAction(t, s, "wf1"); !bytes.Equal(again.TargetInput, got.TargetInput) {
			t.Fatal("mutating accepted outcome target_input changed persisted action")
		}
		var target wf.DeviceTestInput
		if err := json.Unmarshal(got.TargetInput, &target); err != nil {
			t.Fatalf("target_input: %v", err)
		}
		if target.Attempt != got.Attempt || target.WorkflowID() != got.TargetWorkflowID ||
			target.SourceWorkflowID != "wf1" || target.Version != "1.2.3" ||
			target.RuleVersion != "rules-v1" || len(target.Packages) != 1 {
			t.Fatalf("target_input 未完整钉死: %#v action=%#v", target, got)
		}
		if inbox := mustGetInbox(t, s, "e1"); inbox.State != "processed" || inbox.ProcessedAt == nil {
			t.Fatalf("inbox 未与接受事务一起完成: %#v", inbox)
		}
		if n := countAuditByAction(t, s, "card.retry.accepted"); n != 1 {
			t.Fatalf("accepted 审计=%d, want 1", n)
		}
		msg := mustGetActionMessage(t, s, "wf1", "om_shared")
		if msg.RenderKind != "action" || msg.DesiredRevision != 1 || msg.UpdateState != "pending" {
			t.Fatalf("message=%#v", msg)
		}
	})

	t.Run("AcceptBuildTargetFailureRollsBackEverything", func(t *testing.T) {
		s := newStore(t)
		seedRunAndArtifacts(t, s, "wf1", "grp/p", "abcd1234", 42, "v1")
		tok := claimForTest(t, s, "e1", "wf1", "retry")
		before := artifactAttempt(t, s, "grp/p")
		req := acceptReq("e1", tok, "wf1", "retry")
		req.BuildTarget = func(attempt int) ([]byte, string, error) {
			if attempt != before+1 {
				t.Fatalf("BuildTarget attempt=%d, want %d", attempt, before+1)
			}
			return nil, "", errors.New("target mismatch")
		}
		if out, err := s.CompleteAccept(ctx, req); err == nil || out != nil {
			t.Fatalf("CompleteAccept=(%#v,%v), want rollback error", out, err)
		}
		if artifactAttempt(t, s, "grp/p") != before {
			t.Fatal("BuildTarget 失败不得推进水位")
		}
		if actionExists(t, s, "wf1") || actionMessageExists(t, s, "wf1", "om_shared") ||
			countAudit(t, s, "e1") != 0 {
			t.Fatal("BuildTarget 失败不得留下 action/message/audit")
		}
		if inbox := mustGetInbox(t, s, "e1"); inbox.State != "received" {
			t.Fatalf("BuildTarget 失败不得处理 inbox: %#v", inbox)
		}

		req = acceptReq("e1", tok, "wf1", "retry")
		if out, err := s.CompleteAccept(ctx, req); err != nil || out.Kind != "accepted" {
			t.Fatalf("同一 live claim 应可重试完成: (%#v,%v)", out, err)
		}
	})

	t.Run("AcceptBuildTargetCannotOutliveInboxLease", func(t *testing.T) {
		s := newStore(t)
		seedRunAndArtifacts(t, s, "wf1", "grp/p", "abcd1234", 42, "v1")
		if _, inserted, err := s.PutInbox(ctx, InboxRow{
			EventID: "e1", Disposition: "accepted", AckToast: "received",
			Action: "retry", WorkflowID: "wf1", ActorOpenID: "ou_x",
			OpenMessageID: "om_shared", PayloadDigest: "digest-e1", State: "received",
		}, nil); err != nil || !inserted {
			t.Fatalf("PutInbox = (%v, %v)", inserted, err)
		}
		token := "short-lease"
		if _, err := s.ClaimInbox(ctx, "e1", token, 25*time.Millisecond); err != nil {
			t.Fatalf("ClaimInbox: %v", err)
		}
		request := acceptReq("e1", token, "wf1", "retry")
		build := request.BuildTarget
		request.BuildTarget = func(attempt int) ([]byte, string, error) {
			time.Sleep(75 * time.Millisecond)
			return build(attempt)
		}
		out, err := s.CompleteAccept(ctx, request)
		if err != nil || out == nil || out.Kind != "lost" {
			t.Fatalf("CompleteAccept = (%#v, %v), want lost", out, err)
		}
		if actionExists(t, s, "wf1") || actionMessageExists(t, s, "wf1", "om_shared") ||
			countAudit(t, s, "e1") != 0 || mustGetInbox(t, s, "e1").State != "received" {
			t.Fatal("expired BuildTarget completion left business writes")
		}
		if got := artifactAttempt(t, s, "grp/p"); got != 0 {
			t.Fatalf("expired BuildTarget advanced waterline to %d", got)
		}
	})

	t.Run("AcceptIsSerializedAndBumpsWaterlineOnce", func(t *testing.T) {
		s := newStore(t)
		seedRunAndArtifacts(t, s, "wf1", "grp/p", "abcd1234", 42, "v1", "v2")
		before := artifactAttempt(t, s, "grp/p")

		const n = 8
		toks := make([]string, n)
		for i := range n {
			toks[i] = claimForTest(t, s, fmt.Sprintf("e%d", i), "wf1", "retry")
		}

		kinds := make(chan string, n)
		errs := make(chan error, n)
		var builds atomic.Int32
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				req := acceptReq(fmt.Sprintf("e%d", i), toks[i], "wf1", "retry")
				build := req.BuildTarget
				req.BuildTarget = func(attempt int) ([]byte, string, error) {
					builds.Add(1)
					return build(attempt)
				}
				out, err := s.CompleteAccept(ctx, req)
				if err != nil {
					errs <- err
					return
				}
				kinds <- out.Kind
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent accept: %v", err)
		}
		close(kinds)
		accepted, conflicts := 0, 0
		for kind := range kinds {
			switch kind {
			case "accepted":
				accepted++
			case "conflict":
				conflicts++
			default:
				t.Fatalf("unexpected outcome %q", kind)
			}
		}
		if accepted != 1 || conflicts != n-1 {
			t.Fatalf("accepted=%d conflicts=%d, want 1/%d", accepted, conflicts, n-1)
		}
		if diff := artifactAttempt(t, s, "grp/p") - before; diff != 1 {
			t.Fatalf("水位推进 %d 次, want 1", diff)
		}
		if builds.Load() != 1 {
			t.Fatalf("BuildTarget 调用=%d, want 1", builds.Load())
		}
		if countAuditByAction(t, s, "card.retry.accepted") != 1 ||
			countAuditByAction(t, s, "card.retry.rejected.conflict") != n-1 {
			t.Fatal("并发接受的 accepted/conflict 审计数量不对")
		}
	})

	t.Run("AcceptRequiresMatchingOwnerAndLiveLease", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			prepare func(fullStore, string)
			token   string
		}{
			{name: "owner mismatch", token: "wrong-token"},
			{name: "expired lease", token: "inbox-e1", prepare: func(s fullStore, _ string) {
				expireInboxLease(t, s, "e1")
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				s := newStore(t)
				seedRunAndArtifacts(t, s, "wf1", "grp/p", "abcd1234", 42, "v1")
				tok := claimForTest(t, s, "e1", "wf1", "retry")
				if tc.prepare != nil {
					tc.prepare(s, tok)
				}
				before := artifactAttempt(t, s, "grp/p")
				var built atomic.Int32
				req := acceptReq("e1", tc.token, "wf1", "retry")
				build := req.BuildTarget
				req.BuildTarget = func(attempt int) ([]byte, string, error) {
					built.Add(1)
					return build(attempt)
				}
				out, err := s.CompleteAccept(ctx, req)
				if err != nil || out == nil || out.Kind != "lost" {
					t.Fatalf("CompleteAccept=(%#v,%v), want lost", out, err)
				}
				if built.Load() != 0 || artifactAttempt(t, s, "grp/p") != before ||
					actionExists(t, s, "wf1") || actionMessageExists(t, s, "wf1", "om_shared") ||
					countAudit(t, s, "e1") != 0 {
					t.Fatal("失去 fencing 的 CompleteAccept 必须零业务写入")
				}
				if inbox := mustGetInbox(t, s, "e1"); inbox.State != "received" {
					t.Fatalf("inbox changed: %#v", inbox)
				}
			})
		}
	})

	t.Run("AcceptedEventReconsumptionWritesNoSecondAudit", func(t *testing.T) {
		s := newStore(t)
		seedRunAndArtifacts(t, s, "wf1", "grp/p", "abcd1234", 42, "v1")
		token := claimForTest(t, s, "e1", "wf1", "retry")
		request := acceptReq("e1", token, "wf1", "retry")
		var builds atomic.Int32
		build := request.BuildTarget
		request.BuildTarget = func(attempt int) ([]byte, string, error) {
			builds.Add(1)
			return build(attempt)
		}
		first, err := s.CompleteAccept(ctx, request)
		if err != nil || first == nil || first.Kind != "accepted" {
			t.Fatalf("first CompleteAccept = (%#v, %v)", first, err)
		}
		before := mustGetAction(t, s, "wf1")
		second, err := s.CompleteAccept(ctx, request)
		if err != nil || second == nil || second.Kind != "lost" {
			t.Fatalf("second CompleteAccept = (%#v, %v), want lost", second, err)
		}
		if builds.Load() != 1 || countAudit(t, s, "e1") != 1 {
			t.Fatalf("reconsumption builds=%d audits=%d, want 1/1", builds.Load(), countAudit(t, s, "e1"))
		}
		if after := mustGetAction(t, s, "wf1"); !reflect.DeepEqual(after, before) {
			t.Fatalf("reconsumption changed action: before=%#v after=%#v", before, after)
		}
	})

	t.Run("AcceptLegacyWritesOnlyRejection", func(t *testing.T) {
		s := newStore(t)
		tok := claimForTest(t, s, "e1", "legacy-wf", "retry")
		var built atomic.Int32
		req := acceptReq("e1", tok, "legacy-wf", "retry")
		req.BuildTarget = func(int) ([]byte, string, error) {
			built.Add(1)
			return nil, "", nil
		}
		out, err := s.CompleteAccept(ctx, req)
		if err != nil || out == nil || out.Kind != "legacy" {
			t.Fatalf("CompleteAccept=(%#v,%v), want legacy", out, err)
		}
		if built.Load() != 0 || actionExists(t, s, "legacy-wf") {
			t.Fatal("legacy 分支不得构造 target 或写 action")
		}
		msg := mustGetActionMessage(t, s, "legacy-wf", "om_shared")
		if msg.RenderKind != "rejection" || msg.RejectionReason == "" || msg.ButtonsMode != "none" {
			t.Fatalf("legacy message=%#v", msg)
		}
		if countAudit(t, s, "e1") != 1 || mustGetInbox(t, s, "e1").State != "processed" {
			t.Fatal("legacy 拒绝审计与 inbox 必须同事务完成")
		}
	})

	t.Run("AcceptedActionUpgradesPriorRejectionMessage", func(t *testing.T) {
		s := newStore(t)
		token := claimForTestMessage(t, s, "rejected", "wf1", "retry", "om_rejected")
		if err := s.CompleteReject(ctx, "rejected", token, RejectRender{
			Code: "StillRunning", RejectionReason: "still running",
		}); err != nil {
			t.Fatalf("CompleteReject: %v", err)
		}
		rejection := mustGetActionMessage(t, s, "wf1", "om_rejected")
		if rejection.RenderKind != "rejection" {
			t.Fatalf("initial message = %#v", rejection)
		}

		seedRunAndArtifacts(t, s, "wf1", "grp/p", "abcd1234", 42, "v1")
		token = claimForTestMessage(t, s, "accepted", "wf1", "retry", "om_accepted")
		out, err := s.CompleteAccept(
			ctx, acceptReqMessage("accepted", token, "wf1", "retry", "om_accepted"),
		)
		if err != nil || out == nil || out.Kind != "accepted" {
			t.Fatalf("CompleteAccept = (%#v, %v)", out, err)
		}
		for _, messageID := range []string{"om_rejected", "om_accepted"} {
			action := mustGetActionMessage(t, s, "wf1", messageID)
			if action.RenderKind != "action" || action.RejectionReason != "" ||
				action.ButtonsMode != "none" || action.DesiredRevision != 1 ||
				action.UpdateState != "pending" {
				t.Fatalf("%s was not upgraded to action: %#v", messageID, action)
			}
		}
	})

	t.Run("ActionRejectCompletesAtomicallyAndMapsButtons", func(t *testing.T) {
		cases := map[string]struct {
			buttons string
			suffix  string
		}{
			"StillRunning":     {buttons: "both", suffix: "still_running"},
			"ResultUnreadable": {buttons: "both", suffix: "result_unreadable"},
			"ArtifactMissing":  {buttons: "both", suffix: "artifact_missing"},
			"VariantNotMember": {buttons: "both", suffix: "variant_not_member"},
			"NotAuthoritative": {buttons: "none", suffix: "not_authoritative"},
			"NoFailedVariants": {buttons: "none", suffix: "no_failed_variants"},
		}
		for code, policy := range cases {
			t.Run(code, func(t *testing.T) {
				s := newStore(t)
				tok := claimForTest(t, s, "e1", "wf1", "retry")
				err := s.CompleteReject(ctx, "e1", tok, RejectRender{
					Code: code, RejectionReason: "rejected: " + code,
				})
				if err != nil {
					t.Fatalf("CompleteReject: %v", err)
				}
				msg := mustGetActionMessage(t, s, "wf1", "om_shared")
				if msg.RenderKind != "rejection" || msg.RejectionReason != "rejected: "+code ||
					msg.ButtonsMode != policy.buttons || msg.UpdateState != "pending" {
					t.Fatalf("message=%#v, want buttons=%q", msg, policy.buttons)
				}
				if err := s.CompleteReject(ctx, "e1", tok, RejectRender{
					Code: code, RejectionReason: "duplicate: " + code,
				}); err != nil {
					t.Fatalf("duplicate CompleteReject: %v", err)
				}
				if after := mustGetActionMessage(t, s, "wf1", "om_shared"); !reflect.DeepEqual(after, msg) {
					t.Fatalf("duplicate changed message: before=%#v after=%#v", msg, after)
				}
				if countAudit(t, s, "e1") != 1 {
					t.Fatal("CompleteReject 必须恰好写一行审计")
				}
				wantAudit := "card.retry.rejected." + policy.suffix
				if countAuditByAction(t, s, wantAudit) != 1 {
					t.Fatalf("audit action %q count != 1", wantAudit)
				}
				if inbox := mustGetInbox(t, s, "e1"); inbox.State != "processed" || inbox.ProcessedAt == nil {
					t.Fatalf("inbox=%#v", inbox)
				}
				if actionExists(t, s, "wf1") {
					t.Fatal("rejection must not create an action row")
				}
			})
		}
	})

	t.Run("ActionRejectRequiresMatchingOwnerAndLiveLease", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			token  string
			expire bool
		}{
			{name: "owner mismatch", token: "wrong-token"},
			{name: "expired lease", token: "inbox-e1", expire: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				s := newStore(t)
				claimForTest(t, s, "e1", "wf1", "retry")
				if tc.expire {
					expireInboxLease(t, s, "e1")
				}
				if err := s.CompleteReject(ctx, "e1", tc.token, RejectRender{
					Code: "StillRunning", RejectionReason: "still running",
				}); err != nil {
					t.Fatalf("CompleteReject: %v", err)
				}
				if actionMessageExists(t, s, "wf1", "om_shared") || countAudit(t, s, "e1") != 0 {
					t.Fatal("失去 fencing 的 CompleteReject 必须零业务写入")
				}
				if inbox := mustGetInbox(t, s, "e1"); inbox.State != "received" {
					t.Fatalf("inbox changed: %#v", inbox)
				}
			})
		}
	})

	t.Run("ActionRejectNeverOverwritesActionMessage", func(t *testing.T) {
		s := newStore(t)
		seedAcceptedRetry(t, s, "wf1", "tokA")
		before := mustGetActionMessage(t, s, "wf1", "om_shared")
		tok := claimForTest(t, s, "e2", "wf1", "retry")
		if err := s.CompleteReject(ctx, "e2", tok, RejectRender{
			Code: "StillRunning", RejectionReason: "stale rejection",
		}); err != nil {
			t.Fatalf("CompleteReject: %v", err)
		}
		after := mustGetActionMessage(t, s, "wf1", "om_shared")
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("rejection 覆盖了 action message: before=%#v after=%#v", before, after)
		}
		if countAudit(t, s, "e2") != 1 || mustGetInbox(t, s, "e2").State != "processed" {
			t.Fatal("保留 action message 时仍必须完成拒绝审计与 inbox")
		}
	})

	t.Run("ActionRejectNewMessageShowsExistingAction", func(t *testing.T) {
		s := newStore(t)
		seedAcceptedRetry(t, s, "wf1", "tokA")
		action := mustGetAction(t, s, "wf1")

		token := claimForTestMessage(t, s, "e2", "wf1", "retry", "om_new")
		if err := s.CompleteReject(ctx, "e2", token, RejectRender{
			Code: "StillRunning", RejectionReason: "stale rejection",
		}); err != nil {
			t.Fatalf("CompleteReject: %v", err)
		}
		message := mustGetActionMessage(t, s, "wf1", "om_new")
		if message.RenderKind != "action" || message.RejectionReason != "" ||
			message.ButtonsMode != "none" || message.DesiredRevision != action.Revision ||
			message.UpdateState != "pending" {
			t.Fatalf("new message did not converge to existing action: %#v", message)
		}
		if countAudit(t, s, "e2") != 1 || mustGetInbox(t, s, "e2").State != "processed" {
			t.Fatal("action convergence must still complete rejection audit and inbox")
		}
	})

	t.Run("FinalizeRequiresOwnerAndLiveLease", func(t *testing.T) {
		for _, state := range []string{"succeeded", "failed"} {
			t.Run(state, func(t *testing.T) {
				s := newStore(t)
				seedAcceptedRetry(t, s, "wf1", "tokA")
				before := mustGetAction(t, s, "wf1")

				ok, err := s.FinalizeAction(ctx, "wf1", "wrong-token", state, "must not land")
				if err != nil || ok {
					t.Fatalf("owner mismatch finalize=(%v,%v)", ok, err)
				}
				expireActionLease(t, s, "wf1")
				ok, err = s.FinalizeAction(ctx, "wf1", "tokA", state, "must not land")
				if err != nil || ok {
					t.Fatalf("expired finalize=(%v,%v)", ok, err)
				}
				got := mustGetAction(t, s, "wf1")
				before.LeaseExpiresAt = got.LeaseExpiresAt
				if !reflect.DeepEqual(got, before) {
					t.Fatalf("失败的 finalize 不得改动 action: before=%#v got=%#v", before, got)
				}
			})
		}
	})

	t.Run("FinalizeAdvancesRevisionAndReordersAllMessages", func(t *testing.T) {
		for _, state := range []string{"succeeded", "failed"} {
			t.Run(state, func(t *testing.T) {
				s := newStore(t)
				seedAcceptedRetry(t, s, "wf1", "tokA")
				tok := claimForTestMessage(t, s, "e2", "wf1", "retry", "om_second")
				out, err := s.CompleteAccept(
					ctx, acceptReqMessage("e2", tok, "wf1", "retry", "om_second"),
				)
				if err != nil || out.Kind != "conflict" {
					t.Fatalf("register second message=(%#v,%v)", out, err)
				}
				for _, messageID := range []string{"om_shared", "om_second"} {
					setMessageBusyForTest(t, s, "wf1", messageID)
				}
				lastErr := ""
				if state == "failed" {
					lastErr = "temporal down"
				}
				ok, err := s.FinalizeAction(ctx, "wf1", "tokA", state, lastErr)
				if err != nil || !ok {
					t.Fatalf("FinalizeAction=(%v,%v)", ok, err)
				}
				action := mustGetAction(t, s, "wf1")
				if action.State != state || action.Revision != 2 || action.LastError != lastErr ||
					action.Owner != "" || action.LeaseExpiresAt != nil {
					t.Fatalf("action=%#v", action)
				}
				for _, messageID := range []string{"om_shared", "om_second"} {
					msg := mustGetActionMessage(t, s, "wf1", messageID)
					if msg.DesiredRevision != 2 || msg.UpdateState != "pending" || msg.Owner != "" ||
						msg.LeaseExpiresAt != nil || msg.ReconcileAfter != nil {
						t.Fatalf("%s 未被重排: %#v", messageID, msg)
					}
				}
			})
		}
	})

	t.Run("FailedActionResumesReusingPins", func(t *testing.T) {
		s := newStore(t)
		seedAcceptedRetry(t, s, "wf1", "tokA")
		tok := claimForTestMessage(t, s, "conflict", "wf1", "retry", "om_second")
		if out, err := s.CompleteAccept(ctx,
			acceptReqMessage("conflict", tok, "wf1", "retry", "om_second")); err != nil ||
			out.Kind != "conflict" {
			t.Fatalf("register second message=(%#v,%v)", out, err)
		}
		if ok, err := s.FinalizeAction(ctx, "wf1", "tokA", "failed", "temporal down"); err != nil || !ok {
			t.Fatalf("seed failed action=(%v,%v)", ok, err)
		}
		before := mustGetAction(t, s, "wf1")
		waterBefore := artifactAttempt(t, s, "grp/p")
		for _, messageID := range []string{"om_shared", "om_second"} {
			setMessageBusyForTest(t, s, "wf1", messageID)
		}

		tok = claimForTestMessage(t, s, "e2", "wf1", "retry", "om_third")
		req := acceptReqMessage("e2", tok, "wf1", "retry", "om_third")
		req.ActionToken = "tokB"
		req.Project, req.CommitSHA, req.PipelineID = "", "", 0
		req.BuildTarget = func(int) ([]byte, string, error) {
			t.Fatal("resume 不得重新 BuildTarget")
			return nil, "", nil
		}
		out, err := s.CompleteAccept(ctx, req)
		if err != nil || out == nil || out.Kind != "resumed" {
			t.Fatalf("CompleteAccept=(%#v,%v), want resumed", out, err)
		}
		if out.TargetWorkflowID != before.TargetWorkflowID ||
			!bytes.Equal(out.TargetInput, before.TargetInput) {
			t.Fatalf("resumed outcome pins=%#v, persisted=%#v", out, before)
		}
		after := mustGetAction(t, s, "wf1")
		if after.Attempt != before.Attempt || after.TargetWorkflowID != before.TargetWorkflowID ||
			!bytes.Equal(after.TargetInput, before.TargetInput) {
			t.Fatalf("resume 必须复用原 pins: before=%#v after=%#v", before, after)
		}
		if after.State != "pending" || after.Revision != before.Revision+1 ||
			after.Owner != "tokB" || after.LeaseExpiresAt == nil || after.LastError != "" {
			t.Fatalf("resume state=%#v", after)
		}
		if artifactAttempt(t, s, "grp/p") != waterBefore {
			t.Fatal("resume 不得推进水位")
		}
		if countAuditByAction(t, s, "card.retry.accepted") != 1 ||
			countAuditByAction(t, s, "card.retry.resumed") != 1 {
			t.Fatal("resume 不得重复 accepted 审计,且必须写 resumed 审计")
		}
		out.TargetInput[0] ^= 0xff
		if again := mustGetAction(t, s, "wf1"); !bytes.Equal(again.TargetInput, before.TargetInput) {
			t.Fatal("mutating resumed outcome target_input changed persisted action")
		}
		for _, messageID := range []string{"om_shared", "om_second", "om_third"} {
			msg := mustGetActionMessage(t, s, "wf1", messageID)
			if msg.RenderKind != "action" || msg.DesiredRevision != after.Revision ||
				msg.UpdateState != "pending" || msg.Owner != "" || msg.LeaseExpiresAt != nil ||
				msg.ReconcileAfter != nil {
				t.Fatalf("%s resume 后未重排: %#v", messageID, msg)
			}
		}
	})

	t.Run("ActionCannotChangeAfterAccept", func(t *testing.T) {
		s := newStore(t)
		seedAcceptedRetry(t, s, "wf1", "tokA")
		if ok, err := s.FinalizeAction(ctx, "wf1", "tokA", "failed", "boom"); err != nil || !ok {
			t.Fatalf("FinalizeAction=(%v,%v)", ok, err)
		}
		before := mustGetAction(t, s, "wf1")
		tok := claimForTestMessage(t, s, "e3", "wf1", "ignore", "om_ignore")
		out, err := s.CompleteAccept(ctx, acceptReqMessage("e3", tok, "wf1", "ignore", "om_ignore"))
		if err != nil || out == nil || out.Kind != "conflict" {
			t.Fatalf("异 action 必须 conflict, got (%#v,%v)", out, err)
		}
		after := mustGetAction(t, s, "wf1")
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("conflict 改动了既有 action: before=%#v after=%#v", before, after)
		}
		msg := mustGetActionMessage(t, s, "wf1", "om_ignore")
		if msg.RenderKind != "action" || msg.DesiredRevision != before.Revision {
			t.Fatalf("conflict message=%#v", msg)
		}
	})

	t.Run("IgnoreLandsTerminalWithoutPins", func(t *testing.T) {
		s := newStore(t)
		seedRunAndArtifacts(t, s, "wf1", "grp/p", "abcd1234", 42, "v1")
		tok := claimForTest(t, s, "e1", "wf1", "ignore")
		req := acceptReq("e1", tok, "wf1", "ignore")
		req.BuildTarget = func(int) ([]byte, string, error) {
			t.Fatal("ignore 不得 BuildTarget")
			return nil, "", nil
		}
		out, err := s.CompleteAccept(ctx, req)
		if err != nil || out == nil || out.Kind != "accepted" {
			t.Fatalf("ignore accept=(%#v,%v)", out, err)
		}
		got := mustGetAction(t, s, "wf1")
		if got.State != "succeeded" || got.Attempt != 0 || got.TargetWorkflowID != "" ||
			got.TargetInput != nil || got.Owner != "" || got.LeaseExpiresAt != nil {
			t.Fatalf("ignore 行必须终态且无 pins: %#v", got)
		}
		if out.ActionToken != "" || out.Attempt != 0 {
			t.Fatalf("ignore outcome=%#v", out)
		}
	})

	t.Run("RejectedInboxIsTerminalOnInsert", func(t *testing.T) {
		s := newStore(t)
		// rejected 行必须首次 INSERT 即为 processed。先插 received 再 UPDATE 会撞 23514。
		row := InboxRow{EventID: "e1", Disposition: "rejected", AckToast: "无权限",
			Action: "retry", WorkflowID: "wf1", ActorOpenID: "ou_x", State: "processed"}
		audit := &AuditRow{Actor: "feishu:ou_x", Action: "card.retry.rejected.unauthorized",
			Target: "wf1", InboxEventID: "e1"}
		if _, inserted, err := s.PutInbox(ctx, row, audit); err != nil || !inserted {
			t.Fatalf("PutInbox = (%v, %v)", inserted, err)
		}
		got := mustGetInbox(t, s, "e1")
		if got.State != "processed" || got.ProcessedAt == nil {
			t.Fatalf("rejected 行必须落库即终态: %#v", got)
		}
	})

	t.Run("RejectedInboxRollsBackWhenAuditFails", func(t *testing.T) {
		s := newStore(t)
		// 审计写不进去时整笔回滚:否则该 event 被永久当作"已处理",审计里却查无此人。
		//
		// 故障注入用**非法审计行**(actor 为空,撞 audit_log 的 CHECK),
		// 而不是在生产结构体上加 ForceFailForTest 之类的测试开关——
		// 那种接缝会永久留在生产类型里,而且 MemStore 与 PGStore 各写一份必然漂移。
		bad := &AuditRow{Actor: "", Action: "card.retry.rejected.unauthorized",
			Target: "wf1", InboxEventID: "e1"}
		if _, _, err := s.PutInbox(ctx, InboxRow{EventID: "e1", Disposition: "rejected",
			AckToast: "x", State: "processed"}, bad); err == nil {
			t.Fatal("审计失败时 PutInbox 必须返回错误")
		}
		if _, err := s.GetInbox(ctx, "e1"); !errors.Is(err, ErrInboxNotFound) {
			t.Fatalf("整笔必须回滚,inbox 不得留行: %v", err)
		}
	})

	t.Run("DuplicateRejectedEventReplaysToast", func(t *testing.T) {
		s := newStore(t)
		row := InboxRow{EventID: "e1", Disposition: "rejected", AckToast: "按钮已停用",
			Action: "retry", WorkflowID: "wf1", State: "processed"}
		audit := &AuditRow{Actor: "feishu:ou_x", Action: "card.retry.rejected.disabled",
			Target: "wf1", InboxEventID: "e1"}
		for i := 0; i < 3; i++ {
			existing, inserted, err := s.PutInbox(ctx, row, audit)
			if err != nil {
				t.Fatalf("第 %d 次: %v", i, err)
			}
			if i == 0 && !inserted {
				t.Fatal("首次必须插入")
			}
			if i > 0 {
				if inserted {
					t.Fatalf("第 %d 次不应插入", i)
				}
				if existing.AckToast != "按钮已停用" {
					t.Fatalf("toast 必须原样重放,got %q", existing.AckToast)
				}
			}
		}
		if n := countAudit(t, s, "e1"); n != 1 {
			t.Fatalf("审计行数 = %d, want 1", n)
		}
	})

	t.Run("ClaimInboxTakesLeaseOnce", func(t *testing.T) {
		s := newStore(t)
		_, _, _ = s.PutInbox(ctx, InboxRow{EventID: "e1", Disposition: "accepted",
			AckToast: "已收到，正在处理", Action: "retry", WorkflowID: "wf1",
			ActorOpenID: "ou_x", OpenMessageID: "om_1", State: "received"}, nil)

		got, err := s.ClaimInbox(ctx, "e1", "tokA", 120*time.Second)
		if err != nil || got == nil {
			t.Fatalf("首次 claim 失败: %v", err)
		}
		// 租约未过期时第二个 worker 抢不到
		if _, err := s.ClaimInbox(ctx, "e1", "tokB", 120*time.Second); !errors.Is(err, ErrInboxNotClaimable) {
			t.Fatalf("租约有效期内第二次 claim 应失败, got %v", err)
		}
	})

	t.Run("MessageSweepPredicateAcceptsNullLease", func(t *testing.T) {
		s := newStore(t)
		seedAcceptedRetry(t, s, "wf1", "tokA")

		claim, err := s.ClaimMessage(ctx, "tokM", 120*time.Second)
		if err != nil || claim == nil {
			t.Fatalf("NULL 租约的 message 必须可 claim: (%#v, %v)", claim, err)
		}
		if claim.WorkflowID != "wf1" || claim.OpenMessageID != "om_shared" ||
			claim.RenderKind != "action" || claim.DesiredRevision != 1 ||
			claim.Action == nil || claim.Action.Revision != claim.DesiredRevision {
			t.Fatalf("message claim 未钉住一致的 action revision: %#v", claim)
		}
		if msg := mustGetActionMessage(t, s, "wf1", "om_shared"); msg.Attempts != 1 ||
			msg.Owner != "tokM" || msg.LeaseExpiresAt == nil {
			t.Fatalf("claim 未写 owner/lease/attempts: %#v", msg)
		}
	})

	t.Run("MessageCompletionNeedsOwnerLeaseAndRevision", func(t *testing.T) {
		s := newStore(t)
		seedAcceptedRetry(t, s, "wf1", "tokA")
		claim, err := s.ClaimMessage(ctx, "tokM", 120*time.Second)
		if err != nil || claim == nil {
			t.Fatalf("ClaimMessage: (%#v, %v)", claim, err)
		}
		if ok, err := s.FinalizeAction(ctx, "wf1", "tokA", "succeeded", ""); err != nil || !ok {
			t.Fatalf("FinalizeAction: (%v, %v)", ok, err)
		}

		ok, err := s.CompleteMessageRender(ctx, *claim, "tokM")
		if err != nil || ok {
			t.Fatalf("旧 revision completion = (%v, %v), want false", ok, err)
		}
		msg := mustGetActionMessage(t, s, "wf1", "om_shared")
		if msg.UpdateState != "pending" || msg.DesiredRevision != 2 ||
			msg.RenderedRevision != 0 {
			t.Fatalf("rev2 必须留在 pending 等待 sweep: %#v", msg)
		}
		next, err := s.ClaimMessage(ctx, "tokN", 120*time.Second)
		if err != nil || next == nil || next.DesiredRevision != 2 ||
			next.Action == nil || next.Action.Revision != 2 {
			t.Fatalf("rev2 未被下一轮一致 claim: (%#v, %v)", next, err)
		}
	})

	t.Run("MessageRenderOutcomesUpdateClaimedRow", func(t *testing.T) {
		t.Run("succeeded", func(t *testing.T) {
			s := newStore(t)
			seedAcceptedRetry(t, s, "wf1", "tokA")
			claim, _ := s.ClaimMessage(ctx, "tokM", 120*time.Second)
			if ok, err := s.CompleteMessageRender(ctx, *claim, "tokM"); err != nil || !ok {
				t.Fatalf("CompleteMessageRender = (%v, %v)", ok, err)
			}
			msg := mustGetActionMessage(t, s, "wf1", "om_shared")
			if msg.UpdateState != "succeeded" || msg.RenderedRevision != 1 ||
				msg.Owner != "" || msg.LeaseExpiresAt != nil ||
				msg.ReconcileAfter != nil || msg.LastError != "" {
				t.Fatalf("successful render state = %#v", msg)
			}
		})

		t.Run("abandoned", func(t *testing.T) {
			s := newStore(t)
			seedAcceptedRetry(t, s, "wf1", "tokA")
			claim, _ := s.ClaimMessage(ctx, "tokM", 120*time.Second)
			if ok, err := s.AbandonMessageRender(
				ctx, *claim, "tokM", "message gone",
			); err != nil || !ok {
				t.Fatalf("AbandonMessageRender = (%v, %v)", ok, err)
			}
			msg := mustGetActionMessage(t, s, "wf1", "om_shared")
			if msg.UpdateState != "abandoned" || msg.RenderedRevision != 0 ||
				msg.LastError != "message gone" || msg.Owner != "" ||
				msg.LeaseExpiresAt != nil || msg.ReconcileAfter != nil {
				t.Fatalf("abandoned render state = %#v", msg)
			}
		})

		t.Run("rejection metadata", func(t *testing.T) {
			s := newStore(t)
			token := claimForTest(t, s, "e1", "wf1", "retry")
			if err := s.CompleteReject(ctx, "e1", token, RejectRender{
				Code: "StillRunning", RejectionReason: "still running",
			}); err != nil {
				t.Fatalf("CompleteReject: %v", err)
			}
			claim, err := s.ClaimMessage(ctx, "tokM", 120*time.Second)
			if err != nil || claim == nil || claim.RenderKind != "rejection" ||
				claim.RejectionReason != "still running" || claim.ButtonsMode != "both" ||
				claim.DesiredRevision != 1 || claim.Action != nil {
				t.Fatalf("rejection claim = (%#v, %v)", claim, err)
			}
		})
	})

	t.Run("LostLeaseWriterWritesNothing", func(t *testing.T) {
		calls := []struct {
			name string
			call func(fullStore, MessageClaim) (bool, error)
		}{
			{name: "succeeded", call: func(s fullStore, c MessageClaim) (bool, error) {
				return s.CompleteMessageRender(ctx, c, "tokM")
			}},
			{name: "timeout", call: func(s fullStore, c MessageClaim) (bool, error) {
				return s.DeferMessageRender(ctx, c, "tokM", time.Minute, "timeout")
			}},
			{name: "abandoned", call: func(s fullStore, c MessageClaim) (bool, error) {
				return s.AbandonMessageRender(ctx, c, "tokM", "gone")
			}},
		}
		for _, tc := range calls {
			t.Run(tc.name, func(t *testing.T) {
				s := newStore(t)
				seedAcceptedRetry(t, s, "wf1", "tokA")
				claim, err := s.ClaimMessage(ctx, "tokM", 120*time.Second)
				if err != nil || claim == nil {
					t.Fatalf("ClaimMessage: (%#v, %v)", claim, err)
				}
				expireMessageLease(t, s, "wf1", "om_shared")
				replacement, err := s.ClaimMessage(ctx, "tokN", 120*time.Second)
				if err != nil || replacement == nil {
					t.Fatalf("replacement ClaimMessage: (%#v, %v)", replacement, err)
				}
				before := mustGetActionMessage(t, s, "wf1", "om_shared")

				ok, err := tc.call(s, *claim)
				if err != nil || ok {
					t.Fatalf("expired writer = (%v, %v), want false", ok, err)
				}
				after := mustGetActionMessage(t, s, "wf1", "om_shared")
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("expired writer changed message:\nbefore=%#v\nafter=%#v", before, after)
				}
			})
		}
	})

	t.Run("ClaimLeasePointersAreDefensiveCopies", func(t *testing.T) {
		s := newStore(t)
		seedAcceptedRetry(t, s, "wf1", "tokA")

		messageClaim, err := s.ClaimMessage(ctx, "tokM", 120*time.Second)
		if err != nil || messageClaim == nil || messageClaim.Action == nil ||
			messageClaim.Action.LeaseExpiresAt == nil {
			t.Fatalf("ClaimMessage = (%#v, %v)", messageClaim, err)
		}
		storedActionLease := *mustGetAction(t, s, "wf1").LeaseExpiresAt
		*messageClaim.Action.LeaseExpiresAt = messageClaim.Action.LeaseExpiresAt.Add(24 * time.Hour)
		if got := mustGetAction(t, s, "wf1").LeaseExpiresAt; got == nil || !got.Equal(storedActionLease) {
			t.Fatalf("message claim mutated stored action lease: got=%v want=%v", got, storedActionLease)
		}

		expireActionLease(t, s, "wf1")
		actionClaim, err := s.ClaimStaleAction(ctx, "tokB", 120*time.Second)
		if err != nil || actionClaim == nil || actionClaim.LeaseExpiresAt == nil {
			t.Fatalf("ClaimStaleAction = (%#v, %v)", actionClaim, err)
		}
		storedActionLease = *mustGetAction(t, s, "wf1").LeaseExpiresAt
		*actionClaim.LeaseExpiresAt = actionClaim.LeaseExpiresAt.Add(24 * time.Hour)
		if got := mustGetAction(t, s, "wf1").LeaseExpiresAt; got == nil || !got.Equal(storedActionLease) {
			t.Fatalf("stale action claim mutated stored lease: got=%v want=%v", got, storedActionLease)
		}

		if _, inserted, err := s.PutInbox(ctx, InboxRow{
			EventID: "lease-copy", Disposition: "accepted", AckToast: "ok",
			Action: "retry", WorkflowID: "wf2", ActorOpenID: "ou_x",
			OpenMessageID: "om_2", State: "received",
		}, nil); err != nil || !inserted {
			t.Fatalf("PutInbox = (%v, %v)", inserted, err)
		}
		inboxClaim, err := s.ClaimStaleInbox(ctx, "inbox-owner", 120*time.Second)
		if err != nil || inboxClaim == nil || inboxClaim.LeaseExpiresAt == nil {
			t.Fatalf("ClaimStaleInbox = (%#v, %v)", inboxClaim, err)
		}
		storedInboxLease := *mustGetInbox(t, s, "lease-copy").LeaseExpiresAt
		*inboxClaim.LeaseExpiresAt = inboxClaim.LeaseExpiresAt.Add(24 * time.Hour)
		if got := mustGetInbox(t, s, "lease-copy").LeaseExpiresAt; got == nil || !got.Equal(storedInboxLease) {
			t.Fatalf("stale inbox claim mutated stored lease: got=%v want=%v", got, storedInboxLease)
		}
	})

	t.Run("ReconcileAfterDelaysNextClaim", func(t *testing.T) {
		s := newStore(t)
		seedAcceptedRetry(t, s, "wf1", "tokA")
		claim, err := s.ClaimMessage(ctx, "tokM", 120*time.Second)
		if err != nil || claim == nil {
			t.Fatalf("ClaimMessage: (%#v, %v)", claim, err)
		}
		before := time.Now().UTC()
		if ok, err := s.DeferMessageRender(
			ctx, *claim, "tokM", time.Minute, "patch timeout",
		); err != nil || !ok {
			t.Fatalf("DeferMessageRender = (%v, %v)", ok, err)
		}
		deferred := mustGetActionMessage(t, s, "wf1", "om_shared")
		if deferred.UpdateState != "pending" || deferred.ReconcileAfter == nil ||
			deferred.ReconcileAfter.Before(before.Add(55*time.Second)) ||
			deferred.ReconcileAfter.After(time.Now().UTC().Add(65*time.Second)) ||
			deferred.Owner != "" || deferred.LeaseExpiresAt != nil {
			t.Fatalf("timeout 未按 fresh wall time 延迟复核: %#v", deferred)
		}
		if got, err := s.ClaimMessage(ctx, "tokN", 120*time.Second); err != nil || got != nil {
			t.Fatalf("reconcile_after 未到期时 claim = (%#v, %v), want nil", got, err)
		}
		makeMessageReconcileDue(t, s, "wf1", "om_shared")
		if got, err := s.ClaimMessage(ctx, "tokN", 120*time.Second); err != nil || got == nil {
			t.Fatalf("reconcile_after 到期后必须可 claim: (%#v, %v)", got, err)
		}
	})

	t.Run("ReconcileRevisionReorderBecomesImmediatelyEligible", func(t *testing.T) {
		s := newStore(t)
		seedAcceptedRetry(t, s, "wf1", "tokA")
		claim, _ := s.ClaimMessage(ctx, "tokM", 120*time.Second)
		if ok, err := s.DeferMessageRender(
			ctx, *claim, "tokM", time.Hour, "patch timeout",
		); err != nil || !ok {
			t.Fatalf("DeferMessageRender = (%v, %v)", ok, err)
		}
		if ok, err := s.FinalizeAction(ctx, "wf1", "tokA", "succeeded", ""); err != nil || !ok {
			t.Fatalf("FinalizeAction = (%v, %v)", ok, err)
		}
		got, err := s.ClaimMessage(ctx, "tokN", 120*time.Second)
		if err != nil || got == nil || got.DesiredRevision != 2 {
			t.Fatalf("revision 重排必须清 defer 并立即可 claim: (%#v, %v)", got, err)
		}
	})

	t.Run("GetCardActionReturnsDefensiveCopyAndTypedNotFound", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.GetCardAction(ctx, "missing"); !errors.Is(err, ErrCardActionNotFound) {
			t.Fatalf("GetCardAction missing error = %v, want ErrCardActionNotFound", err)
		}
		seedAcceptedRetry(t, s, "wf1", "tokA")
		first, err := s.GetCardAction(ctx, "wf1")
		if err != nil || first == nil || first.LeaseExpiresAt == nil || len(first.TargetInput) == 0 {
			t.Fatalf("GetCardAction first = (%#v, %v)", first, err)
		}
		storedInput := append([]byte(nil), first.TargetInput...)
		storedLease := *first.LeaseExpiresAt
		first.TargetInput[0] ^= 0xff
		*first.LeaseExpiresAt = first.LeaseExpiresAt.Add(24 * time.Hour)

		again, err := s.GetCardAction(ctx, "wf1")
		if err != nil {
			t.Fatalf("GetCardAction again: %v", err)
		}
		if !bytes.Equal(again.TargetInput, storedInput) ||
			again.LeaseExpiresAt == nil || !again.LeaseExpiresAt.Equal(storedLease) {
			t.Fatalf("GetCardAction leaked caller mutation: %#v", again)
		}
	})

	t.Run("SnapshotRoundTripUsesDefensiveCopies", func(t *testing.T) {
		s := newStore(t)
		input := []byte(`{"config":{"wide_screen_mode":true,"update_multi":true}}`)
		var want any
		if err := json.Unmarshal(input, &want); err != nil {
			t.Fatal(err)
		}
		if err := s.PutCardSnapshot(ctx, "wf1", input); err != nil {
			t.Fatalf("PutCardSnapshot: %v", err)
		}
		input[0] = '['
		got, err := s.GetCardSnapshot(ctx, "wf1")
		var gotJSON any
		if err == nil {
			err = json.Unmarshal(got, &gotJSON)
		}
		if err != nil || !reflect.DeepEqual(gotJSON, want) {
			t.Fatalf("GetCardSnapshot = (%s, %v), want %#v", got, err, want)
		}
		got[0] = '['
		again, err := s.GetCardSnapshot(ctx, "wf1")
		var againJSON any
		if err == nil {
			err = json.Unmarshal(again, &againJSON)
		}
		if err != nil || !reflect.DeepEqual(againJSON, want) {
			t.Fatalf("snapshot leaked returned byte mutation: (%s, %v)", again, err)
		}
	})

	t.Run("StaleActionClaimIsExclusive", func(t *testing.T) {
		s := newStore(t)
		seedAcceptedRetry(t, s, "wf1", "tokA")
		expireActionLease(t, s, "wf1")

		first, err := s.ClaimStaleAction(ctx, "sweeper-a", 120*time.Second)
		if err != nil || first == nil || first.Owner != "sweeper-a" ||
			first.LeaseExpiresAt == nil {
			t.Fatalf("ClaimStaleAction first = (%#v, %v)", first, err)
		}
		second, err := s.ClaimStaleAction(ctx, "sweeper-b", 120*time.Second)
		if err != nil || second != nil {
			t.Fatalf("live action lease 被重复 claim: (%#v, %v)", second, err)
		}
	})

	t.Run("StaleActionNullLeaseIsClaimable", func(t *testing.T) {
		s := newStore(t)
		seedAcceptedRetry(t, s, "wf1", "tokA")
		clearActionLease(t, s, "wf1")
		claim, err := s.ClaimStaleAction(ctx, "sweeper-a", 120*time.Second)
		if err != nil || claim == nil || claim.Owner != "sweeper-a" {
			t.Fatalf("NULL action lease claim = (%#v, %v)", claim, err)
		}
	})

	t.Run("StaleInboxClaimIsExclusive", func(t *testing.T) {
		s := newStore(t)
		if _, inserted, err := s.PutInbox(ctx, InboxRow{
			EventID: "e1", Disposition: "accepted", AckToast: "ack",
			Action: "retry", WorkflowID: "wf1", ActorOpenID: "ou_x",
			OpenMessageID: "om_1", State: "received",
		}, nil); err != nil || !inserted {
			t.Fatalf("PutInbox = (%v, %v)", inserted, err)
		}

		first, err := s.ClaimStaleInbox(ctx, "sweeper-a", 120*time.Second)
		if err != nil || first == nil || first.Owner != "sweeper-a" ||
			first.Attempts != 1 || first.LeaseExpiresAt == nil {
			t.Fatalf("ClaimStaleInbox first = (%#v, %v)", first, err)
		}
		second, err := s.ClaimStaleInbox(ctx, "sweeper-b", 120*time.Second)
		if err != nil || second != nil {
			t.Fatalf("live inbox lease 被重复 claim: (%#v, %v)", second, err)
		}
		expireInboxLease(t, s, "e1")
		third, err := s.ClaimStaleInbox(ctx, "sweeper-c", 120*time.Second)
		if err != nil || third == nil || third.Owner != "sweeper-c" ||
			third.Attempts != 2 {
			t.Fatalf("expired inbox lease 未被接管: (%#v, %v)", third, err)
		}
	})

	t.Run("StaleInboxClaimPrioritizesNeverAttemptedRows", func(t *testing.T) {
		s := newStore(t)
		putInbox := func(eventID string) {
			t.Helper()
			if _, inserted, err := s.PutInbox(ctx, InboxRow{
				EventID: eventID, Disposition: "accepted", AckToast: "ack",
				Action: "retry", WorkflowID: "wf-" + eventID, ActorOpenID: "ou_x",
				OpenMessageID: "om-" + eventID, State: "received",
			}, nil); err != nil || !inserted {
				t.Fatalf("PutInbox(%s) = (%v, %v)", eventID, inserted, err)
			}
		}
		putInbox("e1")
		putInbox("e2")

		first, err := s.ClaimStaleInbox(ctx, "sweeper-a", 120*time.Second)
		if err != nil || first == nil || first.EventID != "e1" ||
			first.Owner != "sweeper-a" || first.Attempts != 1 ||
			first.LeaseExpiresAt == nil {
			t.Fatalf("first ClaimStaleInbox = (%#v, %v)", first, err)
		}
		expireInboxLease(t, s, "e1")

		second, err := s.ClaimStaleInbox(ctx, "sweeper-b", 120*time.Second)
		if err != nil || second == nil || second.EventID != "e2" ||
			second.Owner != "sweeper-b" || second.Attempts != 1 ||
			second.LeaseExpiresAt == nil {
			t.Fatalf("never-attempted inbox did not outrank expired retry: (%#v, %v)", second, err)
		}
		if firstStored := mustGetInbox(t, s, "e1"); firstStored.Attempts != 1 ||
			firstStored.Owner != "sweeper-a" || firstStored.LeaseExpiresAt == nil ||
			!firstStored.LeaseExpiresAt.Before(time.Now().UTC()) {
			t.Fatalf("skipped expired inbox changed during second claim: %#v", firstStored)
		}

		expireInboxLease(t, s, "e2")
		putInbox("e3")
		third, err := s.ClaimStaleInbox(ctx, "sweeper-c", 120*time.Second)
		if err != nil || third == nil || third.EventID != "e3" ||
			third.Owner != "sweeper-c" || third.Attempts != 1 ||
			third.LeaseExpiresAt == nil {
			t.Fatalf("new zero-attempt inbox did not outrank retries: (%#v, %v)", third, err)
		}

		expireInboxLease(t, s, "e3")
		retry, err := s.ClaimStaleInbox(ctx, "sweeper-d", 120*time.Second)
		if err != nil || retry == nil || retry.EventID != "e1" ||
			retry.Owner != "sweeper-d" || retry.Attempts != 2 ||
			retry.LeaseExpiresAt == nil {
			t.Fatalf("retry tie-break or claim mutation = (%#v, %v)", retry, err)
		}
		if secondStored := mustGetInbox(t, s, "e2"); secondStored.Attempts != 1 ||
			secondStored.Owner != "sweeper-b" {
			t.Fatalf("unselected retry changed: %#v", secondStored)
		}
	})
}
