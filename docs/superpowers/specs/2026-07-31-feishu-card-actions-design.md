# 飞书终态卡片按钮设计（重试 / 忽略）

**日期：** 2026-07-31
**状态：** 待评审（v2；v1 评审发现两处架构阻断，已重写 §3 §5 §6 §7 §8）
**范围：** 三轮交互能力的第二轮。终态通知卡片上加两个**卡片级**按钮——「重试失败变体」「忽略」。
不含 NL 确认/取消按钮（第三轮）、隔离按钮、证据链接、卡片模板化。

前置轮：
- `2026-07-30-feishu-notification-card-design.md`（展示卡片，§10 是本轮的输入清单）
- `2026-07-30-workflow-runs-design.md`（权威运行记录，解除了 §10(3)(4)）

> **v1 → v2 的两处架构阻断**（评审发现，均已实证）：
> 1. PostgreSQL 的 CHECK **不可延迟到 COMMIT**，也不接受 `DEFERRABLE`
>    （实测：先插默认值再 UPDATE → `SQLSTATE 23514`；`CHECK ... DEFERRABLE` → `SQLSTATE 0A000`）。
>    v1 的"先 INSERT 再 UPDATE 钉死"必然失败。§5 改为锁定后**一次性插入完整行**。
> 2. 同步 ACK 与异步 claim 之间没有持久交接，进程在两者之间退出会让点击**永久消失**
>    （飞书已收到成功应答，不再重投）。§6 新增 `card_action_inbox`。

---

## 1. 门禁裁决与实测结论

开工前跑了一次性 spike（`runtime/cmd/spike-cardaction`），对真实飞书应用验证。**裁决：GO。**
四条实测结论是本设计的事实基础，不再重新论证：

1. **WS 通道成立。** `card.action.trigger` 经长连接送达，事件订阅保持现状即可，**不需要公网回调 URL**。
   实现路径唯一：在现有 `dispatcher.EventDispatcher` 上注册 `OnP2CardActionTrigger`。
   （`ws.WithCardHandler` 在 SDK v3.9.9 是注释掉的，`MessageTypeCard` 帧被
   `ws/client.go:626` 直接丢弃——那条路走不通，不要再试。）
2. **反馈必须分两层。** 即时层 = toast，**best-effort**（实测存在错误码 200671）；
   权威层 = 卡片延迟更新，**不受 3 秒响应预算限制，这层必须做**。
3. **重复投递是常态。** 实测一次 ignore 点击收到 **3 次**回调。幂等按动作幂等键消费，
   **不以到达次数为准**。
4. **载荷最小化验证通过。** `value` 只放 `action` + `source_workflow_id`。

---

## 2. 语义

### 2.1 重试

**完整复用 `rerun` 的业务语义**，但**不复用其调用路径**——这两件事必须分开承诺。

失败集合与 `rerun` 逐字一致：源 workflow 已关闭，从 Temporal `DeviceTestOutput` 取
`verdict ∉ {PASSED, SKIPPED}` 的 `TaskSummary`。**不从 tasks 缺行推断 SKIPPED**——
`AcquireDevice` 耗尽、`CreateTask` 活动自身失败都会产出没有 task 行的失败 summary。

重试产生**新的 workflow，并发出它自己的终态卡片**。若仍失败，从新卡片继续重试。
因此原卡片只承载一次 chosen action。

### 2.2 忽略

**只表示人工确认**：某人看过这次失败并决定不处理。不修改 `tasks.verdict`；不触发任何策略；
**当下没有任何消费方读取它**。这三条必须写在用户可见文档里，否则第一个用的人会以为按钮坏了。

权威记录是 `card_actions` + `audit_log`。**不写 `decisions`**——理由见 §3.5。

### 2.3 按钮出现条件

见 §7.1：readiness 是**五项合取**，且只能承诺"发送瞬间 ready"。

---

## 3. 数据模型

四张表：`card_action_inbox`（传输层）、`card_actions`（动作）、`card_action_messages`（卡片渲染）、
`audit_log`（审计）。分开的理由各自写在下面。

### 3.1 card_action_inbox —— 同步段的持久交接

同步应答一旦返回成功，飞书**不再重投**。因此在返回之前必须先落盘，否则进程在
"已应答、未 claim"之间退出，动作永久消失。

