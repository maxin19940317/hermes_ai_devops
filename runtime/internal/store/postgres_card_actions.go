package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// inboxColumns 与 scanInboxRow 的字段顺序一一对应,SELECT 与 UPDATE...RETURNING
// 共用同一份,避免两处顺序漂移。
const inboxColumns = `
	event_id, disposition, ack_toast, action, workflow_id, actor_open_id,
	open_message_id, payload_digest, state, owner, lease_expires_at,
	attempts, last_error, processed_at`

func scanInboxRow(scanner interface{ Scan(...any) error }) (InboxRow, error) {
	var row InboxRow
	var lease, processed sql.NullTime
	err := scanner.Scan(
		&row.EventID, &row.Disposition, &row.AckToast, &row.Action, &row.WorkflowID,
		&row.ActorOpenID, &row.OpenMessageID, &row.PayloadDigest, &row.State, &row.Owner,
		&lease, &row.Attempts, &row.LastError, &processed,
	)
	if lease.Valid {
		t := lease.Time.UTC()
		row.LeaseExpiresAt = &t
	}
	if processed.Valid {
		t := processed.Time.UTC()
		row.ProcessedAt = &t
	}
	return row, err
}

// PutInbox 见 card_actions.go 上 MemStore 同名方法的 godoc(两套实现共享同一
// 契约)。PG 侧单事务:CHECK 立即生效且不可 DEFERRABLE,所以 rejected 分支
// 在这一条 INSERT 里就写成终态,审计与插入共享同一事务——审计失败(此处等价
// 于撞上 audit_log 的 actor/action CHECK)时 defer 的 Rollback 把整笔一起
// 撤销,不会出现"事件已处理,审计查无此人"的半途状态。
func (s *PGStore) PutInbox(ctx context.Context, row InboxRow, auditOnReject *AuditRow) (*InboxRow, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("put inbox %s: begin: %w", row.EventID, err)
	}
	defer func() { _ = tx.Rollback() }()

	var processedAt any
	if row.State == "processed" {
		processedAt = time.Now().UTC()
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO card_action_inbox
			(event_id, disposition, ack_toast, action, workflow_id, actor_open_id,
			 open_message_id, payload_digest, state, processed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (event_id) DO NOTHING`,
		row.EventID, row.Disposition, row.AckToast, row.Action, row.WorkflowID,
		row.ActorOpenID, row.OpenMessageID, row.PayloadDigest, row.State, processedAt)
	if err != nil {
		return nil, false, fmt.Errorf("put inbox %s: insert: %w", row.EventID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("put inbox %s: rows affected: %w", row.EventID, err)
	}
	inserted := n == 1

	// 只在真正插入时才写审计:重投的事件命中 ON CONFLICT DO NOTHING,不得
	// 重复计入(§3.5:一个 inbox 事件恰好一条审计,靠 audit_log.inbox_event_id
	// 的 UNIQUE 兜底,这里提前避免二次写入)。
	if inserted && auditOnReject != nil {
		if err := insertAuditTx(ctx, tx, *auditOnReject); err != nil {
			return nil, false, fmt.Errorf("put inbox %s: audit: %w", row.EventID, err)
		}
	}

	stored, err := scanInboxRow(tx.QueryRowContext(ctx,
		`SELECT `+inboxColumns+` FROM card_action_inbox WHERE event_id = $1`, row.EventID))
	if err != nil {
		return nil, false, fmt.Errorf("put inbox %s: read back: %w", row.EventID, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("put inbox %s: commit: %w", row.EventID, err)
	}
	return &stored, inserted, nil
}

// insertAuditTx 写 audit_log 一行;card_action_workflow_id/inbox_event_id
// 为空字符串时写 NULL,不去占用对应的 UNIQUE 约束。
func insertAuditTx(ctx context.Context, tx *sql.Tx, a AuditRow) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO audit_log (actor, action, target, payload_digest, card_action_workflow_id, inbox_event_id)
		VALUES ($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,''))`,
		a.Actor, a.Action, a.Target, a.PayloadDigest, a.CardActionWorkflowID, a.InboxEventID)
	if err != nil {
		return fmt.Errorf("insert audit log: %w", err)
	}
	return nil
}

