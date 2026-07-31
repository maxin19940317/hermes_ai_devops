package cardaction

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"hermes-devops/runtime/internal/store"
)

const cardJSONBudget = 30 * 1024

type jsonSpan struct {
	start int
	end   int
}

type renderedText struct {
	Tag     string `json:"tag"`
	Content string `json:"content"`
}

type renderedStatus struct {
	Tag  string       `json:"tag"`
	Text renderedText `json:"text"`
}

type renderedButtonValue struct {
	Action           string `json:"action"`
	SourceWorkflowID string `json:"source_workflow_id"`
}

type renderedButton struct {
	Tag   string              `json:"tag"`
	Text  renderedText        `json:"text"`
	Type  string              `json:"type"`
	Value renderedButtonValue `json:"value"`
}

type renderedActions struct {
	Tag     string           `json:"tag"`
	Actions []renderedButton `json:"actions"`
}

// RenderCard copies the authoritative display-card snapshot and replaces only
// its trailing action element with the status module represented by claim.
func RenderCard(snapshot []byte, claim store.MessageClaim) ([]byte, error) {
	status, buttons, err := renderClaim(claim)
	if err != nil {
		return nil, fmt.Errorf("render card: validate claim: %w", err)
	}
	fields, err := topLevelValueSpans(snapshot)
	if err != nil {
		return nil, fmt.Errorf("render card: parse snapshot: %w", err)
	}
	if err := validateFullCard(snapshot, fields); err != nil {
		return nil, fmt.Errorf("render card: validate snapshot: %w", err)
	}

	elementsSpan := fields["elements"]
	elements, err := arrayElementSpans(snapshot, elementsSpan)
	if err != nil {
		return nil, fmt.Errorf("render card: parse elements: %w", err)
	}
	if len(elements) > 0 {
		var trailing struct {
			Tag string `json:"tag"`
		}
		if err := json.Unmarshal(snapshot[elements[len(elements)-1].start:elements[len(elements)-1].end], &trailing); err != nil {
			return nil, fmt.Errorf("render card: decode trailing element: %w", err)
		}
		if trailing.Tag == "action" {
			elements = elements[:len(elements)-1]
		}
	}

	statusJSON, err := json.Marshal(renderedStatus{
		Tag:  "div",
		Text: renderedText{Tag: "plain_text", Content: status},
	})
	if err != nil {
		return nil, fmt.Errorf("render card: encode status: %w", err)
	}
	module := [][]byte{statusJSON}
	if len(buttons) > 0 {
		actionsJSON, err := json.Marshal(renderedActions{Tag: "action", Actions: buttons})
		if err != nil {
			return nil, fmt.Errorf("render card: encode buttons: %w", err)
		}
		module = append(module, actionsJSON)
	}

	var array bytes.Buffer
	array.WriteByte('[')
	wrote := false
	for _, span := range elements {
		if wrote {
			array.WriteByte(',')
		}
		array.Write(snapshot[span.start:span.end])
		wrote = true
	}
	for _, raw := range module {
		if wrote {
			array.WriteByte(',')
		}
		array.Write(raw)
		wrote = true
	}
	array.WriteByte(']')

	out := make([]byte, 0, len(snapshot)+array.Len()-(elementsSpan.end-elementsSpan.start))
	out = append(out, snapshot[:elementsSpan.start]...)
	out = append(out, array.Bytes()...)
	out = append(out, snapshot[elementsSpan.end:]...)
	if len(out) > cardJSONBudget {
		return append([]byte(nil), snapshot...), nil
	}
	return out, nil
}

func renderClaim(claim store.MessageClaim) (string, []renderedButton, error) {
	if strings.TrimSpace(claim.WorkflowID) == "" ||
		strings.TrimSpace(claim.OpenMessageID) == "" ||
		claim.DesiredRevision <= 0 {
		return "", nil, errors.New("claim identity and positive revision are required")
	}
	switch claim.RenderKind {
	case "action":
		return renderActionClaim(claim)
	case "rejection":
		if claim.Action != nil {
			return "", nil, errors.New("rejection claim must not include an action")
		}
		if claim.RejectionReason == "" {
			return "", nil, errors.New("rejection reason is empty")
		}
		switch claim.ButtonsMode {
		case "none":
			return claim.RejectionReason, nil, nil
		case "both":
			return claim.RejectionReason, []renderedButton{
				actionButton("重试失败变体", "primary", "retry", claim.WorkflowID),
				actionButton("忽略", "default", "ignore", claim.WorkflowID),
			}, nil
		default:
			return "", nil, fmt.Errorf("unsupported rejection buttons mode %q", claim.ButtonsMode)
		}
	default:
		return "", nil, fmt.Errorf("unsupported render kind %q", claim.RenderKind)
	}
}