```sql
CREATE TABLE IF NOT EXISTS card_action_inbox (
    event_id         TEXT PRIMARY KEY,          -- 飞书事件 ID:跨进程、跨重启的去重依据
    disposition      TEXT NOT NULL CHECK (disposition IN ('accepted','rejected')),
    ack_toast        TEXT NOT NULL,             -- 同步应答原文,重投时原样重放
    action           TEXT NOT NULL DEFAULT '',
    workflow_id      TEXT NOT NULL DEFAULT '',
    actor_open_id    TEXT NOT NULL DEFAULT '',
    open_message_id  TEXT NOT NULL DEFAULT '',
    state            TEXT NOT NULL DEFAULT 'received'
        CHECK (state IN ('received','processed')),
    owner            TEXT        NOT NULL DEFAULT '',
    lease_expires_at TIMESTAMPTZ,
    attempts         INTEGER     NOT NULL DEFAULT 0,
    last_error       TEXT        NOT NULL DEFAULT '',
    received_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at     TIMESTAMPTZ,
    -- rejected 是同步段的终局,不需要异步消费
    CONSTRAINT inbox_rejected_is_terminal CHECK (
        disposition <> 'rejected' OR state = 'processed')
);

CREATE INDEX IF NOT EXISTS card_action_inbox_sweep_idx
    ON card_action_inbox(lease_expires_at) WHERE state = 'received';
```

**它没有指向 `workflow_runs` 或 `card_actions` 的外键**——同步段还没做权威校验，
此时唯一确定的事实是"一个形态合法、身份获授权的点击到达过"。

`event_id` 主键同时承担三件事：跨进程去重（进程内缓存做不到）、跨重启去重（`dedupCache`
重启即失效）、以及**同步应答重放**——重投读出既有行返回同一条 `ack_toast`，
既满足实测结论 3 的"三次相同 toast"，又保证审计不重复。

### 3.2 card_actions —— 动作，按 workflow 唯一

```sql
CREATE TABLE IF NOT EXISTS card_actions (
    workflow_id        TEXT PRIMARY KEY REFERENCES workflow_runs(workflow_id),
    action             TEXT        NOT NULL CHECK (action IN ('retry','ignore')),
    actor_open_id      TEXT        NOT NULL,
    state              TEXT        NOT NULL CHECK (state IN ('pending','succeeded','failed')),
    owner              TEXT        NOT NULL DEFAULT '',
    lease_expires_at   TIMESTAMPTZ,
    target_workflow_id TEXT        NOT NULL DEFAULT '',
    attempt            INTEGER     NOT NULL DEFAULT 0,
    last_error         TEXT        NOT NULL DEFAULT '',
    -- 每次对用户可见的状态变化 +1;卡片渲染以它为收敛目标(§3.3)
    revision           INTEGER     NOT NULL DEFAULT 1 CHECK (revision > 0),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT card_actions_retry_pinned CHECK (
        action <> 'retry'  OR (attempt >  0 AND target_workflow_id <> '')),
    CONSTRAINT card_actions_ignore_unpinned CHECK (
        action <> 'ignore' OR (attempt =  0 AND target_workflow_id =  ''))
);

CREATE INDEX IF NOT EXISTS card_actions_sweep_idx
    ON card_actions(lease_expires_at) WHERE state = 'pending';
```

**`workflow_id` 做主键**是本设计的支点，一次性消解三个问题：

- **互斥**（§10(5)）：一次运行最多一个 claim，`action` 只是列。不需要额外的互斥规则。
- **幂等**：与 inbox 的 `event_id` 互补——inbox 挡同一次点击的重投，主键挡不同点击的竞争。
- **legacy 隔离**：`REFERENCES workflow_runs(workflow_id)` 让没有权威行的历史运行不可能产生 claim。

**两个 CHECK 是 §10(2) 的机械化，且因为 PostgreSQL 的 CHECK 不可延迟（实证见文首），
它们把"接受动作必须一次性写入完整行"变成了数据库层面不可违反的约束**——任何
"先插 pending 再补 attempt"的实现会在第一条 INSERT 就拿到 `23514`。

### 3.3 card_action_messages —— 卡片渲染，按消息唯一

`NotifyCard` 是**可重试活动**：飞书已收到、活动 ACK 丢失时会重发，同一 workflow 可能存在
**多张卡片**。因此"一个 workflow 一个 `open_message_id`"不成立，必须按消息实例收敛。

```sql
CREATE TABLE IF NOT EXISTS card_action_messages (
    workflow_id       TEXT        NOT NULL REFERENCES card_actions(workflow_id),
    open_message_id   TEXT        NOT NULL,
    update_state      TEXT        NOT NULL DEFAULT 'pending'
        CHECK (update_state IN ('pending','succeeded','abandoned')),
    -- 已确认渲染到飞书的 revision;收敛目标是 card_actions.revision
    rendered_revision INTEGER     NOT NULL DEFAULT 0,
    owner             TEXT        NOT NULL DEFAULT '',
    lease_expires_at  TIMESTAMPTZ,
    attempts          INTEGER     NOT NULL DEFAULT 0,
    last_error        TEXT        NOT NULL DEFAULT '',
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workflow_id, open_message_id)
);

CREATE INDEX IF NOT EXISTS card_action_messages_sweep_idx
    ON card_action_messages(lease_expires_at) WHERE update_state = 'pending';
```

