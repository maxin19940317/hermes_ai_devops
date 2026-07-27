-- 存量库迁移(2026-07-27):Evidence 快照持久化(设计基线 v1.0 差距 #6)。
-- 幂等,可重复执行。evidence.json(≤96KB)上传 MinIO,evidence_snapshots 登记,
-- decisions 通过 evidence_snapshot_id 引用快照(actor=hermes 的决策才带,rule 为空)。
-- 执行:docker exec -i hermes-runtime-postgres-1 psql -U hermes_runtime -d hermes_runtime -v ON_ERROR_STOP=1 < 本文件

BEGIN;

CREATE TABLE IF NOT EXISTS evidence_snapshots (
    evidence_id       TEXT PRIMARY KEY,
    task_id           TEXT        NOT NULL,
    attempt           INTEGER     NOT NULL DEFAULT 0,
    object_key        TEXT        NOT NULL,
    sha256            TEXT        NOT NULL,
    extractor_version TEXT        NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS evidence_snapshots_task_id_idx ON evidence_snapshots(task_id);

ALTER TABLE decisions ADD COLUMN IF NOT EXISTS evidence_snapshot_id TEXT NOT NULL DEFAULT '';

COMMIT;