func renderActionClaim(claim store.MessageClaim) (string, []renderedButton, error) {
	action := claim.Action
	if action == nil {
		return "", nil, errors.New("action claim has nil action")
	}
	if action.WorkflowID != claim.WorkflowID {
		return "", nil, fmt.Errorf(
			"action workflow id %q does not match claim %q",
			action.WorkflowID, claim.WorkflowID,
		)
	}
	if action.Revision != claim.DesiredRevision {
		return "", nil, fmt.Errorf(
			"action revision %d does not match claimed revision %d",
			action.Revision, claim.DesiredRevision,
		)
	}
	switch action.Action {
	case "retry":
		switch action.State {
		case "pending":
			if action.ActorOpenID == "" {
				return "", nil, errors.New("pending retry actor is empty")
			}
			return fmt.Sprintf("已由 %s 重试，正在启动…", action.ActorOpenID), nil, nil
		case "succeeded":
			if action.ActorOpenID == "" || action.TargetWorkflowID == "" {
				return "", nil, errors.New("succeeded retry actor and target are required")
			}
			return fmt.Sprintf(
				"已由 %s 重试 → %s",
				action.ActorOpenID, action.TargetWorkflowID,
			), nil, nil
		case "failed":
			if action.LastError == "" {
				return "", nil, errors.New("failed retry last error is empty")
			}
			return "重试启动失败：" + action.LastError,
				[]renderedButton{
					actionButton("重新重试", "primary", "retry", claim.WorkflowID),
				}, nil
		default:
			return "", nil, fmt.Errorf("unsupported retry state %q", action.State)
		}
	case "ignore":
		if action.State != "succeeded" {
			return "", nil, fmt.Errorf("unsupported ignore state %q", action.State)
		}
		if action.ActorOpenID == "" {
			return "", nil, errors.New("succeeded ignore actor is empty")
		}
		return fmt.Sprintf(
			"已由 %s 忽略（仅记录，不改变判定）",
			action.ActorOpenID,
		), nil, nil
	default:
		return "", nil, fmt.Errorf("unsupported action %q", action.Action)
	}
}

func actionButton(label, buttonType, action, workflowID string) renderedButton {
	return renderedButton{
		Tag:  "button",
		Text: renderedText{Tag: "plain_text", Content: label},
		Type: buttonType,
		Value: renderedButtonValue{
			Action: action, SourceWorkflowID: workflowID,
		},
	}
}

func validateFullCard(snapshot []byte, fields map[string]jsonSpan) error {
	if len(snapshot) == 0 || !json.Valid(snapshot) {
		return errors.New("snapshot is not valid JSON")
	}
	for _, name := range []string{"config", "header", "elements"} {
		if _, ok := fields[name]; !ok {
			return fmt.Errorf("snapshot is missing %q", name)
		}
	}
	for _, name := range []string{"config", "header"} {
		span := fields[name]
		var object map[string]json.RawMessage
		if err := json.Unmarshal(snapshot[span.start:span.end], &object); err != nil || object == nil {
			return fmt.Errorf("%s must be a JSON object", name)
		}
	}
	if _, err := arrayElementSpans(snapshot, fields["elements"]); err != nil {
		return fmt.Errorf("elements must be an array of objects: %w", err)
	}
	return nil
}

func topLevelValueSpans(raw []byte) (map[string]jsonSpan, error) {
	if len(raw) == 0 || !json.Valid(raw) {
		return nil, errors.New("invalid JSON")
	}
	i := skipJSONSpace(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return nil, errors.New("full card must be a JSON object")
	}
	i++
	fields := make(map[string]jsonSpan)
	i = skipJSONSpace(raw, i)
	if i < len(raw) && raw[i] == '}' {
		return fields, nil
	}
	for {
		i = skipJSONSpace(raw, i)
		keyStart := i
		keyEnd, err := scanJSONString(raw, keyStart)
		if err != nil {
			return nil, fmt.Errorf("scan field name: %w", err)
		}
		var key string
		if err := json.Unmarshal(raw[keyStart:keyEnd], &key); err != nil {
			return nil, fmt.Errorf("decode field name: %w", err)
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("duplicate top-level field %q", key)
		}
		i = skipJSONSpace(raw, keyEnd)
		if i >= len(raw) || raw[i] != ':' {
			return nil, fmt.Errorf("field %q has no colon", key)
		}
		valueStart := skipJSONSpace(raw, i+1)
		valueEnd, err := scanJSONValue(raw, valueStart)
		if err != nil {
			return nil, fmt.Errorf("scan field %q: %w", key, err)
		}
		fields[key] = jsonSpan{start: valueStart, end: valueEnd}
		i = skipJSONSpace(raw, valueEnd)
		if i >= len(raw) {
			return nil, errors.New("unterminated top-level object")
		}
		switch raw[i] {
		case ',':
			i++
		case '}':
			i = skipJSONSpace(raw, i+1)
			if i != len(raw) {
				return nil, errors.New("trailing data after card object")
			}
			return fields, nil
		default:
			return nil, fmt.Errorf("unexpected byte %q after field %q", raw[i], key)
		}
	}
}

