package cardaction

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"hermes-devops/runtime/internal/store"
)

func TestRenderPreservesSnapshotExceptTrailingActionModule(t *testing.T) {
	snapshot := []byte(`{
  "config": {"wide_screen_mode": true, "update_multi": true, "unknown": 7},
  "header": {"title": {"tag": "plain_text", "content": "original"}, "template": "red"},
  "elements": [
    {"tag":"div", "text":{"tag":"plain_text","content":"body"}, "unknown":{"b":2,"a":1}},
    {"tag":"hr", "unknown_order":[3,2,1]},
    {"tag":"action","actions":[{"tag":"button","text":{"tag":"plain_text","content":"old"},"type":"primary","value":{"action":"retry","source_workflow_id":"wf-source"}}]}
  ],
  "top_level_extension": {"keep": true}
}`)
	before := append([]byte(nil), snapshot...)

	out, err := RenderCard(snapshot, succeededRetryClaim("ou_x", "wf-target"))
	if err != nil {
		t.Fatalf("RenderCard: %v", err)
	}
	if !bytes.Equal(snapshot, before) {
		t.Fatal("RenderCard mutated the snapshot")
	}

	var original, rendered map[string]json.RawMessage
	mustUnmarshal(t, snapshot, &original)
	mustUnmarshal(t, out, &rendered)
	for _, field := range []string{"config", "header", "top_level_extension"} {
		if !jsonStructurallyEqual(original[field], rendered[field]) {
			t.Fatalf("%s changed:\noriginal=%s\nrendered=%s", field, original[field], rendered[field])
		}
	}

	originalElements := rawElements(t, snapshot)
	renderedElements := rawElements(t, out)
	if len(renderedElements) != len(originalElements) {
		t.Fatalf("rendered elements = %d, want %d", len(renderedElements), len(originalElements))
	}
	for i := 0; i < len(originalElements)-1; i++ {
		if !bytes.Equal(renderedElements[i], originalElements[i]) {
			t.Fatalf("element %d raw JSON changed:\noriginal=%s\nrendered=%s",
				i, originalElements[i], renderedElements[i])
		}
	}
	if tagOf(t, renderedElements[len(renderedElements)-1]) != "div" {
		t.Fatalf("trailing action was not replaced: %s", renderedElements[len(renderedElements)-1])
	}
	if got := statusText(t, out); got != "已由 ou_x 重试 → wf-target" {
		t.Fatalf("status = %q", got)
	}
}

func TestRenderAppendsWhenSnapshotHasNoTrailingAction(t *testing.T) {
	snapshot := minimalSnapshot()
	originalElements := rawElements(t, snapshot)

	out, err := RenderCard(snapshot, pendingRetryClaim("ou_x"))
	if err != nil {
		t.Fatalf("RenderCard: %v", err)
	}
	renderedElements := rawElements(t, out)
	if len(renderedElements) != len(originalElements)+1 {
		t.Fatalf("rendered elements = %d, want %d", len(renderedElements), len(originalElements)+1)
	}
	if !bytes.Equal(renderedElements[0], originalElements[0]) {
		t.Fatalf("existing element changed:\noriginal=%s\nrendered=%s",
			originalElements[0], renderedElements[0])
	}
}

func TestRenderStatusTexts(t *testing.T) {
	tests := []struct {
		name  string
		claim store.MessageClaim
		want  string
	}{
		{"retry pending", pendingRetryClaim("ou_x"), "已由 ou_x 重试，正在启动…"},
		{"retry succeeded", succeededRetryClaim("ou_x", "wf-target"), "已由 ou_x 重试 → wf-target"},
		{"retry failed", failedRetryClaim("ou_x", "temporal down"), "重试启动失败：temporal down"},
		{"ignore succeeded", succeededIgnoreClaim("ou_x"), "已由 ou_x 忽略（仅记录，不改变判定）"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := RenderCard(minimalSnapshot(), tt.claim)
			if err != nil {
				t.Fatalf("RenderCard: %v", err)
			}
			if got := statusText(t, out); got != tt.want {
				t.Fatalf("status = %q, want %q", got, tt.want)
			}
			status := renderedStatusElement(t, out)
			if !reflect.DeepEqual(status, map[string]any{
				"tag": "div",
				"text": map[string]any{
					"tag":     "plain_text",
					"content": tt.want,
				},
			}) {
				t.Fatalf("status element = %#v", status)
			}
		})
	}
}

