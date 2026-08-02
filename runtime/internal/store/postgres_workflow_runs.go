package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lib/pq"
)

func workflowRunPGError(operation string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23502", "23503", "23505", "23514":
			return fmt.Errorf("%w: %s: %w", ErrWorkflowRunPermanent, operation, err)
		}
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func scanWorkflowRun(scanner interface{ Scan(...any) error }) (WorkflowRun, error) {
	var run WorkflowRun
	var source sql.NullString
	err := scanner.Scan(
		&run.WorkflowID,
		&run.Project,
		&run.CommitSHA,
		&run.PipelineID,
		&run.Version,
		&run.RuleVersion,
		&run.Scope,
		&run.Attempt,
		pq.Array(&run.Variants),
		&source,
		&run.CreatedAt,
	)
	if source.Valid {
		run.SourceWorkflowID = source.String
	}
	return run, err
}

const workflowRunColumns = `
	workflow_id, project, commit_sha, pipeline_id, version, rule_version,
	scope, attempt, variants, source_workflow_id, created_at`

func (s *PGStore) RecordWorkflowRun(ctx context.Context, run WorkflowRun) error {
	canonical, err := canonicalWorkflowRun(run)
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("record workflow run %s: begin: %w", canonical.WorkflowID, err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_runs
			(workflow_id, project, commit_sha, pipeline_id, version, rule_version,
			 scope, attempt, variants, source_workflow_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''))
		ON CONFLICT (workflow_id) DO NOTHING`,
		canonical.WorkflowID,
		canonical.Project,
		canonical.CommitSHA,
		canonical.PipelineID,
		canonical.Version,
		canonical.RuleVersion,
		canonical.Scope,
		canonical.Attempt,
		pq.Array(canonical.Variants),
		canonical.SourceWorkflowID,
	)
	if err != nil {
		return workflowRunPGError("insert workflow run "+canonical.WorkflowID, err)
	}

	stored, err := scanWorkflowRun(tx.QueryRowContext(ctx,
		`SELECT `+workflowRunColumns+` FROM workflow_runs WHERE workflow_id = $1`,
		canonical.WorkflowID))
	if err != nil {
		return workflowRunPGError("read workflow run "+canonical.WorkflowID, err)
	}
	if !workflowRunImmutableEqual(stored, canonical) {
		return fmt.Errorf("%w: %s", ErrWorkflowRunConflict, canonical.WorkflowID)
	}
	if err := tx.Commit(); err != nil {
		return workflowRunPGError("commit workflow run "+canonical.WorkflowID, err)
	}
	return nil
}

func (s *PGStore) GetWorkflowRun(ctx context.Context, workflowID string) (*WorkflowRun, error) {
	run, err := scanWorkflowRun(s.DB.QueryRowContext(ctx,
		`SELECT `+workflowRunColumns+` FROM workflow_runs WHERE workflow_id = $1`,
		workflowID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrWorkflowRunNotFound, workflowID)
	}
	if err != nil {
		return nil, workflowRunPGError("get workflow run "+workflowID, err)
	}
	run = cloneWorkflowRun(run)
	return &run, nil
}
