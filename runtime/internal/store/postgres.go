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

// RegisterArtifacts 幂等登记:同 (commit,pipeline,variant) 冲突时忽略。
func (s *PGStore) RegisterArtifacts(ctx context.Context, arts []Artifact) error {
	const q = `INSERT INTO artifacts
		(project, commit_sha, pipeline_id, variant, build_type, url, sha256, size, manifest_digest)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (commit_sha, pipeline_id, variant) DO NOTHING`
	for _, a := range arts {
		if _, err := s.DB.ExecContext(ctx, q,
			a.Project, a.CommitSHA, a.PipelineID, a.Variant, a.BuildType,
			a.URL, a.SHA256, a.Size, a.ManifestDigest); err != nil {
			return fmt.Errorf("register artifact %s: %w", a.Variant, err)
		}
	}
	return nil
}