func TestFailedRetryKeepsOnlyResumeButton(t *testing.T) {
	out, err := RenderCard(minimalSnapshot(), failedRetryClaim("ou_x", "boom"))
	if err != nil {
		t.Fatalf("RenderCard: %v", err)
	}
	buttons := renderedButtons(t, out)
	if len(buttons) != 1 {
		t.Fatalf("buttons = %d, want 1", len(buttons))
	}
	assertExactButton(t, buttons[0], "重新重试", "primary", "retry", "wf-source")
}

func TestRejectionButtonsMode(t *testing.T) {
	tests := []struct {
		name        string
		buttonsMode string
		wantButtons int
	}{
		{"both", "both", 2},
		{"none", "none", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claim := rejectionClaim("workflow 尚未结束", tt.buttonsMode)
			out, err := RenderCard(minimalSnapshotWithAction(), claim)
			if err != nil {
				t.Fatalf("RenderCard: %v", err)
			}
			if got := statusText(t, out); got != claim.RejectionReason {
				t.Fatalf("status = %q, want %q", got, claim.RejectionReason)
			}
			buttons := renderedButtons(t, out)
			if len(buttons) != tt.wantButtons {
				t.Fatalf("buttons = %d, want %d", len(buttons), tt.wantButtons)
			}
			if tt.buttonsMode == "both" {
				assertExactButton(t, buttons[0], "重试失败变体", "primary", "retry", "wf-source")
				assertExactButton(t, buttons[1], "忽略", "default", "ignore", "wf-source")
			}
		})
	}
}

func TestRenderRejectsOpenEndedButtonsMode(t *testing.T) {
	_, err := RenderCard(minimalSnapshot(), rejectionClaim("try later", "retry-only"))
	if err == nil || !strings.Contains(err.Error(), "buttons mode") {
		t.Fatalf("error = %v, want wrapped closed-enum error", err)
	}
}

func TestRenderOverBudgetReturnsIndependentOriginalCopy(t *testing.T) {
	large := strings.Repeat("x", 31*1024)
	snapshot := []byte(`{"config":{"update_multi":true},"header":{"title":{"tag":"plain_text","content":"h"}},` +
		`"elements":[{"tag":"div","text":{"tag":"plain_text","content":"` + large + `"}}]}`)
	before := append([]byte(nil), snapshot...)

	out, err := RenderCard(snapshot, pendingRetryClaim("ou_x"))
	if err != nil {
		t.Fatalf("RenderCard: %v", err)
	}
	if !bytes.Equal(out, before) {
		t.Fatal("over-budget render did not return byte-for-byte original snapshot")
	}
	if len(out) == 0 {
		t.Fatal("unexpected empty output")
	}
	out[0] ^= 0xff
	if !bytes.Equal(snapshot, before) {
		t.Fatal("over-budget output aliases the input snapshot")
	}
}

