package activity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"

	"hermes-devops/runtime/internal/cardaction"
	wf "hermes-devops/runtime/internal/workflow"
)

type snapshotWrite struct {
	workflowID string
	raw        []byte
}

type snapshotStore struct {
	Store
	err    error
	writes []snapshotWrite
	order  *[]string
}

func (s *snapshotStore) PutCardSnapshot(_ context.Context, workflowID string, raw []byte) error {
	if s.order != nil {
		*s.order = append(*s.order, "snapshot")
	}
	s.writes = append(s.writes, snapshotWrite{
		workflowID: workflowID,
		raw:        append([]byte(nil), raw...),
	})
	return s.err
}

type readinessDroppingSnapshotStore struct {
	snapshotStore
	readiness *cardaction.Readiness
}

func (s *readinessDroppingSnapshotStore) PutCardSnapshot(
	ctx context.Context,
	workflowID string,
	raw []byte,
) error {
	err := s.snapshotStore.PutCardSnapshot(ctx, workflowID, raw)
	s.readiness.SetWS(false)
	return err
}

type injectCardSender struct {
	fakeSender
	cards   []wf.NotificationCard
	cardErr error
	order   *[]string
}

func (s *injectCardSender) SendCard(_ context.Context, card any) error {
	if s.order != nil {
		*s.order = append(*s.order, "send")
	}
	typed, ok := card.(wf.NotificationCard)
	if !ok {
		return fmt.Errorf("SendCard got %T, want workflow.NotificationCard", card)
	}
	s.cards = append(s.cards, typed)
	return s.cardErr
}

func readyCardActions() *cardaction.Readiness {
	r := cardaction.NewReadiness(cardaction.ReadinessConfig{
		Enabled:           true,
		WhitelistNonEmpty: true,
		SenderIsApp:       true,
		HandlerWired:      true,
	})
	r.SetWS(true)
	return r
}

func readinessForTest(cfg cardaction.ReadinessConfig, ws bool) *cardaction.Readiness {
	r := cardaction.NewReadiness(cfg)
	r.SetWS(ws)
	return r
}

func activityWorkflowIDProbe(ctx context.Context) (string, error) {
	return activity.GetInfo(ctx).WorkflowExecution.ID, nil
}

// runNotifyCardInActivity executes both the workflow-ID probe and NotifyCard
// in the real Temporal activity testsuite context. SDK v1.46 exposes
// SetExecuteActivitiesInWorkflow to populate or clear WorkflowExecution.
func runNotifyCardInActivity(
	t *testing.T,
	acts *Acts,
	req wf.NotifyCardRequest,
	workflowActivity bool,
	dc converter.DataConverter,
) string {
	t.Helper()
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()
	env.SetExecuteActivitiesInWorkflow(workflowActivity)
	if dc != nil {
		env.SetDataConverter(dc)
	}
	env.RegisterActivity(activityWorkflowIDProbe)
	env.RegisterActivity(acts.NotifyCard)

	encodedID, err := env.ExecuteActivity(activityWorkflowIDProbe)
	if err != nil {
		t.Fatalf("probe activity workflow ID: %v", err)
	}
	var workflowID string
	if err := encodedID.Get(&workflowID); err != nil {
		t.Fatalf("decode probe workflow ID: %v", err)
	}
	if _, err := env.ExecuteActivity(acts.NotifyCard, req); err != nil {
		t.Fatalf("NotifyCard activity: %v", err)
	}
	return workflowID
}

func injectRequest(template string) wf.NotifyCardRequest {
	const visibleID = "device-test-visible-gnot-the-activity-p999"
	return wf.NotifyCardRequest{
		Card: wf.NotificationCard{
			Config: wf.CardConfig{WideScreenMode: true, UpdateMulti: true},
			Header: wf.CardHeader{
				Title:    wf.CardText{Tag: "plain_text", Content: visibleID},
				Template: template,
			},
			Elements: []wf.CardElement{{
				Tag: "div",
				Text: &wf.CardText{
					Tag:     "plain_text",
					Content: "body mentions " + visibleID,
				},
			}},
		},
		FallbackText: "fallback mentions " + visibleID,
	}
}