每个通过校验的回调**先登记消息实例**（`ON CONFLICT DO NOTHING`）：动作按 workflow 唯一，
卡片按 message 唯一收敛。用户在哪张卡片上点，哪张卡片一定会被更新；其余同 workflow 的卡片
也会被同一个 sweep 收敛到同一状态。

### 3.4 audit_log

CLAUDE.md §11 声明、`schema.sql` 从未建过。本轮建表并成为第一个写入方。

```sql
CREATE TABLE IF NOT EXISTS audit_log (
    audit_id                BIGSERIAL   PRIMARY KEY,
    actor                   TEXT        NOT NULL,
    action                  TEXT        NOT NULL,
    target                  TEXT        NOT NULL,
    payload_digest          TEXT        NOT NULL DEFAULT '',
    -- 一个 accepted action 恰好一条审计;被拒点击写 NULL(UNIQUE 放行多个 NULL)
    card_action_workflow_id TEXT UNIQUE REFERENCES card_actions(workflow_id),
    ts                      TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### 3.5 decisions 不动

**明确否决**"把 `decisions.task_id` 改成可空"。它是全局放宽：会允许 `rule` / `hermes` 的
decision 也写成无归属的 NULL task。而且 `decisions` 没有 `workflow_id` 列，run 级归属只能
藏进 JSON，FK 与查询接口都无法保证。

> 将来若确有消费方需要把 run 级人工决策放进 `decisions`，必须新增
> `workflow_id REFERENCES workflow_runs(workflow_id)` 并加
> `CHECK ((task_id IS NULL) <> (workflow_id IS NULL))`，**不能只放宽 NOT NULL**。

### 3.6 文档同步修正

- **CLAUDE.md:37**（§3 规则 7）——终态卡片确认是 **run 级**动作，记于 `card_actions` +
  `audit_log`，不属于 task-level decision。
- **CLAUDE.md §11 数据模型**——补四张表。
- **`docs/device-test-sequence.md`**——终态卡片确认**不投 workflow signal、不走 outbox**。

---

## 4. 共享 RerunResolver

文本 `rerun` 与按钮重试必须共用一份业务语义，否则两处实现必然漂移。

新增包 `runtime/internal/rerun`（不放进 `feishucmd`，否则 `cardaction` 要反向依赖指令包）：

```go
// Resolve 是只读的:不分配 attempt、不写库、不启动 workflow。
// variant 为空 = "未指定 variant" 模式(按钮唯一使用的模式);
// 非空 = 显式单变体模式(文本 rerun 专用)。
func (r *Resolver) Resolve(ctx context.Context, workflowID, variant string) (*Resolution, error)

type Resolution struct {
    Run      store.WorkflowRun
    Targets  []string        // canonical 顺序
    Packages []wf.PackageRef // 与 Targets 一一对应
    Scope    string          // 显式模式 = variant;未指定模式 = Run.Scope
}
```

**硬约束：必须支持 optional variant**，使文本 `rerun` 的显式单变体语义**和回复文案逐字不变**。
`executor_test.go` 里既有的 rerun 用例一行不改即通过，是本轮验收项。

`Resolve` 含 Temporal 调用，**必须在数据库事务之外执行**。

拒绝原因是封闭枚举：`NotAuthoritative` / `StillRunning` / `ResultUnreadable` /
`NoFailedVariants` / `VariantNotMember` / `ArtifactMissing`。

---

## 5. 接受动作：单事务、一次性插入

### 5.1 边界

CHECK 不可延迟，所以顺序是**先锁、再算、最后一次性插入完整行**：

```text
[事务外·只读] Resolve(workflow_id, "") → 目标集 + packages,或封闭枚举的拒绝原因

BEGIN
  SELECT 1 FROM workflow_runs WHERE workflow_id = $1 FOR UPDATE
    ├ 无行 → rejected.legacy,事务结束
    └ 取得行锁 = 该 workflow 的所有 claim 竞争在此串行化
       (显式先查并加锁,不把 FK 违反当控制流;FK 只是最后防线)

  SELECT * FROM card_actions WHERE workflow_id = $1        -- 已在上面的锁保护下
    │
    ├ 无行(首次) →
    │     retry : 锁 artifacts 推进水位得 N → Go 侧算 target_workflow_id
    │     INSERT card_actions(...完整行,含 attempt/target/state/revision=1)  ← 唯一一条 INSERT
    │     INSERT card_action_messages(workflow_id, open_message_id, ...) ON CONFLICT DO NOTHING
    │     INSERT audit_log(..., card_action_workflow_id = workflow_id)
    │
    ├ 同 action 且 state='failed' → §5.3 失败后重试
    │
    └ 其余 → rejected.conflict
