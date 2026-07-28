-- 存量库迁移(2026-07-28):飞书指令层自然语言翻译审计(设计文档 §4.3)。
-- 幂等,可重复执行。翻译发生在任何 task 存在之前,无 task_id 可填,
-- 故不能复用 decisions 表(其 task_id 是 NOT NULL 外键)。
-- 追加式:确认流程不更新已有行,pending_confirm 与 confirmed 各占一行,
-- 同 open_id 按时序读就是完整证据链。
-- 执行:docker exec -i hermes-runtime-postgres-1 psql -U hermes_runtime -d hermes_runtime -v ON_ERROR_STOP=1 < 本文件

BEGIN;

CREATE TABLE IF NOT EXISTS command_translations (
    translation_id BIGSERIAL PRIMARY KEY,
    open_id        TEXT        NOT NULL,
    raw_text       TEXT        NOT NULL,
    prompt_version TEXT        NOT NULL DEFAULT '',
    model          TEXT        NOT NULL DEFAULT '',
    context_digest TEXT        NOT NULL DEFAULT '',   -- 快照 sha256,可回放"当时看到了什么"
    output         JSONB       NOT NULL DEFAULT '{}', -- LLM 原始输出(校验失败也存,落库前截断 4KB)
    rendered       TEXT        NOT NULL DEFAULT '',   -- 渲染出的那行指令
    outcome        TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS command_translations_open_id_idx
    ON command_translations(open_id, created_at DESC);

COMMIT;
