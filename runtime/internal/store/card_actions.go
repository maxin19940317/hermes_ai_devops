package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// 设计文档 2026-07-31-feishu-card-actions-design §3/§5:飞书按钮点击先落
// inbox,异步解析后再由 CompleteAccept/CompleteReject 在同一事务边界内完成
// action、audit、message 与 inbox 写入。

const cardActionLease = 120 * time.Second

var (
	// ErrInboxNotFound: GetInbox 查询到未知 event_id。
	ErrInboxNotFound = errors.New("card action inbox event not found")
	// ErrInboxNotClaimable 覆盖三种情况:event_id 不存在、已进入终态(processed)、
	// 或租约仍被别的 worker 持有——调用方对三者一视同仁(退避重试),故只用一个
	// 哨兵值,不强行区分。
	ErrInboxNotClaimable = errors.New("card action inbox event not claimable")
)

// InboxRow 对应 card_action_inbox 一行(schema.sql,设计文档 §3.1)。
// LeaseExpiresAt/ProcessedAt 用指针表达列的可空性:首次入库的行两者皆 nil。
type InboxRow struct {
	EventID        string
	Disposition    string // accepted|rejected
	AckToast       string // 同步应答原文;重投的飞书事件必须原样重放这句话
	Action         string
	WorkflowID     string
	ActorOpenID    string
	OpenMessageID  string
	PayloadDigest  string // 同步段算出的摘要;恢复路径不得重新猜(原始 payload 已不在手上)
	State          string // received|processed
	Owner          string
	LastError      string
	LeaseExpiresAt *time.Time
	ProcessedAt    *time.Time
	Attempts       int
}

// AuditRow 对应 audit_log 一行。CardActionWorkflowID/InboxEventID 二选一
// (对应该行审计关联的是哪张表);为空字符串时写 NULL,不占用对应 UNIQUE 外键。
type AuditRow struct {
	Actor                string
	Action               string
	Target               string
	PayloadDigest        string
	CardActionWorkflowID string
	InboxEventID         string
}

// CardAction 对应 card_actions 一行。TargetInput 是已经过调用方逐字段断言的
// canonical wf.DeviceTestInput JSON;store 负责把它与 attempt/target 一次性钉死。
type CardAction struct {
	WorkflowID       string
	Action           string
	ActorOpenID      string
	State            string
	Owner            string
	TargetWorkflowID string
	LastError        string
	LeaseExpiresAt   *time.Time
	Attempt          int
	Revision         int
	TargetInput      []byte
}

// AcceptRequest 是 CompleteAccept 的原子输入。BuildTarget 只在首次 retry
// 接受、事务内分配出 attempt 后调用；resume/conflict/legacy/ignore 均不调用。
type AcceptRequest struct {
	EventID       string
	Token         string
	WorkflowID    string
	Action        string
	ActorOpenID   string
	OpenMessageID string
	PayloadDigest string
	ActionToken   string
	Project       string
	CommitSHA     string
	PipelineID    int
	BuildTarget   func(attempt int) (targetInput []byte, targetWorkflowID string, err error)
}

// AcceptOutcome 描述 CompleteAccept 的权威归宿。
type AcceptOutcome struct {
	Kind        string // accepted|resumed|conflict|legacy|lost
	ActionToken string
	Attempt     int
}

// RejectRender 是业务拒绝的持久化渲染输入。ButtonsMode 不由调用方传入，
// 而由 Code 按设计文档 §7.5 的封闭映射确定。
type RejectRender struct {
	Code            string
	RejectionReason string
}

type cardActionMessage struct {
	WorkflowID       string
	OpenMessageID    string
	RenderKind       string
	RejectionReason  string
	ButtonsMode      string
	UpdateState      string
	Owner            string
	LastError        string
	DesiredRevision  int
	RenderedRevision int
	Attempts         int
	LeaseExpiresAt   *time.Time
	ReconcileAfter   *time.Time
}