// GetInbox 按 event_id 查询;未知事件返回 ErrInboxNotFound。
func (s *PGStore) GetInbox(ctx context.Context, eventID string) (*InboxRow, error) {
	row, err := scanInboxRow(s.DB.QueryRowContext(ctx,
		`SELECT `+inboxColumns+` FROM card_action_inbox WHERE event_id = $1`, eventID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrInboxNotFound, eventID)
	}
	if err != nil {
		return nil, fmt.Errorf("get inbox %s: %w", eventID, err)
	}
	return &row, nil
}

// ClaimInbox 条件更新取租约。谓词必须覆盖 lease_expires_at IS NULL(首次
// 入库的行租约天然是 NULL)与 lease_expires_at < now()(过期可回收)两支;
// 零行命中(事件不存在/已 processed/租约仍被占用)一律 ErrInboxNotClaimable,
// 调用方按同一策略处理(退避重试),不需要在 SQL 层面区分三种落空原因。
func (s *PGStore) ClaimInbox(ctx context.Context, eventID, token string, lease time.Duration) (*InboxRow, error) {
	row, err := scanInboxRow(s.DB.QueryRowContext(ctx, `
		UPDATE card_action_inbox
		   SET owner = $2,
		       lease_expires_at = now() + make_interval(secs => $3),
		       attempts = attempts + 1
		 WHERE event_id = $1
		   AND state = 'received'
		   AND (lease_expires_at IS NULL OR lease_expires_at < now())
		RETURNING `+inboxColumns,
		eventID, token, lease.Seconds()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrInboxNotClaimable, eventID)
	}
	if err != nil {
		return nil, fmt.Errorf("claim inbox %s: %w", eventID, err)
	}
	return &row, nil
}

func scanCardAction(scanner interface{ Scan(...any) error }) (CardAction, error) {
	var row CardAction
	var lease sql.NullTime
	var targetInput []byte
	err := scanner.Scan(
		&row.WorkflowID, &row.Action, &row.ActorOpenID, &row.State, &row.Owner,
		&lease, &row.TargetWorkflowID, &row.Attempt, &targetInput, &row.LastError, &row.Revision,
	)
	if lease.Valid {
		expires := lease.Time.UTC()
		row.LeaseExpiresAt = &expires
	}
	row.TargetInput = append([]byte(nil), targetInput...)
	return row, err
}

const cardActionColumns = `
	workflow_id, action, actor_open_id, state, owner, lease_expires_at,
	target_workflow_id, attempt, target_input, last_error, revision`

