-- Runtime 数据模型(CLAUDE.md §11)。幂等:重复执行本文件无副作用。
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

-- CONTRACT-ISSUE: §11 clients 表还列了 status 列,承载"离线判定 3 次丢失"(§10)。
-- 该判定逻辑(基于 last_heartbeat 的超时/丢失计数)本轮(worker 进程装配)未实现,
-- 留给后续步骤;先不加从未被任何代码写入的空列。
CREATE TABLE IF NOT EXISTS clients (
    client_id      TEXT PRIMARY KEY,
    host           TEXT        NOT NULL DEFAULT '',
    version        TEXT        NOT NULL DEFAULT '',
    base_url       TEXT        NOT NULL DEFAULT '',   -- 派单地址(§8.1),来源于心跳注册
    last_heartbeat TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 设备状态(§11):IDLE|BUSY|OFFLINE|QUARANTINED。心跳(UpsertClientDevices)只刷新属性,
-- 绝不触碰 status/fail_streak——隔离/占用状态只能由 AcquireDevice/ReleaseDevice 改变。
CREATE TABLE IF NOT EXISTS devices (
    device_id    TEXT PRIMARY KEY,
    serial       TEXT        NOT NULL UNIQUE,
    client_id    TEXT        NOT NULL REFERENCES clients(client_id),
    soc          TEXT        NOT NULL DEFAULT '',
    abi          TEXT        NOT NULL DEFAULT '',
    capabilities TEXT[]      NOT NULL DEFAULT '{}',
    status       TEXT        NOT NULL DEFAULT 'IDLE',
    fail_streak  INTEGER     NOT NULL DEFAULT 0
);

-- 独占租约:AcquireDevice 用 `SELECT ... FOR UPDATE SKIP LOCKED` 行锁保证并发下
-- 只有一个调用者拿到同一设备(§3 规则 3 独占,§11)。
CREATE TABLE IF NOT EXISTS device_leases (
    device_id        TEXT PRIMARY KEY REFERENCES devices(device_id),
    task_id          TEXT        NOT NULL,
    lease_expires_at TIMESTAMPTZ NOT NULL
);

-- status(生命周期)与 verdict(终态判定)正交,不合并为一个枚举(§9,§14 红线)。
CREATE TABLE IF NOT EXISTS tasks (
    task_id         TEXT PRIMARY KEY,
    workflow_id     TEXT        NOT NULL,
    test_id         TEXT        NOT NULL,
    attempt         INTEGER     NOT NULL,
    idempotency_key TEXT        NOT NULL UNIQUE,
    client_id       TEXT        NOT NULL DEFAULT '',
    device_id       TEXT        NOT NULL DEFAULT '',
    status          TEXT        NOT NULL,
    verdict         TEXT        NOT NULL DEFAULT '',
    error_category  TEXT        NOT NULL DEFAULT '',
    reason          TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at        TIMESTAMPTZ
);

-- 回调可能重发;按 (task_id, seq) 去重(§8.2)。
CREATE TABLE IF NOT EXISTS task_events (
    task_id     TEXT        NOT NULL REFERENCES tasks(task_id),
    seq         INTEGER     NOT NULL,
    from_status TEXT        NOT NULL DEFAULT '',
    to_status   TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, seq)
);

-- CONTRACT-ISSUE: §11 results 表列了展开列(exit_code/duration_sec/counts/metrics/
-- attachments JSONB)。这里收敛成单个 result_json,因为 Phase 1.6 尚无消费方需要
-- SQL 侧结构化查询这些字段;§9 baseline 比较(metrics 表)属于后续 Phase,届时若
-- 需要按 metric_name 聚合查询,再拆出独立列或按 §11 建 metrics 表,不在此处放宽。
CREATE TABLE IF NOT EXISTS results (
    task_id     TEXT PRIMARY KEY REFERENCES tasks(task_id),
    result_json JSONB       NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 一切裁决(规则引擎/LLM/人工)都落 decisions 表,可回放(§11)。
CREATE TABLE IF NOT EXISTS decisions (
    decision_id    BIGSERIAL PRIMARY KEY,
    task_id        TEXT        NOT NULL REFERENCES tasks(task_id),
    actor          TEXT        NOT NULL,            -- hermes|rule|human
    input_digest   TEXT        NOT NULL DEFAULT '', -- 输入摘要(evidence sha256;rule 可为空)
    model          TEXT        NOT NULL DEFAULT '',
    prompt_version TEXT        NOT NULL DEFAULT '',
    output         JSONB       NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS decisions_task_id_idx ON decisions(task_id);