func expectedActionElement(workflowID string) wf.CardElement {
	return wf.CardElement{
		Tag: "action",
		Actions: []wf.CardButton{
			{
				Tag:  "button",
				Text: wf.CardText{Tag: "plain_text", Content: "重试失败变体"},
				Type: "primary",
				Value: wf.CardActionValue{
					Action:           "retry",
					SourceWorkflowID: workflowID,
				},
			},
			{
				Tag:  "button",
				Text: wf.CardText{Tag: "plain_text", Content: "忽略"},
				Type: "default",
				Value: wf.CardActionValue{
					Action:           "ignore",
					SourceWorkflowID: workflowID,
				},
			},
		},
	}
}

func trailingAction(t *testing.T, card wf.NotificationCard, workflowID string) wf.CardElement {
	t.Helper()
	var actionCount int
	for _, element := range card.Elements {
		if element.Tag == "action" {
			actionCount++
		}
	}
	if actionCount != 1 {
		t.Fatalf("action element count = %d, want 1; elements=%#v", actionCount, card.Elements)
	}
	got := card.Elements[len(card.Elements)-1]
	want := expectedActionElement(workflowID)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trailing action = %#v, want %#v", got, want)
	}
	return got
}

func hasAction(card wf.NotificationCard) bool {
	for _, element := range card.Elements {
		if element.Tag == "action" {
			return true
		}
	}
	return false
}

func TestInjectUsesActivityWorkflowID(t *testing.T) {
	req := injectRequest("red")
	st := &snapshotStore{}
	sender := &injectCardSender{}
	acts := &Acts{
		Store:       st,
		Feishu:      sender,
		CardActions: readyCardActions(),
	}

	workflowID := runNotifyCardInActivity(t, acts, req, true, nil)
	if workflowID == "" {
		t.Fatal("activity testsuite workflow ID must be nonempty")
	}
	if len(sender.cards) != 1 {
		t.Fatalf("SendCard calls = %d, want 1", len(sender.cards))
	}
	action := trailingAction(t, sender.cards[0], workflowID)
	for _, button := range action.Actions {
		if strings.Contains(req.Card.Header.Title.Content, button.Value.SourceWorkflowID) ||
			strings.Contains(req.FallbackText, button.Value.SourceWorkflowID) {
			t.Fatalf("source_workflow_id %q was scraped from visible text", button.Value.SourceWorkflowID)
		}
	}
}

func TestInjectEligibilityFromHeaderTemplate(t *testing.T) {
	tests := []struct {
		template string
		want     bool
	}{
		{template: "red", want: true},
		{template: "orange", want: true},
		{template: "green", want: false},
		{template: "", want: false},
		{template: "blue", want: false},
		{template: "RED", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.template, func(t *testing.T) {
			req := injectRequest(tt.template)
			st := &snapshotStore{}
			sender := &injectCardSender{}
			acts := &Acts{Store: st, Feishu: sender, CardActions: readyCardActions()}

			workflowID := runNotifyCardInActivity(t, acts, req, true, nil)
			if len(sender.cards) != 1 {
				t.Fatalf("SendCard calls = %d, want 1", len(sender.cards))
			}
			if got := hasAction(sender.cards[0]); got != tt.want {
				t.Fatalf("template %q action=%v, want %v", tt.template, got, tt.want)
			}
			if got := len(st.writes); got != btoi(tt.want) {
				t.Fatalf("template %q snapshot writes=%d, want %d", tt.template, got, btoi(tt.want))
			}
			if tt.want {
				trailingAction(t, sender.cards[0], workflowID)
			} else if !reflect.DeepEqual(sender.cards[0], req.Card) {
				t.Fatalf("template %q changed display card: got %#v want %#v", tt.template, sender.cards[0], req.Card)
			}
		})
	}
}

