-- 2026-08-07: artifacts 表补充 version 列(包版本 X.Y.Z)。
-- 背景:test 命令(test <variant> [commit])启动 workflow 时 workflow_runs.version
-- 必填,但 artifacts 表未存 version,bundle/kick 登记时丢失,导致 RecordWorkflowRun
-- 报 "required field is empty"。
-- 迁移:新增列 → 从既有 workflow_runs 按 (project, commit_sha, pipeline_id) 回填
-- → 回填后仍为空的置 '0.0.0'(展示用占位,不应被新登记覆盖)。
ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS version text NOT NULL DEFAULT '';

UPDATE artifacts a SET version = r.version
FROM workflow_runs r
WHERE a.version = ''
  AND r.project = a.project
  AND r.commit_sha = a.commit_sha
  AND r.pipeline_id = a.pipeline_id
  AND r.version <> '';

UPDATE artifacts SET version = '0.0.0'
WHERE version = '';
