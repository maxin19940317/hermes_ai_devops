// Package cardaction validates and durably accepts Feishu card button clicks.
package cardaction

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	"github.com/rs/zerolog"

	"hermes-devops/runtime/internal/store"
)

const (
	handlerTimeout  = 2 * time.Second
	maxPayloadBytes = 4 * 1024

	ackAccepted     = "已收到，正在处理"
	ackInvalid      = "请求无效"
	ackDisabled     = "按钮已停用"
	ackUnauthorized = "无权限"
)

// Store is the synchronous handler's complete persistence surface.
type Store interface {
	PutInbox(
		ctx context.Context,
		row store.InboxRow,
		auditOnReject *store.AuditRow,
	) (existing *store.InboxRow, inserted bool, err error)
}

// Handler validates and persists Feishu card action callbacks.
type Handler struct {
	Store     Store
	Readiness *Readiness
	Whitelist map[string]bool
	AppID     string
	Log       *zerolog.Logger
	Consume   func(eventID string)
}

type validatedAction struct {
	eventID       string
	action        string
	workflowID    string
	actorOpenID   string
	openMessageID string
	payloadDigest string
}

// OnCardAction synchronously validates and persists a card click. Accepted
// events are handed to Consume only after the inbox transaction succeeds.
func (h *Handler) OnCardAction(
	ctx context.Context,
	ev *callback.CardActionTriggerEvent,
) (*callback.CardActionTriggerResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, handlerTimeout)
	defer cancel()

	eventID := eventIDOf(ev)
	if strings.TrimSpace(eventID) == "" {
		h.logMissingEventID()
		return toast(ackInvalid), nil
	}

	action, reason := h.validate(ev, eventID)
	if reason != "" {
		return h.persistRejected(ctx, action, reason)
	}
	return h.persistAccepted(ctx, action)
}

func (h *Handler) validate(
	ev *callback.CardActionTriggerEvent,
	eventID string,
) (validatedAction, string) {
	out := validatedAction{eventID: eventID}
	if ev == nil || ev.EventV2Base == nil || ev.EventV2Base.Header == nil ||
		h.AppID == "" || ev.EventV2Base.Header.AppID != h.AppID {
		return observe(ev, eventID), "source"
	}

	req := ev.Event
	headerTenant := ev.EventV2Base.Header.TenantKey
	if req == nil || req.Operator == nil || req.Operator.TenantKey == nil ||
		strings.TrimSpace(headerTenant) == "" ||
		strings.TrimSpace(*req.Operator.TenantKey) == "" ||
		headerTenant != *req.Operator.TenantKey {
		return observe(ev, eventID), "source"
	}
	if req.Action == nil || req.Action.Tag != "button" {
		return observe(ev, eventID), "payload"
	}
	if req.Context == nil || strings.TrimSpace(req.Context.OpenMessageID) == "" {
		return observe(ev, eventID), "payload"
	}
	if req.Host != "im_message" {
		return observe(ev, eventID), "payload"
	}

	out.actorOpenID = req.Operator.OpenID
	out.openMessageID = req.Context.OpenMessageID
	if action, ok := req.Action.Value["action"].(string); ok &&
		(action == "retry" || action == "ignore") {
		out.action = action
	}
	if workflowID, ok := req.Action.Value["source_workflow_id"].(string); ok &&
		len(workflowID) <= maxPayloadBytes {
		out.workflowID = workflowID
	}
	payload, err := json.Marshal(req.Action.Value)
	if err != nil || len(payload) > maxPayloadBytes {
		return out, "payload"
	}
	sum := sha256.Sum256(payload)
	out.payloadDigest = hex.EncodeToString(sum[:])

	if len(req.Action.Value) != 2 {
		return out, "payload"
	}
	if out.action != "retry" && out.action != "ignore" {
		return out, "payload"
	}
	if strings.TrimSpace(out.workflowID) == "" {
		return out, "payload"
	}

	if strings.TrimSpace(req.Operator.OpenID) == "" {
		return out, "identity"
	}
	if h.Readiness == nil || !h.Readiness.Ready() {
		return out, "disabled"
	}
	if !h.Whitelist[req.Operator.OpenID] {
		return out, "unauthorized"
	}
	return out, ""
}

