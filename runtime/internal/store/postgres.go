package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql 驱动
)

//go:embed schema.sql
var schemaSQL string

// PGStore 是 ArtifactStore 的 Postgres 实现(§11 artifacts 表)。
type PGStore struct {
	DB *sql.DB
}

// OpenPG 连接 Postgres 并应用 schema(幂等)。
func OpenPG(ctx context.Context, dsn string) (*PGStore, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &PGStore{DB: db}, nil
}

// NextWorkflowAttempt 原子递增指定 project 的 workflow_attempt。
func (s *PGStore) NextWorkflowAttempt(
	ctx context.Context, project, commitSHA string, pipelineID int, variant string,
) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `
		UPDATE artifacts SET workflow_attempt = workflow_attempt + 1
		WHERE project = $1 AND commit_sha = $2 AND pipeline_id = $3 AND variant = $4
		RETURNING workflow_attempt`,
		project, commitSHA, pipelineID, variant).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("next workflow attempt: artifact not registered: %s/%s/%d/%s",
			project, commitSHA, pipelineID, variant)
	}
	if err != nil {
		return 0, fmt.Errorf("next workflow attempt %s/%s/%d/%s: %w",
			project, commitSHA, pipelineID, variant, err)
	}
	return n, nil
}

// CurrentWorkflowAttempt 只读当前 retry 计数,不递增。供重试前的
// 防连点检查:先窥视 attempt 派生最新重试 ID 查 Temporal 开关状态,
// 运行中则不再分配新 attempt(认领语义,防卡片按钮连点)。
func (s *PGStore) CurrentWorkflowAttempt(
	ctx context.Context, project, commitSHA string, pipelineID int, variant string,
) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `
		SELECT workflow_attempt FROM artifacts
		WHERE project = $1 AND commit_sha = $2 AND pipeline_id = $3 AND variant = $4`,
		project, commitSHA, pipelineID, variant).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("current workflow attempt: artifact not registered: %s/%s/%d/%s",
			project, commitSHA, pipelineID, variant)
	}
	if err != nil {
		return 0, fmt.Errorf("current workflow attempt %s/%s/%d/%s: %w",
			project, commitSHA, pipelineID, variant, err)
	}
	return n, nil
}

// RegisterArtifacts 幂等登记:同 (project,commit,pipeline,variant) 冲突时忽略。
func (s *PGStore) RegisterArtifacts(ctx context.Context, arts []Artifact) error {
	const q = `INSERT INTO artifacts
		(project, commit_sha, pipeline_id, variant, build_type, version, url, sha256, size, manifest_digest)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT ON CONSTRAINT artifacts_project_key DO NOTHING`
	for _, a := range arts {
		if _, err := s.DB.ExecContext(ctx, q,
			a.Project, a.CommitSHA, a.PipelineID, a.Variant, a.BuildType,
			a.Version, a.URL, a.SHA256, a.Size, a.ManifestDigest); err != nil {
			return fmt.Errorf("register artifact %s: %w", a.Variant, err)
		}
	}
	return nil
}
