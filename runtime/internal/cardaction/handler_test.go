package cardaction

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	"github.com/rs/zerolog"

	"hermes-devops/runtime/internal/store"
)

const (
	testAppID    = "cli_a"
	testTenant   = "tenant_a"
	testOpenID   = "ou_allowed"
	testWorkflow = "wf_source"
)

type fakeStore struct {
	rows     map[string]store.InboxRow
	audits   []store.AuditRow
	putCalls int
	put      func(context.Context, store.InboxRow, *store.AuditRow) (*store.InboxRow, bool, error)
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: make(map[string]store.InboxRow)}
}

func (s *fakeStore) PutInbox(
	ctx context.Context,
	row store.InboxRow,
	audit *store.AuditRow,
) (*store.InboxRow, bool, error) {
	s.putCalls++
	if s.put != nil {
		return s.put(ctx, row, audit)
	}
	if existing, ok := s.rows[row.EventID]; ok {
		copy := existing
		return &copy, false, nil
	}
	s.rows[row.EventID] = row
	if audit != nil {
		s.audits = append(s.audits, *audit)
	}
	copy := row
	return &copy, true, nil
}

func readyAll() *Readiness {
	r := NewReadiness(ReadinessConfig{
		Enabled:           true,
		WhitelistNonEmpty: true,
		SenderIsApp:       true,
		HandlerWired:      true,
	})
	r.SetWS(true)
	return r
}

func newHandler(s Store, readiness *Readiness) *Handler {
	return &Handler{
		Store:     s,
		Readiness: readiness,
		Whitelist: map[string]bool{testOpenID: true},
		AppID:     testAppID,
	}
}

func validEvent() *callback.CardActionTriggerEvent {
	tenant := testTenant
	return &callback.CardActionTriggerEvent{
		EventV2Base: &larkevent.EventV2Base{Header: &larkevent.EventHeader{
			EventID:   "evt_1",
			AppID:     testAppID,
			TenantKey: testTenant,
		}},
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{TenantKey: &tenant, OpenID: testOpenID},
			Action: &callback.CallBackAction{
				Tag: "button",
				Value: map[string]any{
					"action":             "retry",
					"source_workflow_id": testWorkflow,
				},
			},
			Host:    "im_message",
			Context: &callback.Context{OpenMessageID: "om_1"},
		},
	}
}

func toastContent(t *testing.T, resp *callback.CardActionTriggerResponse) string {
	t.Helper()
	if resp == nil || resp.Toast == nil {
		t.Fatal("response must contain a toast")
	}
	return resp.Toast.Content
}

func TestReadinessRequiresAllFiveFactors(t *testing.T) {
	tests := []struct {
		name string
		cfg  ReadinessConfig
		ws   bool
	}{
		{
			name: "all ready",
			cfg: ReadinessConfig{
				Enabled:           true,
				WhitelistNonEmpty: true,
				SenderIsApp:       true,
				HandlerWired:      true,
			},
			ws: true,
		},
		{
			name: "switch disabled",
			cfg: ReadinessConfig{
				WhitelistNonEmpty: true,
				SenderIsApp:       true,
				HandlerWired:      true,
			},
			ws: true,
		},
		{
			name: "whitelist empty",
			cfg: ReadinessConfig{
				Enabled:      true,
				SenderIsApp:  true,
				HandlerWired: true,
			},
			ws: true,
		},
		{
			name: "sender is webhook",
			cfg: ReadinessConfig{
				Enabled:           true,
				WhitelistNonEmpty: true,
				HandlerWired:      true,
			},
			ws: true,
		},
		{
			name: "handler not wired",
			cfg: ReadinessConfig{
				Enabled:           true,
				WhitelistNonEmpty: true,
				SenderIsApp:       true,
			},
			ws: true,
		},
		{
			name: "websocket down",
			cfg: ReadinessConfig{
				Enabled:           true,
				WhitelistNonEmpty: true,
				SenderIsApp:       true,
				HandlerWired:      true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewReadiness(tt.cfg)
			r.SetWS(tt.ws)
			if got, want := r.Ready(), tt.name == "all ready"; got != want {
				t.Fatalf("Ready() = %v, want %v", got, want)
			}
		})
	}

	var nilReadiness *Readiness
	if nilReadiness.Ready() {
		t.Fatal("nil readiness must fail closed")
	}
}

