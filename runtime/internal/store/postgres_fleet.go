package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	wf "hermes-devops/runtime/internal/workflow"
)

// FleetOverview 汇总 fleet 状态(只读;三次简单查询,非热路径)。
func (s *PGStore) FleetOverview(ctx context.Context) (*FleetOverview, error) {
	out := &FleetOverview{Devices: []DeviceStatus{}}
	if err := s.DB.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT workflow_id) FROM tasks
		WHERE status NOT IN ('COMPLETED','FAILED','TIMEOUT','CANCELED')`).Scan(&out.InflightWorkflows); err != nil {
		return nil, fmt.Errorf("fleet overview: count workflows: %w", err)
	}
	if err := s.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM device_leases WHERE released_at IS NULL`).Scan(&out.ActiveLeases); err != nil {
		return nil, fmt.Errorf("fleet overview: count leases: %w", err)
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT d.device_id, d.serial, d.soc, d.status, d.fail_streak,
		       COALESCE(l.task_id, ''), d.client_id, COALESCE(c.fail_streak, 0)
		FROM devices d
		LEFT JOIN device_leases l ON l.device_id = d.device_id AND l.released_at IS NULL
		LEFT JOIN clients c ON c.client_id = d.client_id
		ORDER BY d.device_id`)
	if err != nil {
		return nil, fmt.Errorf("fleet overview: list devices: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d DeviceStatus
		if err := rows.Scan(&d.DeviceID, &d.Serial, &d.SOC, &d.Status, &d.FailStreak,
			&d.LeaseTaskID, &d.ClientID, &d.ClientFailStreak); err != nil {
			return nil, fmt.Errorf("fleet overview: scan device: %w", err)
		}
		out.Devices = append(out.Devices, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fleet overview: list devices: %w", err)
	}
	return out, nil
}

// UnquarantineDevice 解隔离:status='IDLE'、fail_streak=0(飞书指令
// unquarantine)。设备不存在返回 (false, nil);重复解隔离幂等。
func (s *PGStore) UnquarantineDevice(ctx context.Context, deviceID string) (bool, error) {
	res, err := s.DB.ExecContext(ctx, `
		UPDATE devices SET status = 'IDLE', fail_streak = 0 WHERE device_id = $1`, deviceID)
	if err != nil {
		return false, fmt.Errorf("unquarantine device %s: %w", deviceID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("unquarantine device %s: rows affected: %w", deviceID, err)
	}
	return n > 0, nil
}

// ListArtifacts 是 Task 6 前的无 project 兼容入口。匹配唯一 project 时返回
// 全部产物;跨 project 歧义时 fail closed。
func (s *PGStore) ListArtifacts(ctx context.Context, commitSHA string, pipelineID int) ([]Artifact, error) {
	project, found, err := s.legacyArtifactProject(ctx, commitSHA, pipelineID, nil)
	if err != nil {
		return nil, fmt.Errorf("list artifacts %s/%d: %w", commitSHA, pipelineID, err)
	}
	if !found {
		return []Artifact{}, nil
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT project, commit_sha, pipeline_id, variant, build_type, url, sha256, size, manifest_digest
		FROM artifacts WHERE project = $1 AND commit_sha = $2 AND pipeline_id = $3 ORDER BY variant`,
		project, commitSHA, pipelineID)
	if err != nil {
		return nil, fmt.Errorf("list artifacts %s/%d: %w", commitSHA, pipelineID, err)
	}
	defer rows.Close()
	out := []Artifact{}
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.Project, &a.CommitSHA, &a.PipelineID, &a.Variant,
			&a.BuildType, &a.URL, &a.SHA256, &a.Size, &a.ManifestDigest); err != nil {
			return nil, fmt.Errorf("list artifacts %s/%d: scan: %w", commitSHA, pipelineID, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list artifacts %s/%d: %w", commitSHA, pipelineID, err)
	}
	return out, nil
}

