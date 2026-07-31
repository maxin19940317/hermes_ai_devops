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
    CONSTRAINT artifacts_project_key
        UNIQUE (project, commit_sha, pipeline_id, variant)
);

CREATE TABLE IF NOT EXISTS workflow_runs (
    workflow_id        TEXT PRIMARY KEY,
    project            TEXT        NOT NULL,
    commit_sha         TEXT        NOT NULL,
    pipeline_id        INTEGER     NOT NULL CHECK (pipeline_id > 0),
    version            TEXT        NOT NULL,
    rule_version       TEXT        NOT NULL,
    scope              TEXT        NOT NULL DEFAULT '',
    attempt            INTEGER     NOT NULL CHECK (attempt >= 0),
    variants           TEXT[]      NOT NULL,
    source_workflow_id TEXT        REFERENCES workflow_runs(workflow_id) ON DELETE RESTRICT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (source_workflow_id IS NULL OR source_workflow_id <> workflow_id)
);
CREATE INDEX IF NOT EXISTS workflow_runs_recent_idx
    ON workflow_runs(created_at DESC, workflow_id DESC);

-- CONTRACT-ISSUE: §11 clients 表还列了 status 列,承载"离线判定 3 次丢失"(§10)。
-- 该判定逻辑(基于 last_heartbeat 的超时/丢失计数)本轮(worker 进程装配)未实现,
-- 留给后续步骤;先不加从未被任何代码写入的空列。
CREATE TABLE IF NOT EXISTS clients (
    client_id      TEXT PRIMARY KEY,
    host           TEXT        NOT NULL DEFAULT '',
    version        TEXT        NOT NULL DEFAULT '',
    base_url       TEXT        NOT NULL DEFAULT '',   -- 派单地址(§8.1),来源于心跳注册
    last_heartbeat TIMESTAMPTZ NOT NULL DEFAULT now(),
    fail_streak    INTEGER     NOT NULL DEFAULT 0   -- client 级连续失败(差距 #10)
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
CREATE INDEX IF NOT EXISTS tasks_run_variant_latest_idx
    ON tasks(workflow_id, test_id, attempt DESC, created_at DESC);

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

-- outbox 积压视图(第四批:backlog/失败监控)。Relay 会定期把同样的数字打进日志,
-- 这个视图是人工排查入口:`SELECT * FROM outbox_backlog;`
-- stuck 的判定阈值在 Relay 侧可配(RELAY_STUCK_ATTEMPTS),视图固定用 3——
-- 视图是给人看的粗筛,精确阈值以 Relay 日志为准。
CREATE OR REPLACE VIEW outbox_backlog AS
SELECT count(*)                                            AS pending,
       count(*) FILTER (WHERE attempts >= 3)               AS stuck,
       coalesce(EXTRACT(EPOCH FROM (now() - min(created_at))), 0)::bigint
                                                           AS oldest_age_sec,
       max(attempts)                                       AS max_attempts
FROM outbox
WHERE published_at IS NULL;

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

-- 飞书卡片按钮动作(设计文档 2026-07-31-feishu-card-actions-design §3)。
-- 五张表,同步落地进 deploy/postgres/migrations/2026-08-01-card-actions.sql。
-- fresh 库(本文件)不需要该迁移文件开头的 workflow_runs 前置检查——
-- workflow_runs 就在本文件上方,同一事务内先建好。

-- §3.1 card_action_inbox —— 同步段的持久交接
CREATE TABLE IF NOT EXISTS card_action_inbox (
    event_id         TEXT PRIMARY KEY,          -- 飞书事件 ID:跨进程、跨重启去重
    disposition      TEXT NOT NULL CHECK (disposition IN ('accepted','rejected')),
    ack_toast        TEXT NOT NULL,             -- 同步应答原文,重投时原样重放
    action           TEXT NOT NULL DEFAULT '',
    workflow_id      TEXT NOT NULL DEFAULT '',
    actor_open_id    TEXT NOT NULL DEFAULT '',
    open_message_id  TEXT NOT NULL DEFAULT '',
    -- 同步段算出的摘要;恢复路径不得重新猜(原始 payload 已不在手上)
    payload_digest   TEXT NOT NULL DEFAULT '',
    state            TEXT NOT NULL DEFAULT 'received'
        CHECK (state IN ('received','processed')),
    owner            TEXT        NOT NULL DEFAULT '',
    lease_expires_at TIMESTAMPTZ,
    attempts         INTEGER     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error       TEXT        NOT NULL DEFAULT '',
    received_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at     TIMESTAMPTZ,
    CONSTRAINT inbox_rejected_is_terminal CHECK (
        disposition <> 'rejected' OR state = 'processed'),
    CONSTRAINT inbox_processed_pairs_timestamp CHECK (
        (state = 'processed') = (processed_at IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS card_action_inbox_sweep_idx
    ON card_action_inbox(lease_expires_at) WHERE state = 'received';

-- §3.2 card_actions —— 动作,按 workflow 唯一
CREATE TABLE IF NOT EXISTS card_actions (
    workflow_id        TEXT PRIMARY KEY REFERENCES workflow_runs(workflow_id),
    action             TEXT        NOT NULL CHECK (action IN ('retry','ignore')),
    actor_open_id      TEXT        NOT NULL,
    state              TEXT        NOT NULL CHECK (state IN ('pending','succeeded','failed')),
    owner              TEXT        NOT NULL DEFAULT '',
    lease_expires_at   TIMESTAMPTZ,
    target_workflow_id TEXT        NOT NULL DEFAULT '',
    attempt            INTEGER     NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    -- 恢复用的完整启动输入;StartDeviceTest 收的是 DeviceTestInput,不是 ID
    target_input       JSONB,
    last_error         TEXT        NOT NULL DEFAULT '',
    revision           INTEGER     NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT card_actions_retry_pinned CHECK (
        action <> 'retry'  OR (attempt > 0 AND target_workflow_id <> '' AND target_input IS NOT NULL)),
    CONSTRAINT card_actions_ignore_unpinned CHECK (
        action <> 'ignore' OR (attempt = 0 AND target_workflow_id = '' AND target_input IS NULL)),
    -- ignore 没有外部副作用,不存在执行中或执行失败
    CONSTRAINT card_actions_ignore_terminal CHECK (
        action <> 'ignore' OR state = 'succeeded')
);

CREATE INDEX IF NOT EXISTS card_actions_sweep_idx
    ON card_actions(lease_expires_at) WHERE state = 'pending';

-- §3.3 card_action_messages —— 卡片渲染,按消息唯一
CREATE TABLE IF NOT EXISTS card_action_messages (
    -- 不加指向 card_actions 的外键:legacy / 仍在运行 / 无失败变体这些拒绝路径
    -- 不会产生 action 行,但同样需要把结论渲染到卡片上
    workflow_id       TEXT        NOT NULL,
    open_message_id   TEXT        NOT NULL,
    render_kind       TEXT        NOT NULL CHECK (render_kind IN ('action','rejection')),
    rejection_reason  TEXT        NOT NULL DEFAULT '',
    desired_revision  INTEGER     NOT NULL DEFAULT 1 CHECK (desired_revision > 0),
    rendered_revision INTEGER     NOT NULL DEFAULT 0 CHECK (rendered_revision >= 0),
    update_state      TEXT        NOT NULL DEFAULT 'pending'
        CHECK (update_state IN ('pending','succeeded','abandoned')),
    -- 只对 render_kind='rejection' 有意义(§7.5):拒绝原因决定该卡片还该不该留按钮。
    -- render_kind='action' 的按钮集合由动作状态决定,不看这一列。
    buttons_mode      TEXT        NOT NULL DEFAULT 'none'
        CHECK (buttons_mode IN ('none','both')),
    owner             TEXT        NOT NULL DEFAULT '',
    lease_expires_at  TIMESTAMPTZ,
    reconcile_after   TIMESTAMPTZ,               -- 模糊超时后的延迟复核(§8.3)
    attempts          INTEGER     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error        TEXT        NOT NULL DEFAULT '',
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workflow_id, open_message_id),
    CONSTRAINT messages_rejection_has_reason CHECK (
        render_kind <> 'rejection' OR rejection_reason <> '')
);

CREATE INDEX IF NOT EXISTS card_action_messages_sweep_idx
    ON card_action_messages(lease_expires_at) WHERE update_state = 'pending';

-- §3.4 card_action_snapshots —— PATCH 的原卡来源
CREATE TABLE IF NOT EXISTS card_action_snapshots (
    workflow_id  TEXT PRIMARY KEY,   -- 同一 workflow 的多张卡片内容相同,按 workflow 存一份
    card_json    JSONB       NOT NULL,   -- 规范化的原展示卡(不含 action 模块)
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- §3.5 audit_log
-- 对 spec 的加强:CHECK (actor <> '' AND action <> '')。spec 只写了 NOT NULL,
-- 但空串同样是无归属的审计行;此 CHECK 同时为 Task 4 的"审计失败整笔回滚"用例
-- 提供不需要生产代码开测试后门的故障注入手段。
CREATE TABLE IF NOT EXISTS audit_log (
    audit_id                BIGSERIAL   PRIMARY KEY,
    actor                   TEXT        NOT NULL,
    action                  TEXT        NOT NULL,
    target                  TEXT        NOT NULL,
    payload_digest          TEXT        NOT NULL DEFAULT '',
    -- 一个 accepted action 恰好一条审计
    card_action_workflow_id TEXT UNIQUE REFERENCES card_actions(workflow_id),
    -- 一个 inbox 事件恰好一条审计:消费重跑不会写出第二行
    inbox_event_id          TEXT UNIQUE REFERENCES card_action_inbox(event_id),
    ts                      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT audit_log_actor_action_nonempty CHECK (actor <> '' AND action <> '')
);