COMMIT
```

`SELECT ... FOR UPDATE` 加在 `workflow_runs` 而非 `card_actions` 上，因为首次点击时
`card_actions` 行**还不存在**，不存在的行锁不住。父表行锁是唯一能串行化"检查是否已有 claim →
决定是否插入"的位置。`INSERT card_actions` 本身会对同一父行取 `FOR KEY SHARE`，
弱于已持有的 `FOR UPDATE`，不存在锁升级死锁。

水位分配要能组合进本事务，因此 `NextWorkflowAttemptAll` 需抽出 tx 作用域的内部函数
（当前实现自己 `BeginTx`，见 `postgres_fleet.go:95`），独立方法与本事务共用同一份实现。

**验收要求真实 PG 并发测试**：N 个并发点击同一 workflow，断言恰好一次 accepted、
artifacts 水位**恰好推进一次**、其余全部 `rejected.conflict`。

### 5.2 三条 fencing 规则

全部落在 store API 上，不靠调用方自律：

1. **finalize 必须带 fencing**：`UPDATE ... WHERE workflow_id=? AND state='pending' AND owner=?`。
   租约过期被接管后，原 owner 迟到的 finalize 影响 0 行。
2. **恢复与 `failed → pending` 一律复用原 `attempt` / `target_workflow_id`，绝不推进水位。**
   水位推进只发生在首次接受的那个事务里，一次 claim 一次；此后该行的 attempt 只读。
3. **租约只管活性，不管正确性。** 正确性来自"钉死 + CAS"：重复执行只会用同一个 workflow ID
   撞 Temporal `RejectDuplicate`。租约取 **120 秒**。

### 5.3 已有行的三种归宿

| 既有行 | 本次点击 | 归宿 |
|---|---|---|
| 任意 action，`pending` / `succeeded` | 任意 | **被占**：写 `rejected.conflict` 审计 |
| `action` 相同，`state='failed'` | 相同 action | **失败后重试**：CAS `failed → pending`，取新 owner 与租约，**复用原 `attempt` / `target`**，`revision + 1`，重排该 workflow 的全部消息行；不写新的 accepted 审计 |
| `action` 不同，`state='failed'` | 另一 action | **被占**：写 `rejected.conflict` 审计 |

**动作在首次接受时即固定，不可改换。** retry 失败后只能重试 retry——否则 `attempt` /
`target_workflow_id` 就得清空，与"钉死后只读"和两个 CHECK 直接冲突。这也是 §7.4 里
失败态**只保留「重新重试」一个按钮**的原因：摆一个必然 `conflict` 的 ignore 按钮是误导。

失败后重试不写第二行 accepted 审计（`card_action_workflow_id` 有 UNIQUE 约束）。

### 5.4 revision 与卡片重排

**每一次对用户可见的状态变化，都在同一事务内 `revision + 1` 并把该 workflow 的全部
`card_action_messages` 重置为 `update_state='pending'`、清空 owner/lease。** 触发点恰好四个：

| 变化 | revision |
|---|---|
| 首次接受（`pending` 或 `succeeded`） | 置 1 |
| retry finalize → `succeeded` | +1 |
| retry finalize → `failed` | +1 |
| `failed → pending`（失败后重试） | +1 |

v1 的缺陷是 retry finalize 后没有重新排队，卡片会永远停在「正在启动…」。上表把它闭合。

---

## 6. 回调处理

### 6.1 同步段与异步段

飞书响应预算约 3 秒，超时即重投（实测 3 次）。`Resolve` 含两次 Temporal 调用，
**不能放进同步段**。切分点就是 inbox：

**同步段**（毫秒级，一次 INSERT，返回 toast）：

1. **来源校验**（§6.2，全部无 I/O）与载荷校验、身份、白名单、readiness。
2. `INSERT card_action_inbox (event_id, disposition, ack_toast, ...) ON CONFLICT (event_id) DO NOTHING`
   - **影响 0 行（重投）** → 读出既有行，**原样重放 `ack_toast`**，不写审计、不重复处理；
   - **插入成功且 `disposition='rejected'`** → 同事务写拒绝审计，返回精确 toast；
   - **插入成功且 `disposition='accepted'`** → 返回 toast「已收到，正在处理」，交异步段。
3. 拿不到 `event_id` 的畸形事件：拒绝 + 只记日志，**不写审计**——无法去重的审计在重投下无界增长。

**异步段**（无时限，结果经卡片 Patch 呈现）：从 inbox 消费 → `Resolve` → 接受事务 → 执行 →
标记 inbox `processed`。用 listener 的生命周期 ctx，**不用回调 ctx**（后者在应答返回时即取消）。

> **inbox 同时是崩溃兜底**：进程在 ACK 之后、claim 之前退出时，inbox 行留在 `received`，
> 由 §8.1 的 sweep 接管。这正是 v1 缺失的持久交接。
>
> 两层反馈的分工由此固定：toast 是**临时回执**，卡片才是**权威结论**。
> 任何需要 I/O 才能判定的结果（legacy、无失败变体、已被占、启动失败）都由卡片承载，
> **不由 toast 承载**——异步段跑完时同步应答早已返回。

### 6.2 来源与载荷校验（fail-closed）

**全部在任何持久副作用之前完成**，任一不满足即不写 inbox 的 accepted 行、不 claim、
不分配 attempt、不启动 workflow：

| 检查 | 拒绝原因 |
|---|---|
| `Header.AppID` == 配置的 AppID | `payload` |
| header tenant 与 `operator.tenant_key` 均非空且相等 | `payload` |
| `Action.Tag == "button"` | `payload` |
| `Context.OpenMessageID` 非空 | `payload` |
| `Host` 是消息卡片（`im_message`） | `payload` |
| `operator.open_id` 非空 | `payload` |
| payload 大小 ≤ 4 KiB | `payload` |
| `value` 恰好两个键、`action ∈ {retry,ignore}`、`source_workflow_id` 非空 | `payload` |
| **当前 readiness 为真**（§7.1） | `disabled` |
| open_id 在 `FEISHU_CMD_WHITELIST` 内 | `unauthorized` |

白名单复用 `FEISHU_CMD_WHITELIST`：同一批人有权发指令就有权点按钮，再引入第二份名单只会漂移。

非白名单提示必须走**同步 callback toast**，绝不能用 `SendText`——后者会发到固定的
`FEISHU_RECEIVE_ID`（可能是群），等于把一次未授权点击广播给所有人。

### 6.3 审计口径

审计对象是**人的点击尝试与 accepted action**，不是内部执行状态迁移。

| 场景 | action | FK |
|---|---|---|
| 接受（claim 事务内，恰好一行） | `card.retry.accepted` / `card.ignore.accepted` | 设置 |
| 非白名单 | `card.<action>.rejected.unauthorized` | NULL |
| readiness 关闭 | `card.<action>.rejected.disabled` | NULL |
| 来源/载荷不合法 | `card.unknown.rejected.payload` | NULL |
| 无权威 run | `card.<action>.rejected.legacy` | NULL |
| 已被占 | `card.<action>.rejected.conflict` | NULL |
| 源未关闭 / 结果不可读 / 无失败变体 / 缺 artifact | `card.<action>.rejected.<reason>` | NULL |
| 失败后重试、恢复、Start、finalize、Patch | **不写审计** | — |

- `actor` 固定 `feishu:<open_id>`；空身份用固定值 `feishu:unknown`。
- `target` = `source_workflow_id`（无法解析时空串）。
- `payload_digest` = 对**经过大小限制后的**原始 action payload 做 canonical JSON 的 SHA-256。
  **不保存不可信原文。**
- inbox 去重跑在拒绝审计**之前**；不同的真实点击各留一行。

---

## 7. 卡片形态

### 7.1 readiness —— 五项合取

按钮可用性是运行时配置与运行时状态的合取，不能进 activity 载荷——activity 输入由 workflow
写进 history，worker 装配时改不了。

1. `FEISHU_CARD_ACTIONS_ENABLED=true`（显式开关。**本地注册 handler 不能证明飞书后台已订阅
   `card.action.trigger`**，所以必须独立成开关）；
2. `FEISHU_CMD_WHITELIST` 非空（否则 listener 根本不启动）；
3. `NewSender` 返回的 mode == `"app"`（**webhook 模式收不到回调**，缺 `ReceiveID` 时会静默
   回退到 webhook——v1 漏了这一项）；
4. card action handler 已装配；
5. **WS 当前 ready**。用 SDK v3.9.9 的生命周期钩子维护一个原子布尔：
   `WithOnReady` / `WithOnReconnected` → 置真；
   `WithOnReconnecting` / `WithOnDisconnected` / `WithOnError` → 置假；ctx 取消 → 置假。

**只能承诺"发送瞬间 ready"**：卡片发出后连接可能断开。因此回调路径必须**再查一次
readiness**（§6.2），对历史卡片上残留的按钮返回 toast「按钮已停用」并写
`rejected.disabled`——这就是 `rejected.disabled` 的实际检查入口。

不满足时 `NotifyCard` 侧**剥掉按钮**发展示卡片，其余内容逐字不变。

### 7.2 卡片 DTO 变更

1. `CardConfig` 增加 `UpdateMulti bool \`json:"update_multi"\``，**恒为 `true`**。
   飞书 `PATCH /open-apis/im/v1/messages/:message_id` 要求原卡片带 `config.update_multi = true`。