func TestRenderRejectsMalformedSnapshotAndClaim(t *testing.T) {
	validAction := pendingRetryClaim("ou_x")
	tests := []struct {
		name     string
		snapshot []byte
		claim    store.MessageClaim
	}{
		{"malformed JSON", []byte(`{"config":`), validAction},
		{"not an object", []byte(`[]`), validAction},
		{"missing config", []byte(`{"header":{},"elements":[]}`), validAction},
		{"config not object", []byte(`{"config":[],"header":{},"elements":[]}`), validAction},
		{"missing header", []byte(`{"config":{},"elements":[]}`), validAction},
		{"header not object", []byte(`{"config":{},"header":[],"elements":[]}`), validAction},
		{"missing elements", []byte(`{"config":{},"header":{}}`), validAction},
		{"elements not array", []byte(`{"config":{},"header":{},"elements":{}}`), validAction},
		{"element not object", []byte(`{"config":{},"header":{},"elements":[1]}`), validAction},
		{"unknown render kind", minimalSnapshot(), store.MessageClaim{
			WorkflowID: "wf-source", OpenMessageID: "om_1", RenderKind: "future",
			DesiredRevision: 1,
		}},
		{"action is nil", minimalSnapshot(), store.MessageClaim{
			WorkflowID: "wf-source", OpenMessageID: "om_1", RenderKind: "action",
			DesiredRevision: 1,
		}},
		{"action workflow mismatch", minimalSnapshot(), func() store.MessageClaim {
			c := pendingRetryClaim("ou_x")
			c.Action.WorkflowID = "wf-other"
			return c
		}()},
		{"action revision mismatch", minimalSnapshot(), func() store.MessageClaim {
			c := pendingRetryClaim("ou_x")
			c.Action.Revision = 2
			return c
		}()},
		{"unsupported action", minimalSnapshot(), actionClaim("quarantine", "succeeded", "ou_x", "", "")},
		{"unsupported retry state", minimalSnapshot(), actionClaim("retry", "future", "ou_x", "", "")},
		{"unsupported ignore state", minimalSnapshot(), actionClaim("ignore", "failed", "ou_x", "", "boom")},
		{"empty actor", minimalSnapshot(), pendingRetryClaim("")},
		{"succeeded retry target empty", minimalSnapshot(), succeededRetryClaim("ou_x", "")},
		{"failed retry error empty", minimalSnapshot(), failedRetryClaim("ou_x", "")},
		{"rejection reason empty", minimalSnapshot(), rejectionClaim("", "none")},
		{"rejection action present", minimalSnapshot(), func() store.MessageClaim {
			c := rejectionClaim("reason", "none")
			a := store.CardAction{WorkflowID: "wf-source"}
			c.Action = &a
			return c
		}()},
		{"identity empty", minimalSnapshot(), func() store.MessageClaim {
			c := rejectionClaim("reason", "none")
			c.WorkflowID = ""
			return c
		}()},
		{"revision not positive", minimalSnapshot(), func() store.MessageClaim {
			c := rejectionClaim("reason", "none")
			c.DesiredRevision = 0
			return c
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := append([]byte(nil), tt.snapshot...)
			out, err := RenderCard(tt.snapshot, tt.claim)
			if err == nil {
				t.Fatalf("RenderCard output = %s, want error", out)
			}
			if !strings.Contains(err.Error(), "render card") {
				t.Fatalf("error lacks render context: %v", err)
			}
			if out != nil {
				t.Fatalf("output = %q, want nil", out)
			}
			if !bytes.Equal(tt.snapshot, before) {
				t.Fatal("invalid render mutated snapshot")
			}
		})
	}
}

func TestRenderOutputDoesNotAliasSnapshot(t *testing.T) {
	snapshot := minimalSnapshot()
	before := append([]byte(nil), snapshot...)
	out, err := RenderCard(snapshot, pendingRetryClaim("ou_x"))
	if err != nil {
		t.Fatalf("RenderCard: %v", err)
	}
	out[0] ^= 0xff
	if !bytes.Equal(snapshot, before) {
		t.Fatal("render output aliases snapshot")
	}
}

func minimalSnapshot() []byte {
	return []byte(`{"config":{"wide_screen_mode":true,"update_multi":true},` +
		`"header":{"title":{"tag":"plain_text","content":"original"},"template":"red"},` +
		`"elements":[{"tag":"div", "text":{"tag":"plain_text","content":"body"}}]}`)
}

func minimalSnapshotWithAction() []byte {
	return []byte(`{"config":{"wide_screen_mode":true,"update_multi":true},` +
		`"header":{"title":{"tag":"plain_text","content":"original"},"template":"red"},` +
		`"elements":[{"tag":"div","text":{"tag":"plain_text","content":"body"}},` +
		`{"tag":"action","actions":[{"tag":"button","text":{"tag":"plain_text","content":"stale"},"type":"primary",` +
		`"value":{"action":"retry","source_workflow_id":"wf-source"}}]}]}`)
}