// validateAuditRow 复现 audit_log 的 CHECK:actor 与 action 均不得为空串。
// MemStore 没有真实数据库约束,必须在这里手动挡:否则 Task 4 的"审计失败→整笔
// 回滚"用例在两套实现上会漂移——PGStore 靠 Postgres 的 23514 拒绝插入,
// MemStore 若不复现同一条件就会把无归属的审计行悄悄接受。
func validateAuditRow(a AuditRow) error {
	if a.Actor == "" || a.Action == "" {
		return fmt.Errorf("audit row requires non-empty actor and action")
	}
	return nil
}

// PutInbox 写入一次点击,以 event_id 幂等去重(跨进程、跨重启,飞书事件 ID
// 天然满足)。已存在的事件原样返回既有行(inserted=false)且 existing 非
// nil,供调用方对重投的飞书回调重放同一句 ack_toast,而不是重新走一遍
// 判定逻辑。
//
// disposition=rejected 的行必须在这一次调用里就以终态写入(state=processed,
// processed_at 非空):Postgres 侧 CHECK 立即生效且不可 DEFERRABLE,先插
// received 再 UPDATE 会在插入那一刻就以 23514 失败,不存在"先插后补"的路径
// (§CLAUDE.md 附带上下文)。MemStore 没有这个约束,但两套实现的调用契约必须
// 一致,所以这里同样要求调用方直接传入终态的 row。
//
// auditOnReject 只在真正发生插入时才写(重复调用不重复审计:一个事件恰好
// 一条审计,§3.5);写入失败时整个 PutInbox 必须回滚——把事件标记为已处理、
// 审计表里却查无此人,比拒绝这次写入更糟。
func (s *MemStore) PutInbox(_ context.Context, row InboxRow, auditOnReject *AuditRow) (*InboxRow, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.inbox[row.EventID]; ok {
		out := existing
		return &out, false, nil
	}

	if auditOnReject != nil {
		if err := validateAuditRow(*auditOnReject); err != nil {
			// 校验在任何 map 写入之前完成:没有状态可回滚,天然满足"整笔失败"。
			return nil, false, fmt.Errorf("put inbox %s: audit: %w", row.EventID, err)
		}
	}

	if row.State == "processed" && row.ProcessedAt == nil {
		now := time.Now().UTC()
		row.ProcessedAt = &now
	}

	s.inbox[row.EventID] = row
	if auditOnReject != nil {
		s.auditLog = append(s.auditLog, *auditOnReject)
	}
	out := row
	return &out, true, nil
}

// GetInbox 按 event_id 查询;未知事件返回 ErrInboxNotFound。
func (s *MemStore) GetInbox(_ context.Context, eventID string) (*InboxRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.inbox[eventID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrInboxNotFound, eventID)
	}
	out := row
	return &out, nil
}

// ClaimInbox 用条件更新取租约,谓词必须覆盖 lease_expires_at IS NULL(首次
// 入库的行租约天然是 NULL)与 lease_expires_at < now()(过期可回收)两支——
// 漏掉 IS NULL 那一支,新落库的行会永远抢不到租约。
// 不区分"事件不存在/已终态/租约仍被占用"三种落空原因,统一返回
// ErrInboxNotClaimable,调用方按同一策略处理(退避重试)。
func (s *MemStore) ClaimInbox(_ context.Context, eventID, token string, lease time.Duration) (*InboxRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	row, ok := s.inbox[eventID]
	now := time.Now().UTC()
	claimable := ok && row.State == "received" &&
		(row.LeaseExpiresAt == nil || row.LeaseExpiresAt.Before(now))
	if !claimable {
		return nil, fmt.Errorf("%w: %s", ErrInboxNotClaimable, eventID)
	}

	row.Owner = token
	expires := now.Add(lease)
	row.LeaseExpiresAt = &expires
	row.Attempts++
	s.inbox[eventID] = row
	out := row
	return &out, nil
}