// CompleteAccept 只完成已经 ClaimInbox 的 live claim，不会重新 acquire。
// 首次接受通过 workflow_runs 父行锁串行化，attempt 分配、BuildTarget、
// 完整 action 插入、审计、消息登记与 inbox processed 同属一个事务。
func (s *PGStore) CompleteAccept(
	ctx context.Context, req AcceptRequest,
) (*AcceptOutcome, error) {
	if err := validateAcceptEnvelope(req); err != nil {
		return nil, err
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("complete accept %s: begin: %w", req.EventID, err)
	}
	defer func() { _ = tx.Rollback() }()

	inbox, err := scanInboxRow(tx.QueryRowContext(ctx, `
		SELECT `+inboxColumns+`
		  FROM card_action_inbox
		 WHERE event_id=$1 AND state='received'
		   AND owner=$2
		 FOR UPDATE`, req.EventID, req.Token))
	if errors.Is(err, sql.ErrNoRows) {
		return &AcceptOutcome{Kind: "lost"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("complete accept %s: fence inbox: %w", req.EventID, err)
	}
	if inbox.LeaseExpiresAt == nil || !inbox.LeaseExpiresAt.After(time.Now().UTC()) {
		return &AcceptOutcome{Kind: "lost"}, nil
	}
	if err := validateAcceptMatchesInbox(req, inbox); err != nil {
		return nil, err
	}

	var runProject, runCommit string
	var runPipeline int
	err = tx.QueryRowContext(ctx, `
		SELECT project, commit_sha, pipeline_id
		  FROM workflow_runs
		 WHERE workflow_id=$1
		 FOR UPDATE`, req.WorkflowID).Scan(&runProject, &runCommit, &runPipeline)
	if errors.Is(err, sql.ErrNoRows) {
		if err := completeLegacyAcceptTx(ctx, tx, inbox); err != nil {
			return nil, fmt.Errorf("complete accept %s: legacy: %w", req.EventID, err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("complete accept %s: commit legacy: %w", req.EventID, err)
		}
		return &AcceptOutcome{Kind: "legacy"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("complete accept %s: lock workflow run: %w", req.EventID, err)
	}

	action, err := scanCardAction(tx.QueryRowContext(ctx,
		`SELECT `+cardActionColumns+`
		   FROM card_actions
		  WHERE workflow_id=$1
		  FOR UPDATE`,
		req.WorkflowID))
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("complete accept %s: read action: %w", req.EventID, err)
	}

	var outcome AcceptOutcome
	var revision int
	switch {
	case !exists:
		switch req.Action {
		case "retry":
			if req.ActionToken == "" {
				return nil, errors.New("complete accept: retry requires action token")
			}
			if req.Project != runProject || req.CommitSHA != runCommit ||
				req.PipelineID != runPipeline {
				return nil, fmt.Errorf("complete accept: retry coordinates do not match workflow run %s", req.WorkflowID)
			}
			if req.BuildTarget == nil {
				return nil, errors.New("complete accept: retry requires BuildTarget")
			}
			attempt, err := nextWorkflowAttemptAllTx(
				ctx, tx, req.Project, req.CommitSHA, req.PipelineID,
			)
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
			leaseExpiresAt := time.Now().UTC().Add(cardActionLease)
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO card_actions
					(workflow_id, action, actor_open_id, state, owner, lease_expires_at,
					 target_workflow_id, attempt, target_input, revision)
				VALUES ($1,$2,$3,'pending',$4,$5,$6,$7,$8::jsonb,1)`,
				req.WorkflowID, req.Action, req.ActorOpenID, req.ActionToken,
				leaseExpiresAt, targetWorkflowID, attempt, string(targetInput)); err != nil {
				return nil, fmt.Errorf("complete accept %s: insert retry action: %w", req.EventID, err)
			}
			outcome = AcceptOutcome{
				Kind: "accepted", ActionToken: req.ActionToken, Attempt: attempt,
			}
		case "ignore":
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO card_actions
					(workflow_id, action, actor_open_id, state, revision)
				VALUES ($1,$2,$3,'succeeded',1)`,
				req.WorkflowID, req.Action, req.ActorOpenID); err != nil {
				return nil, fmt.Errorf("complete accept %s: insert ignore action: %w", req.EventID, err)
			}
			outcome = AcceptOutcome{Kind: "accepted"}
		}
		if err := insertAuditTx(ctx, tx, acceptedAudit(inbox)); err != nil {
			return nil, fmt.Errorf("complete accept %s: accepted audit: %w", req.EventID, err)
		}
		revision = 1
	case action.Action == req.Action && action.State == "failed":
		if req.ActionToken == "" {
			return nil, errors.New("complete accept: resume requires action token")
		}
		revision = action.Revision + 1
		leaseExpiresAt := time.Now().UTC().Add(cardActionLease)
		if _, err := tx.ExecContext(ctx, `
			UPDATE card_actions
			   SET state='pending', owner=$2,
			       lease_expires_at=$3,
			       last_error='', revision=$4, updated_at=now()
			 WHERE workflow_id=$1 AND state='failed'`,
			req.WorkflowID, req.ActionToken, leaseExpiresAt, revision); err != nil {
			return nil, fmt.Errorf("complete accept %s: resume action: %w", req.EventID, err)
		}
		if err := insertAuditTx(ctx, tx, AuditRow{
			Actor: actorAudit(inbox.ActorOpenID), Action: "card.retry.resumed",
			Target: inbox.WorkflowID, PayloadDigest: inbox.PayloadDigest, InboxEventID: inbox.EventID,
		}); err != nil {
			return nil, fmt.Errorf("complete accept %s: resumed audit: %w", req.EventID, err)
		}
		outcome = AcceptOutcome{
			Kind: "resumed", ActionToken: req.ActionToken, Attempt: action.Attempt,
		}
	default:
		revision = action.Revision
		if err := insertAuditTx(ctx, tx, AuditRow{
			Actor:  actorAudit(inbox.ActorOpenID),
			Action: "card." + inbox.Action + ".rejected.conflict",
			Target: inbox.WorkflowID, PayloadDigest: inbox.PayloadDigest, InboxEventID: inbox.EventID,
		}); err != nil {
			return nil, fmt.Errorf("complete accept %s: conflict audit: %w", req.EventID, err)
		}
		outcome = AcceptOutcome{Kind: "conflict", Attempt: action.Attempt}
	}

	if err := upsertActionMessageTx(
		ctx, tx, inbox.WorkflowID, inbox.OpenMessageID, revision,
	); err != nil {
		return nil, fmt.Errorf("complete accept %s: register message: %w", req.EventID, err)
	}
	if outcome.Kind == "accepted" || outcome.Kind == "resumed" {
		if err := reorderActionMessagesTx(ctx, tx, inbox.WorkflowID, revision); err != nil {
			return nil, fmt.Errorf("complete accept %s: reorder messages: %w", req.EventID, err)
		}
	}
	if err := processInboxTx(ctx, tx, inbox.EventID); err != nil {
		return nil, fmt.Errorf("complete accept %s: process inbox: %w", req.EventID, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("complete accept %s: commit: %w", req.EventID, err)
	}
	return &outcome, nil
}

// CompleteReject atomically writes the rejection audit/message and marks the
// claimed inbox processed. Lost or expired claims are successful no-ops.
func (s *PGStore) CompleteReject(
	ctx context.Context, eventID, token string, render RejectRender,
) error {
	buttonsMode, err := validateRejectRender(render)
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("complete reject %s: begin: %w", eventID, err)
	}
	defer func() { _ = tx.Rollback() }()

	inbox, err := scanInboxRow(tx.QueryRowContext(ctx, `
		SELECT `+inboxColumns+`
		  FROM card_action_inbox
		 WHERE event_id=$1 AND state='received'
		   AND owner=$2
		 FOR UPDATE`, eventID, token))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("complete reject %s: fence inbox: %w", eventID, err)
	}
	if inbox.LeaseExpiresAt == nil || !inbox.LeaseExpiresAt.After(time.Now().UTC()) {
		return nil
	}
	if err := insertAuditTx(ctx, tx, AuditRow{
		Actor:  actorAudit(inbox.ActorOpenID),
		Action: rejectionAuditAction(inbox.Action, render.Code),
		Target: inbox.WorkflowID, PayloadDigest: inbox.PayloadDigest, InboxEventID: inbox.EventID,
	}); err != nil {
		return fmt.Errorf("complete reject %s: audit: %w", eventID, err)
	}
	if err := upsertRejectionMessageTx(
		ctx, tx, inbox.WorkflowID, inbox.OpenMessageID, render.RejectionReason, buttonsMode,
	); err != nil {
		return fmt.Errorf("complete reject %s: register message: %w", eventID, err)
	}
	if err := processInboxTx(ctx, tx, inbox.EventID); err != nil {
		return fmt.Errorf("complete reject %s: process inbox: %w", eventID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("complete reject %s: commit: %w", eventID, err)
	}
	return nil
}

// FinalizeAction applies a retry terminal state only while the pending action
// still has the matching owner and a live lease. Revision and all message
// reordering changes commit atomically.
func (s *PGStore) FinalizeAction(
	ctx context.Context, workflowID, token, state, lastErr string,
) (bool, error) {
	if state != "succeeded" && state != "failed" {
		return false, fmt.Errorf("finalize action: invalid state %q", state)
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return false, fmt.Errorf("finalize action %s: begin: %w", workflowID, err)
	}
	defer func() { _ = tx.Rollback() }()

	action, err := scanCardAction(tx.QueryRowContext(ctx, `
		SELECT `+cardActionColumns+`
		  FROM card_actions
		 WHERE workflow_id=$1 AND state='pending' AND owner=$2
		 FOR UPDATE`,
		workflowID, token))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("finalize action %s: fence: %w", workflowID, err)
	}
	if action.LeaseExpiresAt == nil || !action.LeaseExpiresAt.After(time.Now().UTC()) {
		return false, nil
	}

	var revision int
	err = tx.QueryRowContext(ctx, `
		UPDATE card_actions
		   SET state=$3, last_error=$4, revision=revision+1,
		       owner='', lease_expires_at=NULL, updated_at=now()
		 WHERE workflow_id=$1 AND state='pending' AND owner=$2
		RETURNING revision`,
		workflowID, token, state, lastErr).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("finalize action %s: update: %w", workflowID, err)
	}
	if err := reorderActionMessagesTx(ctx, tx, workflowID, revision); err != nil {
		return false, fmt.Errorf("finalize action %s: reorder messages: %w", workflowID, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("finalize action %s: commit: %w", workflowID, err)
	}
	return true, nil
}