func actionClaim(action, state, actor, target, lastErr string) store.MessageClaim {
	a := &store.CardAction{
		WorkflowID: "wf-source", Action: action, ActorOpenID: actor, State: state,
		TargetWorkflowID: target, LastError: lastErr, Revision: 1,
	}
	return store.MessageClaim{
		WorkflowID: "wf-source", OpenMessageID: "om_1", RenderKind: "action",
		DesiredRevision: 1, Action: a,
	}
}

func pendingRetryClaim(actor string) store.MessageClaim {
	return actionClaim("retry", "pending", actor, "wf-target", "")
}

func succeededRetryClaim(actor, target string) store.MessageClaim {
	return actionClaim("retry", "succeeded", actor, target, "")
}

func failedRetryClaim(actor, lastErr string) store.MessageClaim {
	return actionClaim("retry", "failed", actor, "wf-target", lastErr)
}

func succeededIgnoreClaim(actor string) store.MessageClaim {
	return actionClaim("ignore", "succeeded", actor, "", "")
}

func rejectionClaim(reason, buttonsMode string) store.MessageClaim {
	return store.MessageClaim{
		WorkflowID: "wf-source", OpenMessageID: "om_1", RenderKind: "rejection",
		RejectionReason: reason, ButtonsMode: buttonsMode, DesiredRevision: 1,
	}
}

func rawElements(t *testing.T, card []byte) []json.RawMessage {
	t.Helper()
	var envelope struct {
		Elements []json.RawMessage `json:"elements"`
	}
	mustUnmarshal(t, card, &envelope)
	return envelope.Elements
}

func renderedStatusElement(t *testing.T, card []byte) map[string]any {
	t.Helper()
	elements := rawElements(t, card)
	for i := len(elements) - 1; i >= 0; i-- {
		if tagOf(t, elements[i]) == "div" {
			var element map[string]any
			mustUnmarshal(t, elements[i], &element)
			return element
		}
	}
	t.Fatal("rendered card has no status div")
	return nil
}

func statusText(t *testing.T, card []byte) string {
	t.Helper()
	element := renderedStatusElement(t, card)
	text, ok := element["text"].(map[string]any)
	if !ok {
		t.Fatalf("status text = %#v", element["text"])
	}
	content, ok := text["content"].(string)
	if !ok {
		t.Fatalf("status content = %#v", text["content"])
	}
	return content
}

func renderedButtons(t *testing.T, card []byte) []map[string]any {
	t.Helper()
	elements := rawElements(t, card)
	if len(elements) == 0 || tagOf(t, elements[len(elements)-1]) != "action" {
		return nil
	}
	var action struct {
		Actions []map[string]any `json:"actions"`
	}
	mustUnmarshal(t, elements[len(elements)-1], &action)
	return action.Actions
}

func assertExactButton(
	t *testing.T, button map[string]any, label, buttonType, action, workflowID string,
) {
	t.Helper()
	want := map[string]any{
		"tag":  "button",
		"text": map[string]any{"tag": "plain_text", "content": label},
		"type": buttonType,
		"value": map[string]any{
			"action":             action,
			"source_workflow_id": workflowID,
		},
	}
	if !reflect.DeepEqual(button, want) {
		t.Fatalf("button = %#v, want %#v", button, want)
	}
}

func tagOf(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var element struct {
		Tag string `json:"tag"`
	}
	mustUnmarshal(t, raw, &element)
	return element.Tag
}

func mustUnmarshal(t *testing.T, raw []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
}

func jsonStructurallyEqual(a, b []byte) bool {
	var av, bv any
	return json.Unmarshal(a, &av) == nil &&
		json.Unmarshal(b, &bv) == nil &&
		reflect.DeepEqual(av, bv)
}
