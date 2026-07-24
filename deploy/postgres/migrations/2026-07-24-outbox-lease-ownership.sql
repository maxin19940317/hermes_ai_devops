-- 存量库迁移(2026-07-24):事务性 Outbox + 租约所有权凭据 + 显式 retry 计数。
-- 背景:runtime OpenPG 启动时只执行 CREATE TABLE IF NOT EXISTS,对存量表不会补新列;
-- 本脚本幂等,可在 q-uat 库重复执行。对应 docs/device-test-sequence.md v1.0 差距
-- #1(outbox)/#11(workflow_attempt)/#15(lease 所有权)。
-- 执行:docker exec -i hermes-runtime-postgres-1 psql -U hermes_runtime -d hermes_runtime -v ON_ERROR_STOP=1 < 本文件

BEGIN;

-- 差距 #15:租约所有权凭据(心跳条件续租 + LEASE_NOT_OWNED)
ALTER TABLE device_leases ADD COLUMN IF NOT EXISTS lease_id         TEXT        NOT NULL DEFAULT '';
ALTER TABLE device_leases ADD COLUMN IF NOT EXISTS lease_generation INTEGER     NOT NULL DEFAULT 0;
ALTER TABLE device_leases ADD COLUMN IF NOT EXISTS released_at      TIMESTAMPTZ;

-- 差距 #11:显式 retry 的 -r{N} 后缀来源
ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS workflow_attempt INTEGER NOT NULL DEFAULT 0;

-- 差距 #1:事务性 Outbox(与 runtime/internal/store/schema.sql 保持一致)
CREATE TABLE IF NOT EXISTS outbox (
    id             BIGSERIAL PRIMARY KEY,
    aggregate_type TEXT        NOT NULL,
    aggregate_id   TEXT        NOT NULL,
    event_type     TEXT        NOT NULL,
    event_key      TEXT        NOT NULL UNIQUE,
    payload        JSONB       NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at   TIMESTAMPTZ,
    attempts       INTEGER     NOT NULL DEFAULT 0,
    last_error     TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS outbox_unpublished_idx ON outbox(id) WHERE published_at IS NULL;

COMMIT;