func observe(ev *callback.CardActionTriggerEvent, eventID string) validatedAction {
	out := validatedAction{eventID: eventID}
	if ev == nil || ev.Event == nil {
		return out
	}
	req := ev.Event
	if req.Operator != nil {
		out.actorOpenID = req.Operator.OpenID
	}
	if req.Context != nil {
		out.openMessageID = req.Context.OpenMessageID
	}
	if req.Action == nil {
		return out
	}
	if action, ok := req.Action.Value["action"].(string); ok &&
		(action == "retry" || action == "ignore") {
		out.action = action
	}
	if workflowID, ok := req.Action.Value["source_workflow_id"].(string); ok &&
		len(workflowID) <= maxPayloadBytes {
		out.workflowID = workflowID
	}
	payload, err := json.Marshal(req.Action.Value)
	if err == nil && len(payload) <= maxPayloadBytes {
		sum := sha256.Sum256(payload)
		out.payloadDigest = hex.EncodeToString(sum[:])
	}
	return out
}

func (h *Handler) persistRejected(
	ctx context.Context,
	action validatedAction,
	reason string,
) (*callback.CardActionTriggerResponse, error) {
	now := time.Now().UTC()
	ack, auditAction := rejection(reason, action.action)
	row := store.InboxRow{
		EventID:       action.eventID,
		Disposition:   "rejected",
		AckToast:      ack,
		Action:        action.action,
		WorkflowID:    action.workflowID,
		ActorOpenID:   action.actorOpenID,
		OpenMessageID: action.openMessageID,
		PayloadDigest: action.payloadDigest,
		State:         "processed",
		ProcessedAt:   &now,
	}
	audit := &store.AuditRow{
		Actor:         auditActor(action.actorOpenID),
		Action:        auditAction,
		Target:        action.workflowID,
		PayloadDigest: action.payloadDigest,
		InboxEventID:  action.eventID,
	}
	stored, _, err := h.putInbox(ctx, row, audit)
	if err != nil {
		return nil, err
	}
	return toast(stored.AckToast), nil
}

func (h *Handler) persistAccepted(
	ctx context.Context,
	action validatedAction,
) (*callback.CardActionTriggerResponse, error) {
	row := store.InboxRow{
		EventID:       action.eventID,
		Disposition:   "accepted",
		AckToast:      ackAccepted,
		Action:        action.action,
		WorkflowID:    action.workflowID,
		ActorOpenID:   action.actorOpenID,
		OpenMessageID: action.openMessageID,
		PayloadDigest: action.payloadDigest,
		State:         "received",
	}
	stored, inserted, err := h.putInbox(ctx, row, nil)
	if err != nil {
		return nil, err
	}
	if inserted && h.Consume != nil {
		go h.Consume(action.eventID)
	}
	return toast(stored.AckToast), nil
}

func (h *Handler) putInbox(
	ctx context.Context,
	row store.InboxRow,
	audit *store.AuditRow,
) (*store.InboxRow, bool, error) {
	if h.Store == nil {
		return nil, false, fmt.Errorf("put card action inbox %s: store is nil", row.EventID)
	}
	stored, inserted, err := h.Store.PutInbox(ctx, row, audit)
	if err != nil {
		return nil, false, fmt.Errorf("put card action inbox %s: %w", row.EventID, err)
	}
	if stored == nil {
		return nil, false, fmt.Errorf("put card action inbox %s: store returned nil row", row.EventID)
	}
	return stored, inserted, nil
}

func rejection(reason, action string) (string, string) {
	switch reason {
	case "disabled":
		return ackDisabled, "card." + action + ".rejected.disabled"
	case "unauthorized":
		return ackUnauthorized, "card." + action + ".rejected.unauthorized"
	case "source", "payload", "identity":
		return ackInvalid, "card.unknown.rejected.payload"
	default:
		return ackInvalid, "card.unknown.rejected.payload"
	}
}

func auditActor(openID string) string {
	if strings.TrimSpace(openID) == "" {
		return "feishu:unknown"
	}
	return "feishu:" + openID
}

func eventIDOf(ev *callback.CardActionTriggerEvent) string {
	if ev == nil || ev.EventV2Base == nil || ev.EventV2Base.Header == nil {
		return ""
	}
	return ev.EventV2Base.Header.EventID
}

func (h *Handler) logMissingEventID() {
	if h != nil && h.Log != nil {
		h.Log.Warn().Str("event_id", "").Msg("reject card action without usable event_id")
	}
}

func toast(content string) *callback.CardActionTriggerResponse {
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: "info", Content: content},
	}
}