func upsertActionMessageTx(
	ctx context.Context, tx *sql.Tx, workflowID, openMessageID string, revision int,
) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO card_action_messages
			(workflow_id, open_message_id, render_kind, desired_revision, update_state)
		VALUES ($1,$2,'action',$3,'pending')
		ON CONFLICT (workflow_id, open_message_id) DO UPDATE SET
			render_kind='action', rejection_reason='', buttons_mode='none',
			desired_revision=$3, update_state='pending',
			reconcile_after=NULL, owner='', lease_expires_at=NULL,
			updated_at=now()`,
		workflowID, openMessageID, revision)
	return err
}

func upsertRejectionMessageTx(
	ctx context.Context, tx *sql.Tx,
	workflowID, openMessageID, reason, buttonsMode string,
) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO card_action_messages
			(workflow_id, open_message_id, render_kind, rejection_reason,
			 buttons_mode, desired_revision, update_state)
		VALUES ($1,$2,'rejection',$3,$4,1,'pending')
		ON CONFLICT (workflow_id, open_message_id) DO UPDATE SET
			render_kind='rejection', rejection_reason=$3, buttons_mode=$4,
			update_state='pending', reconcile_after=NULL,
			owner='', lease_expires_at=NULL, updated_at=now()
		WHERE card_action_messages.render_kind <> 'action'`,
		workflowID, openMessageID, reason, buttonsMode)
	return err
}

