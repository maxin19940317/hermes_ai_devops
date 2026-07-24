package store

import (
	"context"
	"encoding/json"
	"fmt"

	wf "hermes-devops/runtime/internal/workflow"
)

// SaveResultWithOutbox 单事务写 results + outbox(原则 3:消灭"写库成功但
// signal 失败"的双写窗口)。两边均幂等:同 task_id 结果 ON CONFLICT 去重,
// 同 event_key 事件不产生第二行;返回结果行是否实际插入。
func (s *PGStore) SaveResultWithOutbox(ctx context.Context, rec wf.ResultRecord, ev OutboxEvent) (bool, error) {
	body, err := json.Marshal(rec.Result)
	if err != nil {
		return false, fmt.Errorf("save result %s: marshal: %w", rec.TaskID, err)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("save result %s: begin tx: %w", rec.TaskID, err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO results (task_id, result_json) VALUES ($1, $2)
		ON CONFLICT (task_id) DO NOTHING`,
		rec.TaskID, body)
	if err != nil {
		return false, fmt.Errorf("save result %s: %w", rec.TaskID, err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("save result %s: rows affected: %w", rec.TaskID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO outbox (aggregate_type, aggregate_id, event_type, event_key, payload)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (event_key) DO NOTHING`,
		ev.AggregateType, ev.AggregateID, ev.EventType, ev.EventKey, ev.Payload); err != nil {
		return false, fmt.Errorf("save result %s: outbox %s: %w", rec.TaskID, ev.EventKey, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("save result %s: commit: %w", rec.TaskID, err)
	}
	return inserted > 0, nil
}

// ClaimUnpublished 按 id 序返回未投递事件(至多 limit 条)。
// Relay 当前单实例运行,不加行锁;多实例化时需改 FOR UPDATE SKIP LOCKED。
func (s *PGStore) ClaimUnpublished(ctx context.Context, limit int) ([]OutboxEvent, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, aggregate_type, aggregate_id, event_type, event_key, payload, attempts, last_error
		FROM outbox WHERE published_at IS NULL ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("claim unpublished outbox: %w", err)
	}
	defer rows.Close()
	out := []OutboxEvent{}
	for rows.Next() {
		var ev OutboxEvent
		if err := rows.Scan(&ev.ID, &ev.AggregateType, &ev.AggregateID, &ev.EventType,
			&ev.EventKey, &ev.Payload, &ev.Attempts, &ev.LastError); err != nil {
			return nil, fmt.Errorf("claim unpublished outbox: scan: %w", err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim unpublished outbox: %w", err)
	}
	return out, nil
}

// MarkPublished 标记投递成功;只作用于未投递行,重复标记幂等。
// 允许 Signal 成功与标记之间崩溃:重投由接收端幂等兜底(docs 文末说明)。
func (s *PGStore) MarkPublished(ctx context.Context, id int64) error {
	if _, err := s.DB.ExecContext(ctx, `
		UPDATE outbox SET published_at = now() WHERE id = $1 AND published_at IS NULL`, id); err != nil {
		return fmt.Errorf("mark outbox %d published: %w", id, err)
	}
	return nil
}

// MarkFailed 记录投递失败(attempts+1 + last_error),留待下轮重试;
// 只作用于未投递行(已投递行是终态,迟到的失败上报不得改它)。
func (s *PGStore) MarkFailed(ctx context.Context, id int64, cause string) error {
	if _, err := s.DB.ExecContext(ctx, `
		UPDATE outbox SET attempts = attempts + 1, last_error = $2
		WHERE id = $1 AND published_at IS NULL`, id, cause); err != nil {
		return fmt.Errorf("mark outbox %d failed: %w", id, err)
	}
	return nil
}