func btoi(v bool) int {
	if v {
		return 1
	}
	return 0
}

func TestInjectIgnoresBodyAndFallback(t *testing.T) {
	render := func(t *testing.T, req wf.NotifyCardRequest) wf.CardElement {
		t.Helper()
		st := &snapshotStore{}
		sender := &injectCardSender{}
		acts := &Acts{Store: st, Feishu: sender, CardActions: readyCardActions()}
		workflowID := runNotifyCardInActivity(t, acts, req, true, nil)
		return trailingAction(t, sender.cards[0], workflowID)
	}

	reqA := injectRequest("red")
	reqB := injectRequest("red")
	reqB.FallbackText = "completely different fallback device-test-other-g0-p0"
	reqB.Card.Elements[0].Text.Content = "completely different body"

	if a, b := render(t, reqA), render(t, reqB); !reflect.DeepEqual(a.Actions, b.Actions) {
		t.Fatalf("button injection depends on body or fallback:\nA=%#v\nB=%#v", a.Actions, b.Actions)
	}
}

func TestInjectEmptyWorkflowIDDoesNotSnapshotOrInject(t *testing.T) {
	req := injectRequest("red")
	st := &snapshotStore{}
	sender := &injectCardSender{}
	acts := &Acts{Store: st, Feishu: sender, CardActions: readyCardActions()}

	if workflowID := runNotifyCardInActivity(t, acts, req, false, nil); workflowID != "" {
		t.Fatalf("non-workflow activity ID = %q, want empty", workflowID)
	}
	if len(st.writes) != 0 {
		t.Fatalf("snapshot writes = %d, want 0", len(st.writes))
	}
	if len(sender.cards) != 1 || !reflect.DeepEqual(sender.cards[0], req.Card) {
		t.Fatalf("sent cards = %#v, want unchanged original", sender.cards)
	}
}

func TestInjectReadinessFactorsFailClosed(t *testing.T) {
	all := cardaction.ReadinessConfig{
		Enabled:           true,
		WhitelistNonEmpty: true,
		SenderIsApp:       true,
		HandlerWired:      true,
	}
	tests := []struct {
		name      string
		readiness *cardaction.Readiness
	}{
		{name: "nil"},
		{name: "disabled", readiness: readinessForTest(cardaction.ReadinessConfig{
			WhitelistNonEmpty: true, SenderIsApp: true, HandlerWired: true,
		}, true)},
		{name: "empty whitelist", readiness: readinessForTest(cardaction.ReadinessConfig{
			Enabled: true, SenderIsApp: true, HandlerWired: true,
		}, true)},
		{name: "webhook sender", readiness: readinessForTest(cardaction.ReadinessConfig{
			Enabled: true, WhitelistNonEmpty: true, HandlerWired: true,
		}, true)},
		{name: "handler unwired", readiness: readinessForTest(cardaction.ReadinessConfig{
			Enabled: true, WhitelistNonEmpty: true, SenderIsApp: true,
		}, true)},
		{name: "websocket down", readiness: readinessForTest(all, false)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.readiness != nil && tt.readiness.Ready() {
				t.Fatal("test setup must have Ready() false")
			}
			req := injectRequest("orange")
			st := &snapshotStore{}
			sender := &injectCardSender{}
			acts := &Acts{Store: st, Feishu: sender, CardActions: tt.readiness}

			runNotifyCardInActivity(t, acts, req, true, nil)
			if len(st.writes) != 0 {
				t.Fatalf("snapshot writes = %d, want 0", len(st.writes))
			}
			if len(sender.cards) != 1 || !reflect.DeepEqual(sender.cards[0], req.Card) {
				t.Fatalf("sent cards = %#v, want unchanged original", sender.cards)
			}
		})
	}
}

