package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	wf "hermes-devops/runtime/internal/workflow"
)

// CreateTask 登记任务;同幂等键(即 task_id)重复创建无副作用(§3 规则 7)。
func (s *PGStore) CreateTask(ctx context.Context, row wf.TaskRow) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO tasks (task_id, workflow_id, test_id, attempt, idempotency_key, client_id, device_id, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (task_id) DO NOTHING`,
		row.TaskID, row.WorkflowID, row.TestID, row.Attempt, row.IdempotencyKey,
		row.ClientID, row.DeviceID, row.Status)
	if err != nil {
		return fmt.Errorf("create task %s: %w", row.TaskID, err)
	}
	return nil
}

// GetTask 返回任务行副本;不存在返回 (nil, nil)。
func (s *PGStore) GetTask(ctx context.Context, taskID string) (*wf.TaskRow, error) {
	var row wf.TaskRow
	err := s.DB.QueryRowContext(ctx, `
		SELECT task_id, workflow_id, test_id, attempt, idempotency_key, client_id, device_id, status
		FROM tasks WHERE task_id = $1`, taskID).Scan(
		&row.TaskID, &row.WorkflowID, &row.TestID, &row.Attempt, &row.IdempotencyKey,
		&row.ClientID, &row.DeviceID, &row.Status)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get task %s: %w", taskID, err)
	}
	return &row, nil
}

// SetTaskStatus 更新生命周期状态(status 与 verdict 正交,§9)。
// 任务不存在时 UPDATE 空转,无副作用。
func (s *PGStore) SetTaskStatus(ctx context.Context, taskID, status string) error {
	if _, err := s.DB.ExecContext(ctx, `UPDATE tasks SET status = $2 WHERE task_id = $1`,
		taskID, status); err != nil {
		return fmt.Errorf("set task status %s: %w", taskID, err)
	}
	return nil
}

// FinishTask 落终态 status 与裁决(verdict/category/reason)。
func (s *PGStore) FinishTask(ctx context.Context, req wf.FinishRequest) error {
	if _, err := s.DB.ExecContext(ctx, `
		UPDATE tasks SET status = $2, verdict = $3, error_category = $4, reason = $5, ended_at = now()
		WHERE task_id = $1`,
		req.TaskID, req.Status, req.Verdict, req.Category, req.Reason); err != nil {
		return fmt.Errorf("finish task %s: %w", req.TaskID, err)
	}
	return nil
}

// AppendTaskEvent 追加状态迁移事件;重复 (task_id, seq) 去重(回调可能重发,§8.2)。
// 返回是否实际插入。
func (s *PGStore) AppendTaskEvent(ctx context.Context, ev TaskEvent) (bool, error) {
	res, err := s.DB.ExecContext(ctx, `
		INSERT INTO task_events (task_id, seq, from_status, to_status)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (task_id, seq) DO NOTHING`,
		ev.TaskID, ev.Seq, ev.From, ev.To)
	if err != nil {
		return false, fmt.Errorf("append task event %s#%d: %w", ev.TaskID, ev.Seq, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("append task event %s#%d: rows affected: %w", ev.TaskID, ev.Seq, err)
	}
	return n > 0, nil
}

// SaveResult 落 results 表;同 task_id 重复回传去重(§8.2)。返回是否实际插入。
func (s *PGStore) SaveResult(ctx context.Context, rec wf.ResultRecord) (bool, error) {
	body, err := json.Marshal(rec.Result)
	if err != nil {
		return false, fmt.Errorf("save result %s: marshal: %w", rec.TaskID, err)
	}
	res, err := s.DB.ExecContext(ctx, `
		INSERT INTO results (task_id, result_json) VALUES ($1, $2)
		ON CONFLICT (task_id) DO NOTHING`,
		rec.TaskID, body)
	if err != nil {
		return false, fmt.Errorf("save result %s: %w", rec.TaskID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("save result %s: rows affected: %w", rec.TaskID, err)
	}
	return n > 0, nil
}
