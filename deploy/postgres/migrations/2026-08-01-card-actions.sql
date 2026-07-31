BEGIN;

-- 前置检查:本轮的 card_actions.workflow_id 外键依赖 workflow_runs。
-- 上一轮生产迁移未完成时必须明确失败,而不是静默建出一张缺 FK 的表。
-- 用 to_regclass() 而非 pg_tables:后者按 tablename 全库扫描,不看 search_path,
-- 会在同一集群的其他 schema 里"看见"同名表而误判存在;to_regclass() 与随后
-- CREATE TABLE ... REFERENCES workflow_runs(...) 走同一套 search_path 名字解析,
-- 结果才和实际会不会建出缺 FK 的表保持一致。
DO $$
BEGIN
    IF to_regclass('workflow_runs') IS NULL THEN
        RAISE EXCEPTION 'card-actions migration requires table workflow_runs; run the workflow_runs migration first';
    END IF;
END $$;

-- 五张表的 DDL 逐字取自 spec §3.1-§3.5(含全部 CHECK 与部分索引)。
-- 创建顺序遵循 FK 依赖:card_actions 依赖 workflow_runs(已由前置检查确认存在);
-- audit_log 依赖 card_actions 与 card_action_inbox,必须放在最后。

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

COMMIT;