func arrayElementSpans(raw []byte, span jsonSpan) ([]jsonSpan, error) {
	i := skipJSONSpace(raw, span.start)
	if i >= span.end || raw[i] != '[' {
		return nil, errors.New("value is not an array")
	}
	i++
	i = skipJSONSpace(raw, i)
	if i < span.end && raw[i] == ']' {
		return nil, nil
	}
	var elements []jsonSpan
	for {
		start := skipJSONSpace(raw, i)
		if start >= span.end || raw[start] != '{' {
			return nil, errors.New("card element is not an object")
		}
		end, err := scanJSONValue(raw, start)
		if err != nil {
			return nil, fmt.Errorf("scan card element: %w", err)
		}
		elements = append(elements, jsonSpan{start: start, end: end})
		i = skipJSONSpace(raw, end)
		if i >= span.end {
			return nil, errors.New("unterminated elements array")
		}
		switch raw[i] {
		case ',':
			i++
		case ']':
			i = skipJSONSpace(raw, i+1)
			if i != span.end {
				return nil, errors.New("trailing data in elements array")
			}
			return elements, nil
		default:
			return nil, fmt.Errorf("unexpected byte %q after card element", raw[i])
		}
	}
}

func scanJSONValue(raw []byte, i int) (int, error) {
	i = skipJSONSpace(raw, i)
	if i >= len(raw) {
		return 0, errors.New("missing JSON value")
	}
	switch raw[i] {
	case '"':
		return scanJSONString(raw, i)
	case '{':
		return scanJSONObject(raw, i)
	case '[':
		return scanJSONArray(raw, i)
	case 't':
		return scanJSONLiteral(raw, i, "true")
	case 'f':
		return scanJSONLiteral(raw, i, "false")
	case 'n':
		return scanJSONLiteral(raw, i, "null")
	default:
		return scanJSONNumber(raw, i)
	}
}

func scanJSONString(raw []byte, i int) (int, error) {
	if i >= len(raw) || raw[i] != '"' {
		return 0, errors.New("expected JSON string")
	}
	for i++; i < len(raw); i++ {
		switch raw[i] {
		case '\\':
			i++
			if i >= len(raw) {
				return 0, errors.New("unterminated string escape")
			}
		case '"':
			return i + 1, nil
		}
	}
	return 0, errors.New("unterminated JSON string")
}

func scanJSONObject(raw []byte, i int) (int, error) {
	i++
	i = skipJSONSpace(raw, i)
	if i < len(raw) && raw[i] == '}' {
		return i + 1, nil
	}
	for {
		var err error
		i, err = scanJSONString(raw, skipJSONSpace(raw, i))
		if err != nil {
			return 0, err
		}
		i = skipJSONSpace(raw, i)
		if i >= len(raw) || raw[i] != ':' {
			return 0, errors.New("object field has no colon")
		}
		i, err = scanJSONValue(raw, i+1)
		if err != nil {
			return 0, err
		}
		i = skipJSONSpace(raw, i)
		if i >= len(raw) {
			return 0, errors.New("unterminated JSON object")
		}
		switch raw[i] {
		case ',':
			i++
		case '}':
			return i + 1, nil
		default:
			return 0, errors.New("invalid JSON object separator")
		}
	}
}

func scanJSONArray(raw []byte, i int) (int, error) {
	i++
	i = skipJSONSpace(raw, i)
	if i < len(raw) && raw[i] == ']' {
		return i + 1, nil
	}
	for {
		var err error
		i, err = scanJSONValue(raw, i)
		if err != nil {
			return 0, err
		}
		i = skipJSONSpace(raw, i)
		if i >= len(raw) {
			return 0, errors.New("unterminated JSON array")
		}
		switch raw[i] {
		case ',':
			i++
		case ']':
			return i + 1, nil
		default:
			return 0, errors.New("invalid JSON array separator")
		}
	}
}

func scanJSONLiteral(raw []byte, i int, literal string) (int, error) {
	end := i + len(literal)
	if end > len(raw) || string(raw[i:end]) != literal {
		return 0, errors.New("invalid JSON literal")
	}
	return end, nil
}

func scanJSONNumber(raw []byte, i int) (int, error) {
	start := i
	if raw[i] == '-' {
		i++
	}
	if i >= len(raw) {
		return 0, errors.New("invalid JSON number")
	}
	if raw[i] == '0' {
		i++
	} else {
		if raw[i] < '1' || raw[i] > '9' {
			return 0, errors.New("invalid JSON value")
		}
		for i < len(raw) && raw[i] >= '0' && raw[i] <= '9' {
			i++
		}
	}
	if i < len(raw) && raw[i] == '.' {
		i++
		digits := i
		for i < len(raw) && raw[i] >= '0' && raw[i] <= '9' {
			i++
		}
		if i == digits {
			return 0, errors.New("invalid JSON fraction")
		}
	}
	if i < len(raw) && (raw[i] == 'e' || raw[i] == 'E') {
		i++
		if i < len(raw) && (raw[i] == '+' || raw[i] == '-') {
			i++
		}
		digits := i
		for i < len(raw) && raw[i] >= '0' && raw[i] <= '9' {
			i++
		}
		if i == digits {
			return 0, errors.New("invalid JSON exponent")
		}
	}
	if i == start {
		return 0, errors.New("invalid JSON number")
	}
	return i, nil
}

func skipJSONSpace(raw []byte, i int) int {
	for i < len(raw) {
		switch raw[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}