2. 新增封闭的 action 模块。**`value` 用固定字段结构体，不用 `map[string]string`**——
   map 不是封闭 DTO，多一个键的断言无从谈起：

```go
type CardActionModule struct {
    Tag     string       `json:"tag"`     // 恒为 "action"
    Actions []CardButton `json:"actions"`
}

// CardButton 没有 behaviors、没有 url、没有 multi_url——
// 按钮只能回调,不可能变成跳转或表单。
type CardButton struct {
    Tag   string          `json:"tag"`   // 恒为 "button"
    Text  CardText        `json:"text"`  // plain_text
    Type  string          `json:"type"`  // primary | default
    Value CardActionValue `json:"value"`
}

// CardActionValue 是封闭的两字段结构:序列化后恰好两个键。
type CardActionValue struct {
    Action           string `json:"action"`             // retry | ignore
    SourceWorkflowID string `json:"source_workflow_id"`
}
```

**同步影响**：展示卡片轮的递归键断言（`devicetest_test.go:1007`）把 `config` 的键集合精确
锁死为 `{"wide_screen_mode"}`，且把带 `actions` 的卡片判为非法。这两处**必须同步放宽到新的
封闭集合，而不是删除断言**——反例改为"带 `behaviors`""带 `url`""带 `multi_url`"
"button value 含第三个键""value 含 `variant`"，仍须判红。