func TestReadinessSetWSTracksLifecycleAtomically(t *testing.T) {
	r := NewReadiness(ReadinessConfig{
		Enabled:           true,
		WhitelistNonEmpty: true,
		SenderIsApp:       true,
		HandlerWired:      true,
	})
	if r.Ready() {
		t.Fatal("websocket must start down")
	}
	r.SetWS(true)
	if !r.Ready() {
		t.Fatal("SetWS(true) must make an otherwise configured readiness ready")
	}
	r.SetWS(false)
	if r.Ready() {
		t.Fatal("SetWS(false) must fail readiness")
	}
}

func TestSyncRejectsFailClosedInValidationOrder(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*callback.CardActionTriggerEvent)
	}{
		{"nil event payload", func(ev *callback.CardActionTriggerEvent) { ev.Event = nil }},
		{"AppID mismatch", func(ev *callback.CardActionTriggerEvent) { ev.EventV2Base.Header.AppID = "cli_other" }},
		{"header tenant empty", func(ev *callback.CardActionTriggerEvent) { ev.EventV2Base.Header.TenantKey = "" }},
		{"nil operator", func(ev *callback.CardActionTriggerEvent) { ev.Event.Operator = nil }},
		{"operator tenant nil", func(ev *callback.CardActionTriggerEvent) { ev.Event.Operator.TenantKey = nil }},
		{"operator tenant empty", func(ev *callback.CardActionTriggerEvent) { empty := ""; ev.Event.Operator.TenantKey = &empty }},
		{"tenant mismatch", func(ev *callback.CardActionTriggerEvent) { other := "tenant_b"; ev.Event.Operator.TenantKey = &other }},
		{"nil action", func(ev *callback.CardActionTriggerEvent) { ev.Event.Action = nil }},
		{"tag not button", func(ev *callback.CardActionTriggerEvent) { ev.Event.Action.Tag = "select_static" }},
		{"nil context", func(ev *callback.CardActionTriggerEvent) { ev.Event.Context = nil }},
		{"open message empty", func(ev *callback.CardActionTriggerEvent) { ev.Event.Context.OpenMessageID = "" }},
		{"host not message", func(ev *callback.CardActionTriggerEvent) { ev.Event.Host = "im_top_notice" }},
		{"open ID empty", func(ev *callback.CardActionTriggerEvent) { ev.Event.Operator.OpenID = "" }},
		{"payload over four KiB", func(ev *callback.CardActionTriggerEvent) {
			ev.Event.Action.Value["source_workflow_id"] = strings.Repeat("w", 4097)
		}},
		{"nil value", func(ev *callback.CardActionTriggerEvent) { ev.Event.Action.Value = nil }},
		{"missing action key", func(ev *callback.CardActionTriggerEvent) { delete(ev.Event.Action.Value, "action") }},
		{"missing workflow key", func(ev *callback.CardActionTriggerEvent) { delete(ev.Event.Action.Value, "source_workflow_id") }},
		{"extra variant key", func(ev *callback.CardActionTriggerEvent) { ev.Event.Action.Value["variant"] = "android" }},
		{"extra arbitrary key", func(ev *callback.CardActionTriggerEvent) { ev.Event.Action.Value["extra"] = "x" }},
		{"action not string", func(ev *callback.CardActionTriggerEvent) { ev.Event.Action.Value["action"] = 1 }},
		{"unknown action", func(ev *callback.CardActionTriggerEvent) { ev.Event.Action.Value["action"] = "reboot" }},
		{"workflow not string", func(ev *callback.CardActionTriggerEvent) { ev.Event.Action.Value["source_workflow_id"] = 1 }},
		{"workflow empty", func(ev *callback.CardActionTriggerEvent) { ev.Event.Action.Value["source_workflow_id"] = "" }},
		{"payload cannot encode", func(ev *callback.CardActionTriggerEvent) {
			ev.Event.Action.Value["source_workflow_id"] = make(chan int)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newFakeStore()
			h := newHandler(st, readyAll())
			ev := validEvent()
			tt.mutate(ev)

			resp, err := h.OnCardAction(context.Background(), ev)
			if err != nil {
				t.Fatalf("malformed event returned error: %v", err)
			}
			if got := toastContent(t, resp); got != "请求无效" {
				t.Fatalf("toast = %q, want %q", got, "请求无效")
			}
			if st.putCalls != 1 {
				t.Fatalf("PutInbox calls = %d, want 1", st.putCalls)
			}
			row := st.rows["evt_1"]
			if row.Disposition != "rejected" || row.State != "processed" || row.ProcessedAt == nil {
				t.Fatalf("rejected row = %+v", row)
			}
			if len(st.audits) != 1 {
				t.Fatalf("audit rows = %d, want 1", len(st.audits))
			}
			audit := st.audits[0]
			if audit.Action != "card.unknown.rejected.payload" {
				t.Fatalf("audit action = %q", audit.Action)
			}
			if audit.InboxEventID != "evt_1" {
				t.Fatalf("audit event ID = %q", audit.InboxEventID)
			}
		})
	}
}