func TestSnapshotFailureShipsCardWithoutButtons(t *testing.T) {
	req := injectRequest("red")
	st := &snapshotStore{err: errors.New("db down")}
	sender := &injectCardSender{}
	acts := &Acts{Store: st, Feishu: sender, CardActions: readyCardActions()}

	runNotifyCardInActivity(t, acts, req, true, nil)
	if len(st.writes) != 1 {
		t.Fatalf("snapshot writes = %d, want 1", len(st.writes))
	}
	if len(sender.cards) != 1 || !reflect.DeepEqual(sender.cards[0], req.Card) {
		t.Fatalf("sent cards = %#v, want original without buttons", sender.cards)
	}
	if len(sender.texts) != 0 {
		t.Fatalf("fallback texts = %#v, want none", sender.texts)
	}
}

func TestSnapshotNilStoreShipsCardWithoutButtons(t *testing.T) {
	req := injectRequest("orange")
	sender := &injectCardSender{}
	acts := &Acts{Feishu: sender, CardActions: readyCardActions()}

	runNotifyCardInActivity(t, acts, req, true, nil)
	if len(sender.cards) != 1 || !reflect.DeepEqual(sender.cards[0], req.Card) {
		t.Fatalf("sent cards = %#v, want original without buttons", sender.cards)
	}
	if len(sender.texts) != 0 {
		t.Fatalf("fallback texts = %#v, want none", sender.texts)
	}
}

func TestSnapshotWrittenBeforeSendAndContainsOriginal(t *testing.T) {
	req := injectRequest("red")
	var order []string
	st := &snapshotStore{order: &order}
	sender := &injectCardSender{order: &order}
	acts := &Acts{Store: st, Feishu: sender, CardActions: readyCardActions()}

	workflowID := runNotifyCardInActivity(t, acts, req, true, nil)
	if !reflect.DeepEqual(order, []string{"snapshot", "send"}) {
		t.Fatalf("operation order = %v, want [snapshot send]", order)
	}
	if len(st.writes) != 1 {
		t.Fatalf("snapshot writes = %d, want 1", len(st.writes))
	}
	if st.writes[0].workflowID != workflowID {
		t.Fatalf("snapshot workflow ID = %q, want %q", st.writes[0].workflowID, workflowID)
	}
	wantRaw, err := json.Marshal(req.Card)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(st.writes[0].raw, wantRaw) {
		t.Fatalf("snapshot raw = %s, want exact original %s", st.writes[0].raw, wantRaw)
	}
	var snap wf.NotificationCard
	if err := json.Unmarshal(st.writes[0].raw, &snap); err != nil {
		t.Fatal(err)
	}
	if hasAction(snap) {
		t.Fatalf("snapshot contains injected action: %#v", snap.Elements)
	}
	trailingAction(t, sender.cards[0], workflowID)
}

func TestReadinessDropsDuringSnapshotShipsOriginalCardWithoutButtons(t *testing.T) {
	req := injectRequest("orange")
	originalRaw := marshalCard(t, req.Card)
	readiness := readyCardActions()
	if !readiness.Ready() {
		t.Fatal("test setup must start with all readiness factors true")
	}
	st := &readinessDroppingSnapshotStore{readiness: readiness}
	sender := &injectCardSender{}
	acts := &Acts{Store: st, Feishu: sender, CardActions: readiness}

	runNotifyCardInActivity(t, acts, req, true, nil)

	if readiness.Ready() {
		t.Fatal("snapshot store must drop WebSocket readiness before returning")
	}
	if len(st.writes) != 1 {
		t.Fatalf("snapshot writes = %d, want 1", len(st.writes))
	}
	if !bytes.Equal(st.writes[0].raw, originalRaw) {
		t.Fatalf("snapshot raw = %s, want exact original %s", st.writes[0].raw, originalRaw)
	}
	if len(sender.cards) != 1 {
		t.Fatalf("SendCard calls = %d, want 1", len(sender.cards))
	}
	if !reflect.DeepEqual(sender.cards[0], req.Card) || hasAction(sender.cards[0]) {
		t.Fatalf("sent card = %#v, want original without buttons %#v", sender.cards[0], req.Card)
	}
	if len(sender.texts) != 0 {
		t.Fatalf("fallback texts = %#v, want none", sender.texts)
	}
	if got := marshalCard(t, req.Card); !bytes.Equal(got, originalRaw) {
		t.Fatalf("caller card changed: got %s, want %s", got, originalRaw)
	}
}

