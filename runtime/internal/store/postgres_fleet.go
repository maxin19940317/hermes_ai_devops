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
		SELECT d.device_id, d.serial, d.display_name, d.soc, d.status, d.fail_streak,
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
		if err := rows.Scan(&d.DeviceID, &d.Serial, &d.DisplayName, &d.SOC, &d.Status, &d.FailStreak,
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

// QuarantineDevice 手动隔离(飞书指令 quarantine):status='QUARANTINED'。
// 设备不存在返回 (false, nil);运行中(BUSY)不隔离返回 false;已隔离幂等。
func (s *PGStore) QuarantineDevice(ctx context.Context, deviceID string) (bool, error) {
	res, err := s.DB.ExecContext(ctx, `
		UPDATE devices SET status = 'QUARANTINED'
		WHERE device_id = $1 AND status <> 'BUSY'`, deviceID)
	if err != nil {
		return false, fmt.Errorf("quarantine device %s: %w", deviceID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("quarantine device %s: rows affected: %w", deviceID, err)
	}
	return n > 0, nil
}

// ListArtifacts 返回指定 project/commit/pipeline 的全部产物。
func (s *PGStore) ListArtifacts(
	ctx context.Context, project, commitSHA string, pipelineID int,
) ([]Artifact, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT project, commit_sha, pipeline_id, variant, build_type, version, url, sha256, size, manifest_digest
		FROM artifacts WHERE project = $1 AND commit_sha = $2 AND pipeline_id = $3 ORDER BY variant`,
		project, commitSHA, pipelineID)
	if err != nil {
		return nil, fmt.Errorf("list artifacts %s/%s/%d: %w", project, commitSHA, pipelineID, err)
	}
	defer rows.Close()
	out := []Artifact{}
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.Project, &a.CommitSHA, &a.PipelineID, &a.Variant,
			&a.BuildType, &a.Version, &a.URL, &a.SHA256, &a.Size, &a.ManifestDigest); err != nil {
			return nil, fmt.Errorf("list artifacts %s/%s/%d: scan: %w",
				project, commitSHA, pipelineID, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list artifacts %s/%s/%d: %w", project, commitSHA, pipelineID, err)
	}
	return out, nil
}

// ListArtifactsForVariant 返回指定变体最近 limit 条产物(created_at 倒序,不限
// project)。供飞书指令 artifacts 查询构建历史。
func (s *PGStore) ListArtifactsForVariant(
	ctx context.Context, variant string, limit int,
) ([]Artifact, error) {
	if limit <= 0 || limit > 100 {
		limit = 20 // 防滥用:飞书指令最多看 20 条
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT project, commit_sha, pipeline_id, variant, build_type, version, url, sha256, size, manifest_digest
		FROM artifacts WHERE variant = $1
		ORDER BY created_at DESC, artifact_id DESC LIMIT $2`,
		variant, limit)
	if err != nil {
		return nil, fmt.Errorf("list artifacts for variant %s: %w", variant, err)
	}
	defer rows.Close()
	out := []Artifact{}
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.Project, &a.CommitSHA, &a.PipelineID, &a.Variant,
			&a.BuildType, &a.Version, &a.URL, &a.SHA256, &a.Size, &a.ManifestDigest); err != nil {
			return nil, fmt.Errorf("list artifacts for variant %s: scan: %w", variant, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list artifacts for variant %s: %w", variant, err)
	}
	return out, nil
}

// LatestArtifactForVariant 返回指定变体最近一次构建(按 created_at 最新,不限
// project);无记录返回 (nil, nil)。供 test 命令缺省 commit 时定位"最近构建"。
func (s *PGStore) LatestArtifactForVariant(
	ctx context.Context, variant string,
) (*Artifact, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT project, commit_sha, pipeline_id, variant, build_type, version, url, sha256, size, manifest_digest
		FROM artifacts WHERE variant = $1
		ORDER BY created_at DESC, artifact_id DESC LIMIT 1`,
		variant)
	var a Artifact
	err := row.Scan(&a.Project, &a.CommitSHA, &a.PipelineID, &a.Variant,
		&a.BuildType, &a.Version, &a.URL, &a.SHA256, &a.Size, &a.ManifestDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest artifact %s: %w", variant, err)
	}
	return &a, nil
}

// NextWorkflowAttemptAll 锁定指定 project/commit/pipeline 的全部变体，把它们
// 原子推进到当前最大值的下一水位，确保并发分配不会复用序号。
func (s *PGStore) NextWorkflowAttemptAll(
	ctx context.Context, project, commitSHA string, pipelineID int,
) (int, error) {
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("next workflow attempt %s/%s/%d: begin: %w",
			project, commitSHA, pipelineID, err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
		SELECT workflow_attempt
		FROM artifacts
		WHERE project = $1 AND commit_sha = $2 AND pipeline_id = $3
		ORDER BY variant
		FOR UPDATE`,
		project, commitSHA, pipelineID)
	if err != nil {
		return 0, fmt.Errorf("next workflow attempt %s/%s/%d: lock: %w",
			project, commitSHA, pipelineID, err)
	}
	maxN := -1
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return 0, fmt.Errorf("next workflow attempt %s/%s/%d: scan: %w",
				project, commitSHA, pipelineID, err)
		}
		if n > maxN {
			maxN = n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("next workflow attempt %s/%s/%d: lock: %w",
			project, commitSHA, pipelineID, err)
	}
	if maxN < 0 {
		return 0, fmt.Errorf("next workflow attempt: artifact not registered: %s/%s/%d",
			project, commitSHA, pipelineID)
	}
	target := maxN + 1
	if _, err := tx.ExecContext(ctx, `
		UPDATE artifacts SET workflow_attempt = $4
		WHERE project = $1 AND commit_sha = $2 AND pipeline_id = $3`,
		project, commitSHA, pipelineID, target); err != nil {
		return 0, fmt.Errorf("next workflow attempt %s/%s/%d: update: %w",
			project, commitSHA, pipelineID, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("next workflow attempt %s/%s/%d: commit: %w",
			project, commitSHA, pipelineID, err)
	}
	return target, nil
}