func TestPayloadValidationPrecedesReadinessAndWhitelist(t *testing.T) {
	st := newFakeStore()
	h := newHandler(st, NewReadiness(ReadinessConfig{}))
	h.Whitelist = nil
	ev := validEvent()
	ev.Event.Action.Tag = "select_static"

	resp, err := h.OnCardAction(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}
	if got := toastContent(t, resp); got != "请求无效" {
		t.Fatalf("toast = %q", got)
	}
	if got := st.audits[0].Action; got != "card.unknown.rejected.payload" {
		t.Fatalf("audit action = %q", got)
	}
}

func TestMalformedAuditPreservesObservableActorTargetAndDigest(t *testing.T) {
	st := newFakeStore()
	h := newHandler(st, readyAll())
	ev := validEvent()
	ev.Event.Action.Value["extra"] = "x"

	if _, err := h.OnCardAction(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	audit := st.audits[0]
	if audit.Actor != "feishu:"+testOpenID {
		t.Fatalf("audit actor = %q", audit.Actor)
	}
	if audit.Target != testWorkflow {
		t.Fatalf("audit target = %q", audit.Target)
	}
	sum := sha256.Sum256([]byte(
		`{"action":"retry","extra":"x","source_workflow_id":"wf_source"}`,
	))
	if want := hex.EncodeToString(sum[:]); audit.PayloadDigest != want {
		t.Fatalf("audit payload digest = %q, want %q", audit.PayloadDigest, want)
	}
}

func TestReadinessOffRejectsBeforeWhitelist(t *testing.T) {
	factors := []struct {
		name string
		cfg  ReadinessConfig
		ws   bool
	}{
		{"switch", ReadinessConfig{WhitelistNonEmpty: true, SenderIsApp: true, HandlerWired: true}, true},
		{"whitelist", ReadinessConfig{Enabled: true, SenderIsApp: true, HandlerWired: true}, true},
		{"mode", ReadinessConfig{Enabled: true, WhitelistNonEmpty: true, HandlerWired: true}, true},
		{"handler", ReadinessConfig{Enabled: true, WhitelistNonEmpty: true, SenderIsApp: true}, true},
		{"ws", ReadinessConfig{Enabled: true, WhitelistNonEmpty: true, SenderIsApp: true, HandlerWired: true}, false},
	}

	for _, factor := range factors {
		t.Run(factor.name, func(t *testing.T) {
			st := newFakeStore()
			r := NewReadiness(factor.cfg)
			r.SetWS(factor.ws)
			h := newHandler(st, r)
			h.Whitelist = map[string]bool{}

			resp, err := h.OnCardAction(context.Background(), validEvent())
			if err != nil {
				t.Fatal(err)
			}
			if got := toastContent(t, resp); got != "按钮已停用" {
				t.Fatalf("toast = %q", got)
			}
			row := st.rows["evt_1"]
			if row.Disposition != "rejected" || row.State != "processed" || row.ProcessedAt == nil {
				t.Fatalf("rejected row = %+v", row)
			}
			if got := st.audits[0].Action; got != "card.retry.rejected.disabled" {
				t.Fatalf("audit action = %q", got)
			}
		})
	}
}

func TestUnauthorizedIsRejectedWithSynchronousToast(t *testing.T) {
	st := newFakeStore()
	h := newHandler(st, readyAll())
	h.Whitelist = map[string]bool{"ou_other": true}

	resp, err := h.OnCardAction(context.Background(), validEvent())
	if err != nil {
		t.Fatal(err)
	}
	if got := toastContent(t, resp); got != "无权限" {
		t.Fatalf("toast = %q", got)
	}
	row := st.rows["evt_1"]
	if row.Disposition != "rejected" || row.ActorOpenID != testOpenID ||
		row.Action != "retry" || row.WorkflowID != testWorkflow {
		t.Fatalf("rejected row = %+v", row)
	}
	if got := st.audits[0].Action; got != "card.retry.rejected.unauthorized" {
		t.Fatalf("audit action = %q", got)
	}
}

func TestAcceptedEventPersistsCanonicalDataThenConsumesOnce(t *testing.T) {
	st := newFakeStore()
	consumed := make([]string, 0, 1)
	h := newHandler(st, readyAll())
	h.Consume = func(eventID string) {
		if _, ok := st.rows[eventID]; !ok {
			t.Fatal("Consume called before PutInbox completed")
		}
		consumed = append(consumed, eventID)
	}

	resp, err := h.OnCardAction(context.Background(), validEvent())
	if err != nil {
		t.Fatal(err)
	}
	if got := toastContent(t, resp); got != "已收到，正在处理" {
		t.Fatalf("toast = %q", got)
	}
	row := st.rows["evt_1"]
	if row.EventID != "evt_1" || row.Disposition != "accepted" || row.State != "received" ||
		row.Action != "retry" || row.WorkflowID != testWorkflow ||
		row.ActorOpenID != testOpenID || row.OpenMessageID != "om_1" ||
		row.ProcessedAt != nil {
		t.Fatalf("accepted row = %+v", row)
	}
	sum := sha256.Sum256([]byte(`{"action":"retry","source_workflow_id":"wf_source"}`))
	if want := hex.EncodeToString(sum[:]); row.PayloadDigest != want {
		t.Fatalf("payload digest = %q, want %q", row.PayloadDigest, want)
	}
	if len(st.audits) != 0 {
		t.Fatalf("accepted synchronous path wrote %d audits", len(st.audits))
	}
	if len(consumed) != 1 || consumed[0] != "evt_1" {
		t.Fatalf("consumed = %v", consumed)
	}

	st.rows["evt_1"] = store.InboxRow{
		EventID:     "evt_1",
		Disposition: "accepted",
		AckToast:    "首次精确应答",
		State:       "received",
	}
	resp, err = h.OnCardAction(context.Background(), validEvent())
	if err != nil {
		t.Fatal(err)
	}
	if got := toastContent(t, resp); got != "首次精确应答" {
		t.Fatalf("duplicate toast = %q", got)
	}
	if len(consumed) != 1 {
		t.Fatalf("duplicate consumed = %v", consumed)
	}
}

func TestAcceptedEventAllowsNilConsume(t *testing.T) {
	h := newHandler(newFakeStore(), readyAll())
	if resp, err := h.OnCardAction(context.Background(), validEvent()); err != nil ||
		toastContent(t, resp) != "已收到，正在处理" {
		t.Fatalf("response = %#v, error = %v", resp, err)
	}
}

func TestRejectedDuplicateReplaysToastAndAuditsOnce(t *testing.T) {
	st := newFakeStore()
	h := newHandler(st, readyAll())
	h.Whitelist = map[string]bool{}

	first, err := h.OnCardAction(context.Background(), validEvent())
	if err != nil {
		t.Fatal(err)
	}
	stored := toastContent(t, first)
	second, err := h.OnCardAction(context.Background(), validEvent())
	if err != nil {
		t.Fatal(err)
	}
	if got := toastContent(t, second); got != stored {
		t.Fatalf("duplicate toast = %q, want %q", got, stored)
	}
	if st.putCalls != 2 {
		t.Fatalf("PutInbox calls = %d, want 2", st.putCalls)
	}
	if len(st.audits) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(st.audits))
	}
}