func injectCardOfExactSize(t *testing.T, n int) wf.NotificationCard {
	t.Helper()
	makeCard := func(pad int) wf.NotificationCard {
		return wf.NotificationCard{
			Config: wf.CardConfig{WideScreenMode: true, UpdateMulti: true},
			Header: wf.CardHeader{
				Title:    wf.CardText{Tag: "plain_text", Content: "x"},
				Template: "red",
			},
			Elements: []wf.CardElement{{
				Tag: "div",
				Text: &wf.CardText{
					Tag:     "plain_text",
					Content: strings.Repeat("a", pad),
				},
			}},
		}
	}
	base := len(marshalCard(t, makeCard(0)))
	if n < base {
		t.Fatalf("target %d smaller than card skeleton %d", n, base)
	}
	card := makeCard(n - base)
	if got := len(marshalCard(t, card)); got != n {
		t.Fatalf("card size = %d, want %d", got, n)
	}
	return card
}

func TestInjectOverBudgetSendsOriginalCardBeforeTextFallback(t *testing.T) {
	t.Run("injected over budget but original fits", func(t *testing.T) {
		req := wf.NotifyCardRequest{
			Card:         injectCardOfExactSize(t, cardSizeBudget),
			FallbackText: "must not be used",
		}
		st := &snapshotStore{}
		sender := &injectCardSender{}
		acts := &Acts{Store: st, Feishu: sender, CardActions: readyCardActions()}

		runNotifyCardInActivity(t, acts, req, true, nil)
		if len(sender.cards) != 1 || !reflect.DeepEqual(sender.cards[0], req.Card) {
			t.Fatalf("sent cards = %#v, want original card without buttons", sender.cards)
		}
		if len(sender.texts) != 0 {
			t.Fatalf("fallback texts = %#v, want none", sender.texts)
		}
	})

	t.Run("original over budget still falls back to text", func(t *testing.T) {
		req := wf.NotifyCardRequest{
			Card:         injectCardOfExactSize(t, cardSizeBudget+1),
			FallbackText: "verbatim oversize fallback",
		}
		st := &snapshotStore{}
		sender := &injectCardSender{}
		acts := &Acts{Store: st, Feishu: sender, CardActions: readyCardActions()}

		runNotifyCardInActivity(t, acts, req, true, nil)
		if len(sender.cards) != 0 {
			t.Fatalf("SendCard calls = %d, want 0", len(sender.cards))
		}
		if !reflect.DeepEqual(sender.texts, []string{req.FallbackText}) {
			t.Fatalf("fallback texts = %#v, want %#v", sender.texts, []string{req.FallbackText})
		}
	})
}

// aliasDataConverter deliberately preserves the input slice backing array so
// the testsuite can detect an append into caller-owned spare capacity.
type aliasDataConverter struct {
	mu     sync.Mutex
	next   int
	values map[string]any
}

func newAliasDataConverter() *aliasDataConverter {
	return &aliasDataConverter{values: make(map[string]any)}
}

func (c *aliasDataConverter) ToPayload(value interface{}) (*commonpb.Payload, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	key := strconv.Itoa(c.next)
	c.values[key] = value
	return &commonpb.Payload{
		Metadata: map[string][]byte{"encoding": []byte("test/alias")},
		Data:     []byte(key),
	}, nil
}

