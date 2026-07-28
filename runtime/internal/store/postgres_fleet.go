package store

import (
	"context"
	"fmt"
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
		       COALESCE(l.task_id, '')
		FROM devices d
		LEFT JOIN device_leases l ON l.device_id = d.device_id AND l.released_at IS NULL
		ORDER BY d.device_id`)
	if err != nil {
		return nil, fmt.Errorf("fleet overview: list devices: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d DeviceStatus
		if err := rows.Scan(&d.DeviceID, &d.Serial, &d.SOC, &d.Status, &d.FailStreak, &d.LeaseTaskID); err != nil {
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

// ListArtifacts 返回 (commit,pipeline) 逻辑键下的全部产物行(飞书指令 rerun
// 重建 DeviceTestInput 用);无记录返回空切片。
func (s *PGStore) ListArtifacts(ctx context.Context, commitSHA string, pipelineID int) ([]Artifact, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT project, commit_sha, pipeline_id, variant, build_type, url, sha256, size, manifest_digest
		FROM artifacts WHERE commit_sha = $1 AND pipeline_id = $2 ORDER BY variant`,
		commitSHA, pipelineID)
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

// NextWorkflowAttemptAll 把 (commit,pipeline) 键下全部产物行的 workflow_attempt
// 原子 +1,返回新的最大值(bundle 级显式 rerun 的 -r{N} 后缀来源)。
// 单条 UPDATE 原子递增;键下无记录返回错误。
func (s *PGStore) NextWorkflowAttemptAll(ctx context.Context, commitSHA string, pipelineID int) (int, error) {
	var maxN int
	err := s.DB.QueryRowContext(ctx, `
		WITH bumped AS (
			UPDATE artifacts SET workflow_attempt = workflow_attempt + 1
			WHERE commit_sha = $1 AND pipeline_id = $2
			RETURNING workflow_attempt
		)
		SELECT COALESCE(MAX(workflow_attempt), 0) FROM bumped`,
		commitSHA, pipelineID).Scan(&maxN)
	if err != nil {
		return 0, fmt.Errorf("next workflow attempt %s/%d: %w", commitSHA, pipelineID, err)
	}
	if maxN == 0 {
		return 0, fmt.Errorf("next workflow attempt: artifact not registered: %s/%d", commitSHA, pipelineID)
	}
	return maxN, nil
}