**30 KiB 门禁不变**，action 模块与主行同级**不可裁剪**：裁剪到底仍须保留按钮；
含按钮后仍超预算则按既有规则整卡降级为纯文本（此时自然无按钮）。

### 7.3 动作后的卡片

按钮整块被替换为一行状态文本（`tag=div`, `plain_text`）：

| 状态 | 文本 | 按钮 |
|---|---|---|
| retry pending | `已由 <open_id> 重试，正在启动…` | 无 |
| retry succeeded | `已由 <open_id> 重试 → <target_workflow_id>` | 无 |
| retry failed | `重试启动失败：<last_error>` | **只保留「重新重试」** |
| ignore succeeded | `已由 <open_id> 忽略（仅记录，不改变判定）` | 无 |

失败态**只保留「重新重试」**：动作已固定，再摆一个必然 `conflict` 的 ignore 按钮是误导。
括号里"不改变判定"是 §2.2 语义的用户可见落点，不可省略。

---

## 8. 错误处理与恢复

### 8.1 三个 sweep

worker 启动时各跑一次，之后按 30 秒间隔轮询。**租约谓词必须容纳 NULL**——
v1 用 `lease_expires_at < now()` 而默认值是 NULL，首次 pending 永不命中：

```sql
WHERE <state 列> = '<pending 值>'
  AND (lease_expires_at IS NULL OR lease_expires_at < now())
```

| sweep | 表 | 动作 |
|---|---|---|
| inbox | `card_action_inbox` | CAS 取 owner → 走异步段（Resolve → 接受事务 → 执行）→ `processed` |
| 动作 | `card_actions` | CAS 取 owner → 用**钉死的** target 重新 `StartDeviceTest` → finalize |
| 卡片 | `card_action_messages` | CAS 取 owner → 渲染 `card_actions.revision` 对应的卡片 → Patch |

飞书长连接在多个 worker 实例间负载均衡，点击可能落到任一实例；owner + 租约让 A 崩溃后
B 能接管。**单实例部署下同样必要**——进程重启就是一次接管。

### 8.2 卡片收敛与迟到写

数据库 fencing 挡不住**已经发生的外部写**：A 的 PATCH 超时后 B 渲染并写成
`succeeded`，A 的请求仍可能迟到落到飞书，把卡片覆盖回旧内容。因此：

- 卡片 sweep 渲染时读取当时的 `card_actions.revision`，记为 `r`；
- PATCH 成功后执行带 fencing 的更新：
  `UPDATE ... SET rendered_revision = r, update_state = CASE WHEN r >= <当前 revision> THEN 'succeeded' ELSE 'pending' END WHERE workflow_id=? AND open_message_id=? AND owner=?`；