// CompleteAccept 只完成已经 ClaimInbox 的 live claim，不会重新 acquire。
// owner 或租约不匹配时返回 lost，且不写任何业务状态。
func (s *MemStore) CompleteAccept(_ context.Context, req AcceptRequest) (*AcceptOutcome, error) {
	if err := validateAcceptEnvelope(req); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	inbox, ok := s.inbox[req.EventID]
	now := time.Now().UTC()
	if !ok || inbox.State != "received" || inbox.Owner != req.Token ||
		inbox.LeaseExpiresAt == nil || !inbox.LeaseExpiresAt.After(now) {
		return &AcceptOutcome{Kind: "lost"}, nil
	}
	if err := validateAcceptMatchesInbox(req, inbox); err != nil {
		return nil, err
	}

	run, authoritative := s.workflowRuns[req.WorkflowID]
	if !authoritative {
		if err := s.completeLegacyAcceptLocked(inbox, now); err != nil {
			return nil, err
		}
		return &AcceptOutcome{Kind: "legacy"}, nil
	}

	action, exists := s.cardActions[req.WorkflowID]
	var outcome AcceptOutcome
	var revision int
	switch {
	case !exists:
		switch req.Action {
		case "retry":
			if req.ActionToken == "" {
				return nil, errors.New("complete accept: retry requires action token")
			}
			if req.Project != run.Project || req.CommitSHA != run.CommitSHA ||
				req.PipelineID != run.PipelineID {
				return nil, fmt.Errorf("complete accept: retry coordinates do not match workflow run %s", req.WorkflowID)
			}
			if req.BuildTarget == nil {
				return nil, errors.New("complete accept: retry requires BuildTarget")
			}
			attempt, err := s.nextWorkflowAttemptAllTargetLocked(req.Project, req.CommitSHA, req.PipelineID)
			if err != nil {
				return nil, err
			}
			targetInput, targetWorkflowID, err := req.BuildTarget(attempt)
			if err != nil {
				return nil, fmt.Errorf("complete accept: build target: %w", err)
			}
			if err := validateTargetPins(targetInput, targetWorkflowID); err != nil {
				return nil, fmt.Errorf("complete accept: build target: %w", err)
			}
			expires := time.Now().UTC().Add(cardActionLease)
			action = CardAction{
				WorkflowID: req.WorkflowID, Action: req.Action, ActorOpenID: req.ActorOpenID,
				State: "pending", Owner: req.ActionToken, LeaseExpiresAt: &expires,
				TargetWorkflowID: targetWorkflowID, Attempt: attempt,
				TargetInput: append([]byte(nil), targetInput...), Revision: 1,
			}
			s.setWorkflowAttemptAllLocked(req.Project, req.CommitSHA, req.PipelineID, attempt)
			outcome = AcceptOutcome{Kind: "accepted", ActionToken: req.ActionToken, Attempt: attempt}
		case "ignore":
			action = CardAction{
				WorkflowID: req.WorkflowID, Action: req.Action, ActorOpenID: req.ActorOpenID,
				State: "succeeded", Revision: 1,
			}
			outcome = AcceptOutcome{Kind: "accepted"}
		}
		s.cardActions[req.WorkflowID] = cloneCardAction(action)
		s.auditLog = append(s.auditLog, acceptedAudit(inbox))
		revision = 1
	case action.Action == req.Action && action.State == "failed":
		if req.ActionToken == "" {
			return nil, errors.New("complete accept: resume requires action token")
		}
		expires := time.Now().UTC().Add(cardActionLease)
		action.State = "pending"
		action.Owner = req.ActionToken
		action.LeaseExpiresAt = &expires
		action.LastError = ""
		action.Revision++
		s.cardActions[req.WorkflowID] = cloneCardAction(action)
		s.auditLog = append(s.auditLog, AuditRow{
			Actor: actorAudit(inbox.ActorOpenID), Action: "card.retry.resumed",
			Target: inbox.WorkflowID, PayloadDigest: inbox.PayloadDigest, InboxEventID: inbox.EventID,
		})
		outcome = AcceptOutcome{
			Kind: "resumed", ActionToken: req.ActionToken, Attempt: action.Attempt,
		}
		revision = action.Revision
	default:
		s.auditLog = append(s.auditLog, AuditRow{
			Actor: actorAudit(inbox.ActorOpenID), Action: "card." + inbox.Action + ".rejected.conflict",
			Target: inbox.WorkflowID, PayloadDigest: inbox.PayloadDigest, InboxEventID: inbox.EventID,
		})
		outcome = AcceptOutcome{Kind: "conflict", Attempt: action.Attempt}
		revision = action.Revision
	}

	s.upsertActionMessageLocked(inbox.WorkflowID, inbox.OpenMessageID, revision)
	if outcome.Kind == "accepted" || outcome.Kind == "resumed" {
		s.reorderActionMessagesLocked(inbox.WorkflowID, revision)
	}
	s.processInboxLocked(inbox, now)
	return &outcome, nil
}