func reorderActionMessagesTx(
	ctx context.Context, tx *sql.Tx, workflowID string, revision int,
) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE card_action_messages
		   SET desired_revision=$2, update_state='pending',
		       owner='', lease_expires_at=NULL, reconcile_after=NULL,
		       updated_at=now()
		 WHERE workflow_id=$1`,
		workflowID, revision)
	return err
}

func processInboxTx(ctx context.Context, tx *sql.Tx, eventID string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE card_action_inbox
		   SET state='processed', processed_at=now()
		 WHERE event_id=$1`,
		eventID)
	return err
}

func completeLegacyAcceptTx(ctx context.Context, tx *sql.Tx, inbox InboxRow) error {
	render := RejectRender{
		Code: "NotAuthoritative", RejectionReason: "该运行不是权威记录，无法执行卡片动作",
	}
	buttonsMode, err := validateRejectRender(render)
	if err != nil {
		return err
	}
	if err := insertAuditTx(ctx, tx, AuditRow{
		Actor:  actorAudit(inbox.ActorOpenID),
		Action: rejectionAuditAction(inbox.Action, render.Code),
		Target: inbox.WorkflowID, PayloadDigest: inbox.PayloadDigest, InboxEventID: inbox.EventID,
	}); err != nil {
		return err
	}
	if err := upsertRejectionMessageTx(
		ctx, tx, inbox.WorkflowID, inbox.OpenMessageID, render.RejectionReason, buttonsMode,
	); err != nil {
		return err
	}
	return processInboxTx(ctx, tx, inbox.EventID)
}
