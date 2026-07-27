package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/lib/pq"

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

// GetResult 按 task_id 读权威结果(LoadResult 活动,差距清单 #2);
// 不存在返回 (nil, nil)。
func (s *PGStore) GetResult(ctx context.Context, taskID string) (*wf.ResultRecord, error) {
	var body []byte
	err := s.DB.QueryRowContext(ctx,
		`SELECT result_json FROM results WHERE task_id = $1`, taskID).Scan(&body)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get result %s: %w", taskID, err)
	}
	var sig wf.TaskResultSignal
	if err := json.Unmarshal(body, &sig); err != nil {
		return nil, fmt.Errorf("get result %s: unmarshal: %w", taskID, err)
	}
	return &wf.ResultRecord{TaskID: taskID, Result: sig}, nil
}

// ConclusiveWorkflowIDs 返回候选 workflow ID 中已存在结论性测试结果的子集:
// 该 workflow 下有 status='COMPLETED' 且 verdict ∈ ConclusiveVerdicts 的 task。
// 用于 bundle webhook 跳过已测变体(kick 变体级链路已测过的不再重测)。
func (s *PGStore) ConclusiveWorkflowIDs(ctx context.Context, workflowIDs []string) (map[string]bool, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT DISTINCT workflow_id FROM tasks
		WHERE workflow_id = ANY($1) AND status = 'COMPLETED'
		  AND verdict IN ('PASSED', 'TEST_FAILED')`,
		pq.Array(workflowIDs))
	if err != nil {
		return nil, fmt.Errorf("conclusive workflow ids: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("conclusive workflow ids: scan: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("conclusive workflow ids: %w", err)
	}
	return out, nil
}
