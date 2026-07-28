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
    -- 显式 retry 计数(差距 #11):workflow ID 加 -r{N} 后缀,N 由此列原子递增;
    -- 普通 webhook/kick 重放绝不递增(RejectDuplicate,失败不自动重启)。
    workflow_attempt INTEGER    NOT NULL DEFAULT 0,
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

-- 独占租约:AcquireDevice 用 `SELECT ... FOR UPDATE OF d SKIP LOCKED` 行锁保证并发下
-- 只有一个调用者拿到同一设备(§3 规则 3 独占,§11)。
-- lease_expires_at 由 Client 心跳经 RenewLease 续期(§10 租约 120s);过期 = 持有者失联
-- (workflow 被 Terminate/进程死亡等绕过 ReleaseDevice),由 AcquireDevice 懒回收。
-- 所有权凭据(docs/device-test-sequence.md §10/差距 #15):lease_id(每次授予唯一,
-- 取 task_id)+ lease_generation(每设备单调递增,获取/懒回收时 +1)+ released_at
-- (ReleaseDevice 置位,行保留作审计);心跳续租必须全部匹配且 released_at IS NULL,
-- 失配即 LEASE_NOT_OWNED,旧持有者不得再续已易主的租约。
CREATE TABLE IF NOT EXISTS device_leases (
    device_id        TEXT PRIMARY KEY REFERENCES devices(device_id),
    task_id          TEXT        NOT NULL,
    lease_id         TEXT        NOT NULL DEFAULT '',
    lease_generation INTEGER     NOT NULL DEFAULT 0,
    lease_expires_at TIMESTAMPTZ NOT NULL,
    released_at      TIMESTAMPTZ
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
-- evidence_snapshot_id 引用 evidence_snapshots(差距 #6 决策可回放):
-- 仅 hermes 裁决携带(基于 evidence);rule 裁决基于 result,为空。
CREATE TABLE IF NOT EXISTS decisions (
    decision_id    BIGSERIAL PRIMARY KEY,
    task_id        TEXT        NOT NULL REFERENCES tasks(task_id),
    actor          TEXT        NOT NULL,            -- hermes|rule|human
    input_digest   TEXT        NOT NULL DEFAULT '', -- 输入摘要(evidence sha256;rule 可为空)
    model          TEXT        NOT NULL DEFAULT '',
    prompt_version TEXT        NOT NULL DEFAULT '',
    output         JSONB       NOT NULL,
    evidence_snapshot_id TEXT  NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS decisions_task_id_idx ON decisions(task_id);

-- evidence.json 快照登记(差距 #6):原始日志可按生命周期清理,
-- 快照(≤96KB)随 Decision 保留,hermes 裁决可回放"当时看到了什么"。
-- evidence_id = task_id(含 attempt,全链路唯一,差距 #14);
-- 同一任务重复提取(activity 重试)按 evidence_id 幂等。
CREATE TABLE IF NOT EXISTS evidence_snapshots (
    evidence_id       TEXT PRIMARY KEY,
    task_id           TEXT        NOT NULL,
    attempt           INTEGER     NOT NULL DEFAULT 0,
    object_key        TEXT        NOT NULL,
    sha256            TEXT        NOT NULL,
    extractor_version TEXT        NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS evidence_snapshots_task_id_idx ON evidence_snapshots(task_id);

-- 事务性 Outbox(docs/device-test-sequence.md 设计原则 3,表结构见文末):
-- 关键事件(Result/Cancel/Human Decision)与业务数据单事务写入,由独立 Outbox Relay
-- 至少一次投递 Temporal Signal,接收端幂等兜底。published_at IS NULL = 未投递;
-- 投递失败 attempts+1 并记 last_error,留待下轮重试,可监控。
CREATE TABLE IF NOT EXISTS outbox (
    id             BIGSERIAL PRIMARY KEY,
    aggregate_type TEXT        NOT NULL,            -- task / device / ...
    aggregate_id   TEXT        NOT NULL,
    event_type     TEXT        NOT NULL,            -- task-result / cancel / ...
    event_key      TEXT        NOT NULL UNIQUE,     -- 幂等键,如 {task_id}:result
    payload        JSONB       NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at   TIMESTAMPTZ,                     -- NULL = 未投递
    attempts       INTEGER     NOT NULL DEFAULT 0,
    last_error     TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS outbox_unpublished_idx ON outbox(id) WHERE published_at IS NULL;

-- 飞书指令层自然语言翻译审计(设计文档 §4.3)。翻译发生在任何 task 存在之前,
-- 无 task_id 可填,故不能复用 decisions 表(其 task_id 是 NOT NULL 外键)。
-- 追加式:确认流程不更新已有行,pending_confirm 与 confirmed 各占一行。
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