// NextWorkflowAttemptAll 是 Task 6 前的无 project 兼容入口。匹配唯一 project
// 时原子递增全部变体并返回最大值;跨 project 歧义时 fail closed。
func (s *PGStore) NextWorkflowAttemptAll(ctx context.Context, commitSHA string, pipelineID int) (int, error) {
	project, found, err := s.legacyArtifactProject(ctx, commitSHA, pipelineID, nil)
	if err != nil {
		return 0, fmt.Errorf("next workflow attempt %s/%d: %w", commitSHA, pipelineID, err)
	}
	if !found {
		return 0, fmt.Errorf("next workflow attempt: artifact not registered: %s/%d", commitSHA, pipelineID)
	}
	var maxN int
	err = s.DB.QueryRowContext(ctx, `
		WITH bumped AS (
			UPDATE artifacts SET workflow_attempt = workflow_attempt + 1
			WHERE project = $1 AND commit_sha = $2 AND pipeline_id = $3
			RETURNING workflow_attempt
		)
		SELECT COALESCE(MAX(workflow_attempt), 0) FROM bumped`,
		project, commitSHA, pipelineID).Scan(&maxN)
	if err != nil {
		return 0, fmt.Errorf("next workflow attempt %s/%d: %w", commitSHA, pipelineID, err)
	}
	if maxN == 0 {
		return 0, fmt.Errorf("next workflow attempt: artifact not registered: %s/%d", commitSHA, pipelineID)
	}
	return maxN, nil
}

// RecentRuns 见 MemStore 同名方法的语义说明。
func (s *PGStore) RecentRuns(ctx context.Context, limit int) ([]RecentRun, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT wr.workflow_id, wr.project, wr.commit_sha, wr.pipeline_id,
		       wr.version, wr.rule_version, expanded.variant,
		       COALESCE(task.verdict, ''), task.ended_at
		FROM workflow_runs wr
		CROSS JOIN LATERAL unnest(wr.variants) WITH ORDINALITY
			AS expanded(variant, ord)
		LEFT JOIN LATERAL (
			SELECT verdict, ended_at
			FROM tasks
			WHERE workflow_id = wr.workflow_id
			  AND test_id = expanded.variant
			ORDER BY attempt DESC, created_at DESC, task_id DESC
			LIMIT 1
		) task ON true
		ORDER BY wr.created_at DESC, wr.workflow_id DESC, expanded.ord
		LIMIT $1`,
		limit)
	if err != nil {
		return nil, fmt.Errorf("recent runs: authoritative: %w", err)
	}
	out := make([]RecentRun, 0, limit)
	for rows.Next() {
		var r RecentRun
		var endedAt sql.NullTime
		if err := rows.Scan(
			&r.WorkflowID, &r.Project, &r.Commit, &r.PipelineID,
			&r.Version, &r.RuleVersion, &r.Variant, &r.Verdict, &endedAt,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("recent runs: authoritative scan: %w", err)
		}
		if endedAt.Valid {
			r.EndedAt = endedAt.Time.UTC()
		}
		r.Authoritative = true
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recent runs: authoritative: %w", err)
	}
	remaining := limit - len(out)
	if remaining == 0 {
		return out, nil
	}

	rows, err = s.DB.QueryContext(ctx, `
		SELECT a.project, a.commit_sha, a.pipeline_id, a.variant
		FROM artifacts a
		WHERE NOT EXISTS (
			SELECT 1
			FROM workflow_runs wr
			WHERE wr.project = a.project
			  AND wr.commit_sha = a.commit_sha
			  AND wr.pipeline_id = a.pipeline_id
			  AND a.variant = ANY(wr.variants)
		)
		ORDER BY a.created_at DESC, a.artifact_id DESC
		LIMIT $1`,
		remaining)
	if err != nil {
		return nil, fmt.Errorf("recent runs: legacy: %w", err)
	}
	legacyStart := len(out)
	for rows.Next() {
		var r RecentRun
		if err := rows.Scan(&r.Project, &r.Commit, &r.PipelineID, &r.Variant); err != nil {
			rows.Close()
			return nil, fmt.Errorf("recent runs: legacy scan: %w", err)
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recent runs: legacy: %w", err)
	}

	for i := legacyStart; i < len(out); i++ {
		base := wf.BaseWorkflowID(out[i].Project, out[i].Commit, out[i].PipelineID)
		var verdict sql.NullString
		var endedAt sql.NullTime
		err := s.DB.QueryRowContext(ctx, `
			SELECT verdict, ended_at FROM tasks
			WHERE test_id = $1
			  AND (workflow_id = $2 OR starts_with(workflow_id, $2 || '-'))
			ORDER BY created_at DESC, task_id DESC
			LIMIT 1`, out[i].Variant, base).Scan(&verdict, &endedAt)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("recent runs: legacy lookup %s: %w", out[i].Variant, err)
		}
		out[i].Verdict = verdict.String
		if endedAt.Valid {
			out[i].EndedAt = endedAt.Time.UTC()
		}
	}
	return out, nil
}