// CompleteReject atomically writes the rejection audit/message and marks the
// claimed inbox processed. Lost or expired claims are successful no-ops.
func (s *MemStore) CompleteReject(
	_ context.Context, eventID, token string, render RejectRender,
) error {
	buttonsMode, err := validateRejectRender(render)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	inbox, ok := s.inbox[eventID]
	now := time.Now().UTC()
	if !ok || inbox.State != "received" || inbox.Owner != token ||
		inbox.LeaseExpiresAt == nil || !inbox.LeaseExpiresAt.After(now) {
		return nil
	}
	s.auditLog = append(s.auditLog, AuditRow{
		Actor:  actorAudit(inbox.ActorOpenID),
		Action: rejectionAuditAction(inbox.Action, render.Code),
		Target: inbox.WorkflowID, PayloadDigest: inbox.PayloadDigest, InboxEventID: inbox.EventID,
	})
	s.upsertRejectionMessageLocked(
		inbox.WorkflowID, inbox.OpenMessageID, render.RejectionReason, buttonsMode,
	)
	s.processInboxLocked(inbox, now)
	return nil
}

// FinalizeAction applies a retry terminal state only while the pending action
// still has the matching owner and a live lease. A successful transition
// advances revision and reorders every observed message instance atomically.
func (s *MemStore) FinalizeAction(
	_ context.Context, workflowID, token, state, lastErr string,
) (bool, error) {
	if state != "succeeded" && state != "failed" {
		return false, fmt.Errorf("finalize action: invalid state %q", state)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	action, ok := s.cardActions[workflowID]
	now := time.Now().UTC()
	if !ok || action.State != "pending" || action.Owner != token ||
		action.LeaseExpiresAt == nil || !action.LeaseExpiresAt.After(now) {
		return false, nil
	}
	action.State = state
	action.LastError = lastErr
	action.Owner = ""
	action.LeaseExpiresAt = nil
	action.Revision++
	s.cardActions[workflowID] = cloneCardAction(action)
	s.reorderActionMessagesLocked(workflowID, action.Revision)
	return true, nil
}

func validateAcceptEnvelope(req AcceptRequest) error {
	if req.EventID == "" || req.Token == "" || req.WorkflowID == "" ||
		req.ActorOpenID == "" || req.OpenMessageID == "" {
		return errors.New("complete accept: required field is empty")
	}
	if req.Action != "retry" && req.Action != "ignore" {
		return fmt.Errorf("complete accept: invalid action %q", req.Action)
	}
	return nil
}

func validateAcceptMatchesInbox(req AcceptRequest, inbox InboxRow) error {
	if req.WorkflowID != inbox.WorkflowID || req.Action != inbox.Action ||
		req.ActorOpenID != inbox.ActorOpenID || req.OpenMessageID != inbox.OpenMessageID ||
		req.PayloadDigest != inbox.PayloadDigest {
		return fmt.Errorf("complete accept: request does not match claimed inbox %s", req.EventID)
	}
	return nil
}

func validateTargetPins(targetInput []byte, targetWorkflowID string) error {
	if targetWorkflowID == "" || len(targetInput) == 0 || !json.Valid(targetInput) {
		return errors.New("retry target requires non-empty workflow id and valid JSON input")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(targetInput, &object); err != nil || object == nil {
		return errors.New("retry target input must be a JSON object")
	}
	return nil
}

func validateRejectRender(render RejectRender) (string, error) {
	if render.RejectionReason == "" {
		return "", errors.New("complete reject: rejection reason is empty")
	}
	switch render.Code {
	case "StillRunning", "ResultUnreadable", "ArtifactMissing":
		return "both", nil
	case "NotAuthoritative", "NoFailedVariants":
		return "none", nil
	default:
		return "", fmt.Errorf("complete reject: unsupported reason code %q", render.Code)
	}
}

func rejectionAuditAction(action, code string) string {
	suffix := map[string]string{
		"StillRunning":     "still_running",
		"ResultUnreadable": "result_unreadable",
		"ArtifactMissing":  "artifact_missing",
		"NotAuthoritative": "not_authoritative",
		"NoFailedVariants": "no_failed_variants",
	}[code]
	return "card." + action + ".rejected." + suffix
}

func actorAudit(openID string) string {
	if openID == "" {
		return "feishu:unknown"
	}
	return "feishu:" + openID
}

func acceptedAudit(inbox InboxRow) AuditRow {
	return AuditRow{
		Actor: actorAudit(inbox.ActorOpenID), Action: "card." + inbox.Action + ".accepted",
		Target: inbox.WorkflowID, PayloadDigest: inbox.PayloadDigest,
		CardActionWorkflowID: inbox.WorkflowID, InboxEventID: inbox.EventID,
	}
}

func cloneCardAction(action CardAction) CardAction {
	action.TargetInput = append([]byte(nil), action.TargetInput...)
	return action
}

func messageKey(workflowID, openMessageID string) string {
	return workflowID + "\x00" + openMessageID
}

func (s *MemStore) upsertActionMessageLocked(workflowID, openMessageID string, revision int) {
	key := messageKey(workflowID, openMessageID)
	row := s.cardActionMessages[key]
	row.WorkflowID = workflowID
	row.OpenMessageID = openMessageID
	row.RenderKind = "action"
	row.RejectionReason = ""
	row.ButtonsMode = "none"
	row.DesiredRevision = revision
	row.UpdateState = "pending"
	row.Owner = ""
	row.LeaseExpiresAt = nil
	row.ReconcileAfter = nil
	s.cardActionMessages[key] = row
}

func (s *MemStore) upsertRejectionMessageLocked(
	workflowID, openMessageID, reason, buttonsMode string,
) {
	key := messageKey(workflowID, openMessageID)
	row, exists := s.cardActionMessages[key]
	if exists && row.RenderKind == "action" {
		return
	}
	row.WorkflowID = workflowID
	row.OpenMessageID = openMessageID
	row.RenderKind = "rejection"
	row.RejectionReason = reason
	row.ButtonsMode = buttonsMode
	if row.DesiredRevision == 0 {
		row.DesiredRevision = 1
	}
	row.UpdateState = "pending"
	row.Owner = ""
	row.LeaseExpiresAt = nil
	row.ReconcileAfter = nil
	s.cardActionMessages[key] = row
}

func (s *MemStore) reorderActionMessagesLocked(workflowID string, revision int) {
	for key, row := range s.cardActionMessages {
		if row.WorkflowID != workflowID {
			continue
		}
		row.DesiredRevision = revision
		row.UpdateState = "pending"
		row.Owner = ""
		row.LeaseExpiresAt = nil
		row.ReconcileAfter = nil
		s.cardActionMessages[key] = row
	}
}

func (s *MemStore) processInboxLocked(inbox InboxRow, now time.Time) {
	inbox.State = "processed"
	inbox.ProcessedAt = &now
	s.inbox[inbox.EventID] = inbox
}

func (s *MemStore) completeLegacyAcceptLocked(inbox InboxRow, now time.Time) error {
	render := RejectRender{
		Code: "NotAuthoritative", RejectionReason: "该运行不是权威记录，无法执行卡片动作",
	}
	buttonsMode, err := validateRejectRender(render)
	if err != nil {
		return err
	}
	s.auditLog = append(s.auditLog, AuditRow{
		Actor:  actorAudit(inbox.ActorOpenID),
		Action: rejectionAuditAction(inbox.Action, render.Code),
		Target: inbox.WorkflowID, PayloadDigest: inbox.PayloadDigest, InboxEventID: inbox.EventID,
	})
	s.upsertRejectionMessageLocked(
		inbox.WorkflowID, inbox.OpenMessageID, render.RejectionReason, buttonsMode,
	)
	s.processInboxLocked(inbox, now)
	return nil
}