// RecentRuns 见 MemStore 同名方法的语义说明。
func (s *PGStore) RecentRuns(ctx context.Context, limit int) ([]RecentRun, error) {
	if limit <= 0 {
		return nil, nil
	}
	return s.recentRuns(ctx, limit, nil)
}

func (s *PGStore) recentRuns(
	ctx context.Context, limit int, afterAuthoritative func() error,
) ([]RecentRun, error) {
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("recent runs: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
		SELECT wr.workflow_id, wr.project, wr.commit_sha, wr.pipeline_id,
		       wr.version, wr.rule_version, expanded.variant,
		       COALESCE(task.verdict, ''), task.ended_at,
		       (task.task_id IS NOT NULL)
		FROM workflow_runs wr
		CROSS JOIN LATERAL unnest(wr.variants) WITH ORDINALITY
			AS expanded(variant, ord)
		LEFT JOIN LATERAL (
			SELECT task_id, verdict, ended_at
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
		var hasTask bool
		if err := rows.Scan(
			&r.WorkflowID, &r.Project, &r.Commit, &r.PipelineID,
			&r.Version, &r.RuleVersion, &r.Variant, &r.Verdict, &endedAt, &hasTask,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("recent runs: authoritative scan: %w", err)
		}
		if endedAt.Valid {
			r.EndedAt = endedAt.Time.UTC()
		}
		r.Authoritative = true
		r.HasTask = hasTask
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recent runs: authoritative: %w", err)
	}
	if afterAuthoritative != nil {
		if err := afterAuthoritative(); err != nil {
			return nil, fmt.Errorf("recent runs: after authoritative: %w", err)
		}
	}
	remaining := limit - len(out)
	if remaining == 0 {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("recent runs: commit: %w", err)
		}
		return out, nil
	}

	rows, err = tx.QueryContext(ctx, `
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
		err := tx.QueryRowContext(ctx, `
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
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("recent runs: commit: %w", err)
	}
	return out, nil
}
