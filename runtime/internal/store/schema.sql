-- Runtime 数据模型(CLAUDE.md §11)。Phase 1.5 先落 artifacts 表;
-- 其余表随 Phase 1.6 workflow 落地。幂等:重复执行本文件无副作用。
CREATE TABLE IF NOT EXISTS artifacts (
    artifact_id     BIGSERIAL PRIMARY KEY,
    project         TEXT        NOT NULL,
    commit_sha      TEXT        NOT NULL,
    pipeline_id     INTEGER     NOT NULL,   -- CI_PIPELINE_IID
    variant         TEXT        NOT NULL,
    build_type      TEXT        NOT NULL,
    url             TEXT        NOT NULL,
    sha256          TEXT        NOT NULL,
    size            BIGINT      NOT NULL,
    manifest_digest TEXT        NOT NULL,   -- 派单时透传 Client 核对(§8.1)
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (commit_sha, pipeline_id, variant)
);
