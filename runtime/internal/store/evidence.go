package store

import (
	"context"
	"database/sql"
	"fmt"
)

// EvidenceSnapshot 对应 evidence_snapshots 表一行(docs/device-test-sequence.md
// 差距 #6,文末表结构):evidence.json 的 MinIO 快照登记,hermes 裁决可回放
// "当时看到了哪些日志片段";原始日志可按生命周期清理,快照随 Decision 保留。
type EvidenceSnapshot struct {
	EvidenceID       string // = task_id(含 attempt,全链路唯一)
	TaskID           string
	Attempt          int
	ObjectKey        string
	SHA256           string
	ExtractorVersion string
}

// SaveEvidenceSnapshot 登记快照;同 evidence_id 重复插入无副作用
// (activity 重试/重复提取幂等)。
func (s *MemStore) SaveEvidenceSnapshot(_ context.Context, snap EvidenceSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.evidenceSnaps[snap.EvidenceID]; !ok {
		s.evidenceSnaps[snap.EvidenceID] = snap
	}
	return nil
}

// GetEvidenceSnapshot 按 evidence_id 读快照;不存在返回 (nil, nil)。
func (s *MemStore) GetEvidenceSnapshot(_ context.Context, evidenceID string) (*EvidenceSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok := s.evidenceSnaps[evidenceID]
	if !ok {
		return nil, nil
	}
	out := snap
	return &out, nil
}

// SaveEvidenceSnapshot 登记快照;同 evidence_id 冲突忽略(幂等)。
func (s *PGStore) SaveEvidenceSnapshot(ctx context.Context, snap EvidenceSnapshot) error {
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO evidence_snapshots
			(evidence_id, task_id, attempt, object_key, sha256, extractor_version)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (evidence_id) DO NOTHING`,
		snap.EvidenceID, snap.TaskID, snap.Attempt, snap.ObjectKey, snap.SHA256,
		snap.ExtractorVersion); err != nil {
		return fmt.Errorf("save evidence snapshot %s: %w", snap.EvidenceID, err)
	}
	return nil
}

// GetEvidenceSnapshot 按 evidence_id 读快照;不存在返回 (nil, nil)。
func (s *PGStore) GetEvidenceSnapshot(ctx context.Context, evidenceID string) (*EvidenceSnapshot, error) {
	var snap EvidenceSnapshot
	err := s.DB.QueryRowContext(ctx, `
		SELECT evidence_id, task_id, attempt, object_key, sha256, extractor_version
		FROM evidence_snapshots WHERE evidence_id = $1`, evidenceID).Scan(
		&snap.EvidenceID, &snap.TaskID, &snap.Attempt, &snap.ObjectKey,
		&snap.SHA256, &snap.ExtractorVersion)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get evidence snapshot %s: %w", evidenceID, err)
	}
	return &snap, nil
}
