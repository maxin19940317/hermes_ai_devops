package store

import (
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

type workflowRunsSchemaShape struct {
	Columns     []string
	Constraints []string
	Indexes     []string
}

func captureWorkflowRunsSchemaShape(t *testing.T, db *sql.DB) workflowRunsSchemaShape {
	t.Helper()
	var shape workflowRunsSchemaShape

	rows, err := db.QueryContext(ctx, `
		SELECT column_name, data_type, udt_name, is_nullable, COALESCE(column_default, '')
		FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'workflow_runs'
		ORDER BY ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name, dataType, udtName, nullable, defaultValue string
		if err := rows.Scan(&name, &dataType, &udtName, &nullable, &defaultValue); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		shape.Columns = append(shape.Columns,
			fmt.Sprintf("%s|%s|%s|%s|%s", name, dataType, udtName, nullable, defaultValue))
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}

	rows, err = db.QueryContext(ctx, `
		SELECT c.conrelid::regclass::text, c.conname, c.contype::text,
		       pg_get_constraintdef(c.oid, true)
		FROM pg_constraint c
		WHERE (c.conrelid = to_regclass('workflow_runs'))
		   OR (c.conrelid = 'artifacts'::regclass AND c.conname IN (
		       'artifacts_project_key',
		       'artifacts_commit_sha_pipeline_id_variant_key'))
		ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var tableName, name, constraintType, definition string
		if err := rows.Scan(&tableName, &name, &constraintType, &definition); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		shape.Constraints = append(shape.Constraints,
			fmt.Sprintf("%s|%s|%s|%s", tableName, name, constraintType, definition))
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}

	rows, err = db.QueryContext(ctx, `
		SELECT tablename, indexname, indexdef
		FROM pg_indexes
		WHERE schemaname = current_schema()
		  AND indexname IN ('workflow_runs_recent_idx', 'tasks_run_variant_latest_idx')
		ORDER BY indexname`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var tableName, name, definition string
		if err := rows.Scan(&tableName, &name, &definition); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		shape.Indexes = append(shape.Indexes,
			fmt.Sprintf("%s|%s|%s", tableName, name, definition))
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	return shape
}

func assertFreshWorkflowRunsShape(t *testing.T, shape workflowRunsSchemaShape) {
	t.Helper()
	wantColumns := []string{
		"workflow_id|text|text|NO|",
		"project|text|text|NO|",
		"commit_sha|text|text|NO|",
		"pipeline_id|integer|int4|NO|",
		"version|text|text|NO|",
		"rule_version|text|text|NO|",
		"scope|text|text|NO|''::text",
		"attempt|integer|int4|NO|",
		"variants|ARRAY|_text|NO|",
		"source_workflow_id|text|text|YES|",
		"created_at|timestamp with time zone|timestamptz|NO|now()",
	}
	if !reflect.DeepEqual(shape.Columns, wantColumns) {
		t.Fatalf("workflow_runs columns = %#v, want %#v", shape.Columns, wantColumns)
	}
	joinedConstraints := strings.Join(shape.Constraints, "\n")
	for _, want := range []string{
		"artifacts|artifacts_project_key|u|UNIQUE (project, commit_sha, pipeline_id, variant)",
		"workflow_runs|workflow_runs_pkey|p|PRIMARY KEY (workflow_id)",
		"FOREIGN KEY (source_workflow_id) REFERENCES workflow_runs(workflow_id) ON DELETE RESTRICT",
		"CHECK (pipeline_id > 0)",
		"CHECK (attempt >= 0)",
		"CHECK (source_workflow_id IS NULL OR source_workflow_id <> workflow_id)",
	} {
		if !strings.Contains(joinedConstraints, want) {
			t.Errorf("constraints missing %q:\n%s", want, joinedConstraints)
		}
	}
	if strings.Contains(joinedConstraints, "artifacts_commit_sha_pipeline_id_variant_key") {
		t.Errorf("legacy artifact constraint still exists:\n%s", joinedConstraints)
	}
	if len(shape.Indexes) != 2 {
		t.Fatalf("indexes = %#v, want both workflow/task indexes", shape.Indexes)
	}
	joinedIndexes := strings.Join(shape.Indexes, "\n")
	for _, want := range []string{
		"workflow_runs|workflow_runs_recent_idx",
		"tasks|tasks_run_variant_latest_idx",
	} {
		if !strings.Contains(joinedIndexes, want) {
			t.Errorf("indexes missing %q:\n%s", want, joinedIndexes)
		}
	}
}

func TestWorkflowRunsMigrationUpgradesLegacyArtifactKey(t *testing.T) {
	s := openTestPG(t)
	fresh := captureWorkflowRunsSchemaShape(t, s.DB)
	assertFreshWorkflowRunsShape(t, fresh)

	for _, statement := range []string{
		`DROP TABLE workflow_runs`,
		`ALTER TABLE artifacts DROP CONSTRAINT artifacts_project_key`,
		`ALTER TABLE artifacts ADD CONSTRAINT artifacts_commit_sha_pipeline_id_variant_key
			UNIQUE (commit_sha, pipeline_id, variant)`,
	} {
		if _, err := s.DB.ExecContext(ctx, statement); err != nil {
			t.Fatalf("degrade schema with %q: %v", statement, err)
		}
	}

	migration, err := os.ReadFile("../../../deploy/postgres/migrations/2026-07-30-workflow-runs.sql")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.DB.ExecContext(ctx, string(migration)); err != nil {
			t.Fatalf("migration run %d: %v", i+1, err)
		}
	}
	upgraded := captureWorkflowRunsSchemaShape(t, s.DB)
	if !reflect.DeepEqual(upgraded, fresh) {
		t.Fatalf("upgraded schema differs from fresh:\nfresh=%#v\nupgraded=%#v", fresh, upgraded)
	}

	const insert = `INSERT INTO artifacts
		(project, commit_sha, pipeline_id, variant, build_type, url, sha256, size, manifest_digest)
		VALUES ($1, 'feedbeef', 73, 'v1', 'Release', $2, 'sha', 1, 'manifest')`
	if _, err := s.DB.ExecContext(ctx, insert, "grp/a", "a"); err != nil {
		t.Fatalf("insert first project: %v", err)
	}
	if _, err := s.DB.ExecContext(ctx, insert, "grp/b", "b"); err != nil {
		t.Fatalf("same artifact identity in second project should succeed: %v", err)
	}
	if _, err := s.DB.ExecContext(ctx, insert, "grp/a", "duplicate"); err == nil {
		t.Fatal("same artifact identity in one project should fail")
	}
}