func TestEventWithoutUsableEventIDRejectsAndLogsOnly(t *testing.T) {
	for _, eventID := range []string{"", "   "} {
		t.Run("event_id="+eventID, func(t *testing.T) {
			st := newFakeStore()
			var logs bytes.Buffer
			logger := zerolog.New(&logs)
			h := newHandler(st, readyAll())
			h.Log = &logger
			ev := validEvent()
			ev.EventV2Base.Header.EventID = eventID
			consumed := 0
			h.Consume = func(string) { consumed++ }

			resp, err := h.OnCardAction(context.Background(), ev)
			if err != nil {
				t.Fatal(err)
			}
			if got := toastContent(t, resp); got != "请求无效" {
				t.Fatalf("toast = %q", got)
			}
			if st.putCalls != 0 || len(st.audits) != 0 || consumed != 0 {
				t.Fatalf("put=%d audits=%d consumed=%d", st.putCalls, len(st.audits), consumed)
			}
			if !strings.Contains(logs.String(), "event_id") {
				t.Fatalf("missing event ID was not logged: %s", logs.String())
			}
		})
	}
}

func TestInboxWriteFailureReturnsErrorWithoutToastOrConsume(t *testing.T) {
	st := newFakeStore()
	st.put = func(context.Context, store.InboxRow, *store.AuditRow) (*store.InboxRow, bool, error) {
		return nil, false, errors.New("db down")
	}
	consumed := 0
	h := newHandler(st, readyAll())
	h.Consume = func(string) { consumed++ }

	resp, err := h.OnCardAction(context.Background(), validEvent())
	if err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("response = %#v, error = %v", resp, err)
	}
	if resp != nil {
		t.Fatalf("failure response = %#v, want nil", resp)
	}
	if consumed != 0 {
		t.Fatalf("Consume calls = %d", consumed)
	}
}

