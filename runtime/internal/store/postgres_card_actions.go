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