- **该更新影响 0 行（租约已被接管）时，写方必须无条件重排**：
  `UPDATE ... SET update_state='pending', owner='', lease_expires_at=NULL`。
  重排是安全的：同一 revision 的 PATCH 内容相同，重复渲染幂等，最坏代价是多一次 PATCH。

收敛性由两条保证：§5.4 的每次可见变化都重排；本节的迟到写被发现后也重排。
终态是 `rendered_revision = revision AND update_state='succeeded'`。

### 8.3 错误分类

| 来源 | 永久（不重试） | 暂时（退避重试） |
|---|---|---|
| `StartDeviceTest` | artifact 缺失、非法输入 | Temporal 不可达、超时 |
| 卡片 Patch | 超过飞书 14 天更新期限、message 不存在、权限不足 | 网络错误、限流、5xx |

**`StartDeviceTest` 返回 `started=false`（`AlreadyStarted`）是幂等成功，不是失败**：
target 已钉死，说明该 workflow 已在运行。finalize 为 `succeeded`。这条必须进状态机与测试。

永久错误：动作侧 → `state='failed'` + `last_error`；卡片侧 → `update_state='abandoned'`。
`attempts` 累加，退避上限后转 `abandoned`。

**卡片 Patch 的任何结果都不回写动作 `state`。** 卡片没更新成功不代表重试没发生。
toast 的 200671 属于即时层，**不影响任何状态**，只记日志。

---

## 9. 代码边界

| 位置 | 职责 |
|---|---|
| `runtime/internal/rerun`（新） | 只读 `Resolver`，被 `feishucmd` 与 `cardaction` 共用 |
| `runtime/internal/cardaction`（新） | 回调 handler、异步消费、三个 sweep、卡片渲染 |
| `runtime/internal/store` | 四张表的访问层；接受事务是**一个** store 方法 |
| `runtime/internal/feishucmd/listener.go` | 增注册 `OnP2CardActionTrigger` 与五个生命周期钩子；本身不含业务逻辑 |
| `runtime/internal/feishu` | **新增独立接口 `CardUpdater`**，只由 app sender 实现 |
| `runtime/internal/workflow/devicetest.go` | 卡片 DTO 增 `update_multi` 与 action 模块 |

**绝不往 `CardSender` 上加 `PatchCard`**：`webhookSender` 已实现 `SendCard`（`feishu.go:168`），
扩接口会让它掉出 `CardSender`，于是 `notify_card.go` 的类型断言失败，**webhook 模式的展示
卡片静默退化成纯文本**。让 webhook 假实现一个必然失败的 `PatchCard` 同样错误。

```go
// CardUpdater 只有 app sender 实现:webhook 自定义机器人没有消息更新能力。
type CardUpdater interface {
    PatchCard(ctx context.Context, messageID string, card any) error
}
```

接受事务作为**单个 store 方法**暴露（`AcceptCardAction`），事务边界不外泄，
就不存在"调用方忘了带 owner"这种失败模式。

---

## 10. 测试与验收

### 10.1 Store conformance（MemStore 与 PGStore 共用）

- 接受事务：首次成功；`retry` 行落库即已钉死；
- **`retry` 分两步写入必被拒**（先插默认值再 UPDATE → `23514`），证明"一次性插入"不可绕过；
- `ignore` 行 attempt=0、target 为空、落库即 `succeeded`；
- §5.3 三种归宿逐行覆盖；`failed → pending` 复用原 attempt/target；
- finalize fencing：owner 不匹配影响 0 行；
- 恢复不推进水位（断言 artifacts 计数器不变）；
- 一次接受恰好一行 accepted 审计；失败后重试不产生第二行；
- 拒绝审计可多行且 FK 为 NULL；
- `workflow_id` 不在 `workflow_runs` 中 → FK 拒绝；
- revision：四个触发点各 +1 并重排全部消息行；
- 卡片子状态机与动作 state 相互不影响；
- **租约谓词命中 NULL**（首次 pending 能被 sweep 选中）。

### 10.2 真实 PG 并发（不可用 MemStore 替代）

- N 个并发点击同一 workflow → 恰好一次 accepted，**artifacts 水位恰好推进一次**，
  其余 `rejected.conflict`；
- 并发 retry 与 ignore → 恰好一个胜出；
- 两个 sweep 实例并发取同一行 → 恰好一个取得 owner。

### 10.3 回调处理