func TestHandlerUsesTwoSecondPersistenceDeadline(t *testing.T) {
	st := newFakeStore()
	st.put = func(ctx context.Context, _ store.InboxRow, _ *store.AuditRow) (*store.InboxRow, bool, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("PutInbox context has no deadline")
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || remaining > 2*time.Second {
			t.Fatalf("PutInbox deadline remaining = %s", remaining)
		}
		<-ctx.Done()
		return nil, false, ctx.Err()
	}
	h := newHandler(st, readyAll())

	start := time.Now()
	resp, err := h.OnCardAction(context.Background(), validEvent())
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("response = %#v, error = %v", resp, err)
	}
	if elapsed < 1800*time.Millisecond || elapsed > 2500*time.Millisecond {
		t.Fatalf("handler elapsed = %s, want about 2s", elapsed)
	}
}

func TestHandlerHonorsEarlierCallerCancellation(t *testing.T) {
	st := newFakeStore()
	st.put = func(ctx context.Context, _ store.InboxRow, _ *store.AuditRow) (*store.InboxRow, bool, error) {
		<-ctx.Done()
		return nil, false, ctx.Err()
	}
	h := newHandler(st, readyAll())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	resp, err := h.OnCardAction(ctx, validEvent())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("response = %#v, error = %v", resp, err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("canceled handler took %s", elapsed)
	}
}
