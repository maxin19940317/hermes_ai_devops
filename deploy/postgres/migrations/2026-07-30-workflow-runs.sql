BEGIN;

CREATE TABLE IF NOT EXISTS workflow_runs (
    workflow_id        TEXT PRIMARY KEY,
    project            TEXT NOT NULL,
    commit_sha         TEXT NOT NULL,
    pipeline_id        INTEGER NOT NULL CHECK (pipeline_id > 0),
    version            TEXT NOT NULL,
    rule_version       TEXT NOT NULL,
    scope              TEXT NOT NULL DEFAULT '',
    attempt            INTEGER NOT NULL CHECK (attempt >= 0),
    variants           TEXT[] NOT NULL,
    source_workflow_id TEXT REFERENCES workflow_runs(workflow_id) ON DELETE RESTRICT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (source_workflow_id IS NULL OR source_workflow_id <> workflow_id)
);

CREATE INDEX IF NOT EXISTS workflow_runs_recent_idx
    ON workflow_runs(created_at DESC, workflow_id DESC);
CREATE INDEX IF NOT EXISTS tasks_run_variant_latest_idx
    ON tasks(workflow_id, test_id, attempt DESC, created_at DESC);

CREATE UNIQUE INDEX IF NOT EXISTS artifacts_project_key
    ON artifacts(project, commit_sha, pipeline_id, variant);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'artifacts'::regclass
          AND conname = 'artifacts_project_key'
    ) THEN
        ALTER TABLE artifacts
            ADD CONSTRAINT artifacts_project_key
            UNIQUE USING INDEX artifacts_project_key;
    END IF;
END $$;

ALTER TABLE artifacts
    DROP CONSTRAINT IF EXISTS artifacts_commit_sha_pipeline_id_variant_key;

COMMIT;