- 同一 event ID 重投 3 次 → 一次 claim、**一行审计**、**三次相同 toast**（由 inbox 重放）；
- 不同真实点击各留一行审计；
- ACK 之后、claim 之前进程退出 → inbox 行留 `received`，sweep 接管后动作照常完成
  （**v1 阻断 2 的回归测试**）；
- §6.2 表格逐行覆盖：AppID 不符、tenant 空/不等、`Action.Tag != "button"`、
  `OpenMessageID` 空、host 非消息卡片、超量 payload、value 多一个键；
- 非白名单 → 同步 toast + `rejected.unauthorized`，且 `SendText` **零调用**（机械断言）；
- readiness 五项各自为假时 → `rejected.disabled`，不进 claim；
- 无失败变体 / legacy / 源仍在运行 → `NextWorkflowAttempt*` 与 `StartDeviceTest` **零调用**；
- `started=false` → finalize `succeeded`。

### 10.4 卡片

- 含 `config.update_multi = true`；
- button `value` 序列化后恰好两个键；含第三个键或含 `variant` 判红；
- 带 `behaviors` / `url` / `multi_url` 判红；
- 全绿运行不带按钮；readiness 任一项为假时不带按钮且其余内容逐字不变；
- 30 KiB 门禁下裁剪到底仍保留按钮；
- §7.3 四种状态文本逐字匹配（表格是契约），失败态**只有一个按钮**；
- 同一 workflow 多张卡片（`NotifyCard` 重试）→ 全部收敛到同一状态；
- 迟到写场景：A 失去租约后的 PATCH 被发现 → 重排 → 最终 `rendered_revision = revision`。

### 10.5 文本 rerun 不回归

`feishucmd/executor_test.go` 既有的全部 rerun 用例**一行不改即通过**。这是 `RerunResolver`
抽取正确的判据——语义共用而非复制。

### 10.6 Migration

- 独立幂等 migration，**连续执行两次结果相同**；
- fresh `schema.sql` 与 upgraded 库的最终约束一致（复用 `migration_workflow_runs_test.go` 的模式）；
- `pgtest` 的 TRUNCATE 清单补入四张表；
- **前置检查**：migration 断言 `workflow_runs` 表已存在（本轮 FK 依赖它），
  未完成 workflow_runs 生产迁移时明确失败而非静默建表。

### 10.7 重放

`history-pre-notify-card.json` 继续原样重放通过（它走 DefaultVersion 文本分支，
不调 `buildNotificationCard`，不受 DTO 影响）。

---

## 11. 部署顺序（两段，不可合并）

`update_multi` 是**无副作用的展示 DTO 变更**，而按钮涉及四张新表与交互处理。两者分开：

**第一段——并入 `workflow-runs` 的首次部署**：只带 `config.update_multi: true`
（以及对应的断言放宽）。卡片仍无按钮，行为不变，但此后发出的卡片**具备被更新的能力**。
这样零重放风险、零版本门——当前生产是 `origin/master`，不存在含 `NotifyCard` 的 history。

**第二段——`workflow-runs` rollout 稳定之后**：迁移四张新表并启用按钮。
**绝不把交互处理混进 workflow_runs 的停写窗口**——那个窗口已经同时承担了
artifact 唯一键变更与 analyze_bridge v2 切换，再叠一层新表和新回调路径，
故障归因会变得不可能。

---

## 12. 完成定义

- 终态卡片在有失败变体且 readiness 五项全真时带两个按钮，其余情况逐字保持展示卡片形态；
- 一次点击 = 一次 claim = 一行 accepted 审计，与回调到达次数无关；
- ACK 之后进程退出，动作仍由 inbox sweep 完成，不丢失；
- 重试的 attempt 与 workflow ID 在接受事务内**一次性写入**，恢复路径永不推进水位；
- 卡片最终收敛到 `rendered_revision = revision`，且 Patch 失败不污染动作状态；
- webhook 模式的展示卡片未退化（`CardSender` 未被扩展）；
- 未授权点击只得到同步 toast，`SendText` 零调用；
- 文本 `rerun` 外部行为与回复逐字不变；
- `decisions` 表未被修改；
- runtime 全测试、store PG conformance、真实 PG 并发测试、`go vet ./...` 全绿；
- CLAUDE.md §3 规则 7 / §11 与 `docs/device-test-sequence.md` 已同步修正。

---

## 13. 后续（不在本轮）

- NL 翻译 `rerun` 的确认/取消改卡片按钮（第三轮，复用本轮的鉴权、inbox、claim 与 callback 基础设施）
- 「隔离」按钮——等设备级信号源落地（差距 #10）
- 通知里带日志/证据链接
- 卡片模板化
