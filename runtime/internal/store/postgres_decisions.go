package store

import (
	"context"
	"database/sql"
	"fmt"

	wf "hermes-devops/runtime/internal/workflow"
)

// HasDecision 报告某任务是否已有指定 actor 的裁决(升级判重,
// 设计 §5 门槛 3)。
func (s *PGStore) HasDecision(ctx context.Context, taskID, actor string) (bool, error) {
	var one int
	err := s.DB.QueryRowContext(ctx, `
		SELECT 1 FROM decisions WHERE task_id = $1 AND actor = $2 LIMIT 1`,
		taskID, actor).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("has decision %s/%s: %w", taskID, actor, err)
	}
	return true, nil
}

// SaveDecision 落 decisions 表(§11):规则引擎与 LLM 的每次裁决都落表,可回放。
// output 已是 JSON,原样存入 JSONB;task_id 不存在时外键报错。
func (s *PGStore) SaveDecision(ctx context.Context, row wf.DecisionRow) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO decisions (task_id, actor, input_digest, model, prompt_version, output, evidence_snapshot_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		row.TaskID, row.Actor, row.InputDigest, row.Model, row.PromptVersion, row.Output,
		row.EvidenceSnapshotID)
	if err != nil {
		return fmt.Errorf("save decision %s/%s: %w", row.TaskID, row.Actor, err)
	}
	return nil
}

// ListDecisions 按插入序(decision_id 单调递增)返回某任务的全部裁决;
// 无记录返回空切片。
func (s *PGStore) ListDecisions(ctx context.Context, taskID string) ([]wf.DecisionRow, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT task_id, actor, input_digest, model, prompt_version, output, evidence_snapshot_id
		FROM decisions WHERE task_id = $1
		ORDER BY created_at, decision_id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list decisions %s: %w", taskID, err)
	}
	defer rows.Close()
	out := []wf.DecisionRow{}
	for rows.Next() {
		var d wf.DecisionRow
		if err := rows.Scan(&d.TaskID, &d.Actor, &d.InputDigest, &d.Model,
			&d.PromptVersion, &d.Output, &d.EvidenceSnapshotID); err != nil {
			return nil, fmt.Errorf("list decisions %s: scan: %w", taskID, err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list decisions %s: %w", taskID, err)
	}
	return out, nil
}