func (c *aliasDataConverter) FromPayload(payload *commonpb.Payload, valuePtr interface{}) error {
	c.mu.Lock()
	value, ok := c.values[string(payload.Data)]
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("alias payload %q not found", payload.Data)
	}
	dst := reflect.ValueOf(valuePtr)
	if dst.Kind() != reflect.Pointer || dst.IsNil() {
		return fmt.Errorf("decode target must be a nonnil pointer, got %T", valuePtr)
	}
	src := reflect.ValueOf(value)
	if !src.IsValid() {
		dst.Elem().Set(reflect.Zero(dst.Elem().Type()))
		return nil
	}
	if !src.Type().AssignableTo(dst.Elem().Type()) {
		return fmt.Errorf("cannot assign %s to %s", src.Type(), dst.Elem().Type())
	}
	dst.Elem().Set(src)
	return nil
}

func (c *aliasDataConverter) ToPayloads(values ...interface{}) (*commonpb.Payloads, error) {
	payloads := make([]*commonpb.Payload, 0, len(values))
	for _, value := range values {
		payload, err := c.ToPayload(value)
		if err != nil {
			return nil, err
		}
		payloads = append(payloads, payload)
	}
	return &commonpb.Payloads{Payloads: payloads}, nil
}

func (c *aliasDataConverter) FromPayloads(payloads *commonpb.Payloads, valuePtrs ...interface{}) error {
	if len(payloads.GetPayloads()) != len(valuePtrs) {
		return fmt.Errorf("payload count %d does not match target count %d", len(payloads.GetPayloads()), len(valuePtrs))
	}
	for i, valuePtr := range valuePtrs {
		if err := c.FromPayload(payloads.Payloads[i], valuePtr); err != nil {
			return err
		}
	}
	return nil
}

func (c *aliasDataConverter) ToString(input *commonpb.Payload) string {
	return string(input.GetData())
}

func (c *aliasDataConverter) ToStrings(input *commonpb.Payloads) []string {
	out := make([]string, 0, len(input.GetPayloads()))
	for _, payload := range input.GetPayloads() {
		out = append(out, c.ToString(payload))
	}
	return out
}

func TestInjectDoesNotMutateCallerElementsWithSpareCapacity(t *testing.T) {
	req := injectRequest("red")
	backing := make([]wf.CardElement, 2, 2)
	backing[0] = req.Card.Elements[0]
	sentinel := wf.CardElement{
		Tag:  "div",
		Text: &wf.CardText{Tag: "plain_text", Content: "spare-capacity sentinel"},
	}
	backing[1] = sentinel
	req.Card.Elements = backing[:1]

	st := &snapshotStore{}
	sender := &injectCardSender{}
	acts := &Acts{Store: st, Feishu: sender, CardActions: readyCardActions()}
	workflowID := runNotifyCardInActivity(t, acts, req, true, newAliasDataConverter())

	if got := req.Card.Elements[:cap(req.Card.Elements)][1]; !reflect.DeepEqual(got, sentinel) {
		t.Fatalf("caller spare element mutated: got %#v want %#v", got, sentinel)
	}
	trailingAction(t, sender.cards[0], workflowID)
}

func TestInjectSendCardFailureUsesExactFallbackText(t *testing.T) {
	req := injectRequest("red")
	req.FallbackText = "exact fallback text — do not rewrite"
	st := &snapshotStore{}
	sender := &injectCardSender{cardErr: errors.New("card rejected")}
	acts := &Acts{Store: st, Feishu: sender, CardActions: readyCardActions()}

	workflowID := runNotifyCardInActivity(t, acts, req, true, nil)
	if len(sender.cards) != 1 {
		t.Fatalf("SendCard calls = %d, want 1", len(sender.cards))
	}
	trailingAction(t, sender.cards[0], workflowID)
	if !reflect.DeepEqual(sender.texts, []string{req.FallbackText}) {
		t.Fatalf("fallback texts = %#v, want %#v", sender.texts, []string{req.FallbackText})
	}
}
