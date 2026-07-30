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

// NextWorkflowAttempt 是 Task 6 前的无 project 兼容入口。匹配唯一 project 时
// 原子递增 workflow_attempt;跨 project 歧义时 fail closed,不递增任何行。
func (s *PGStore) NextWorkflowAttempt(ctx context.Context, commitSHA string, pipelineID int, variant string) (int, error) {
	project, found, err := s.legacyArtifactProject(ctx, commitSHA, pipelineID, &variant)
	if err != nil {
		return 0, fmt.Errorf("next workflow attempt %s/%d/%s: %w", commitSHA, pipelineID, variant, err)
	}
	if !found {
		return 0, fmt.Errorf("next workflow attempt: artifact not registered: %s/%d/%s",
			commitSHA, pipelineID, variant)
	}
	var n int
	err = s.DB.QueryRowContext(ctx, `
		UPDATE artifacts SET workflow_attempt = workflow_attempt + 1
		WHERE project = $1 AND commit_sha = $2 AND pipeline_id = $3 AND variant = $4
		RETURNING workflow_attempt`,
		project, commitSHA, pipelineID, variant).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("next workflow attempt: artifact not registered: %s/%d/%s",
			commitSHA, pipelineID, variant)
	}
	if err != nil {
		return 0, fmt.Errorf("next workflow attempt %s/%d/%s: %w", commitSHA, pipelineID, variant, err)
	}
	return n, nil
}

func (s *PGStore) legacyArtifactProject(
	ctx context.Context, commitSHA string, pipelineID int, variant *string,
) (string, bool, error) {
	q := `SELECT DISTINCT project FROM artifacts
		WHERE commit_sha = $1 AND pipeline_id = $2 ORDER BY project LIMIT 2`
	args := []any{commitSHA, pipelineID}
	if variant != nil {
		q = `SELECT DISTINCT project FROM artifacts
			WHERE commit_sha = $1 AND pipeline_id = $2 AND variant = $3
			ORDER BY project LIMIT 2`
		args = append(args, *variant)
	}
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	var projects []string
	for rows.Next() {
		var project string
		if err := rows.Scan(&project); err != nil {
			return "", false, err
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	if len(projects) > 1 {
		return "", false, fmt.Errorf("artifact identity spans multiple projects")
	}
	if len(projects) == 0 {
		return "", false, nil
	}
	return projects[0], true, nil
}

// RegisterArtifacts 幂等登记:同 (project,commit,pipeline,variant) 冲突时忽略。
func (s *PGStore) RegisterArtifacts(ctx context.Context, arts []Artifact) error {
	const q = `INSERT INTO artifacts
		(project, commit_sha, pipeline_id, variant, build_type, url, sha256, size, manifest_digest)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT ON CONSTRAINT artifacts_project_key DO NOTHING`
	for _, a := range arts {
		if _, err := s.DB.ExecContext(ctx, q,
			a.Project, a.CommitSHA, a.PipelineID, a.Variant, a.BuildType,
			a.URL, a.SHA256, a.Size, a.ManifestDigest); err != nil {
			return fmt.Errorf("register artifact %s: %w", a.Variant, err)
		}
	}
	return nil
}
