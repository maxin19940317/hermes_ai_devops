# 飞书终态卡片按钮设计（重试 / 忽略）

**日期：** 2026-07-31
**状态：** 待评审（v5）
**范围：** 三轮交互能力的第二轮。终态通知卡片上加两个**卡片级**按钮——「重试失败变体」「忽略」。
不含 NL 确认/取消按钮（第三轮）、隔离按钮、证据链接、卡片模板化。

前置轮：
- `2026-07-30-feishu-notification-card-design.md`（展示卡片，§10 是本轮的输入清单）
- `2026-07-30-workflow-runs-design.md`（权威运行记录，解除了 §10(3)(4)）

## 修订史

| 版本 | 评审发现的阻断 | 处置 |
|---|---|---|
| v1 → v2 | PostgreSQL 的 CHECK 不可延迟到 COMMIT，"先插后钉"必然 `23514` | §5 改为锁定后**一次性插入完整行** |
| v1 → v2 | 同步 ACK 与异步 claim 之间无持久交接，点击会永久消失 | 新增 `card_action_inbox` |
| v2 → v3 | 第二段部署改变 `NotifyCard` 的 activity input，需版本门与新 fixture | **改为 activity 注入按钮**，workflow 完全不参与，版本门问题消失（§7） |
| v2 → v3 | inbox 消费与 `processed` 不原子，重跑会写重复审计 | 接受/拒绝各收敛为**一个** store 事务（§5、§6） |
| v3 → v4 | inbox 被 acquire 两次，租约未过期使第二次必为零行，动作**永不完成** | 拆成 `ClaimInbox` → `Resolve` → `Complete*`，**Complete 只 fencing 不 acquire**（§5.1） |
| v3 → v4 | 卡片 PATCH 要求完整原卡 JSON，而回调不携带、legacy 路径也无法从 Temporal 重建 | 新增 `card_action_snapshots`，`NotifyCard` 发可点击卡片**之前**落盘（§3.4） |
| v4 → v5 | `rejected` inbox 行按默认 `received` 插入必撞 `inbox_rejected_is_terminal`，`23514` | 首次 INSERT 即写 `processed` + `processed_at`（§6.1） |
| v4 → v5 | `Complete*` 只校验 owner，租约过期的持有者仍能提交失效结果 | 所有 completion 增加 `lease_expires_at > now()`（§5.2 第 0 条） |
| v4 → v5 | 卡片 completion 缺 revision fencing，旧 worker 会把新 revision 标成 succeeded 而永久吞掉 | claim 钉住 `desired_revision`，completion 双重 fencing（§8.3 第 0 条） |

实证过的事实（不再重新论证）：

- `CHECK ... DEFERRABLE` → `SQLSTATE 0A000`；先插默认值再 UPDATE → `SQLSTATE 23514`。
- WS handler 返回 error → SDK 应答 500（`ws/client.go:638`）→ 飞书重投。
- `webhookSender` 已实现 `SendCard`（`feishu.go:168`）。
- SDK v3.9.9 提供 `WithOnReady` / `WithOnError` / `WithOnReconnecting` / `WithOnReconnected` /
  `WithOnDisconnected`。

---

## 1. 门禁裁决与实测结论

开工前跑了一次性 spike（`runtime/cmd/spike-cardaction`），对真实飞书应用验证。**裁决：GO。**

1. **WS 通道成立。** `card.action.trigger` 经长连接送达，事件订阅保持现状即可，
   **不需要公网回调 URL**。实现路径唯一：在现有 `dispatcher.EventDispatcher` 上注册
   `OnP2CardActionTrigger`。（`ws.WithCardHandler` 是注释掉的，`MessageTypeCard` 帧被
   `ws/client.go:626` 直接丢弃。）
2. **反馈分两层。** 即时层 = toast，**best-effort**（实测存在错误码 200671）；
   权威层 = 卡片延迟更新，不受 3 秒预算限制。
3. **重复投递是常态。** 实测一次 ignore 收到 **3 次**回调。
4. **载荷最小化验证通过。** `value` 只放 `action` + `source_workflow_id`。

---

## 2. 语义

### 2.1 重试

**完整复用 `rerun` 的业务语义**，不复用其调用路径。失败集合与 `rerun` 逐字一致：
源 workflow 已关闭，从 Temporal `DeviceTestOutput` 取 `verdict ∉ {PASSED, SKIPPED}` 的
`TaskSummary`。**不从 tasks 缺行推断 SKIPPED**。

重试产生**新的 workflow，并发出它自己的终态卡片**。原卡片只承载一次 chosen action。

### 2.2 忽略

**只表示人工确认**：不修改 `tasks.verdict`；不触发任何策略；**当下没有任何消费方读取它**。
这三条必须写在用户可见文档里。权威记录是 `card_actions` + `audit_log`，**不写 `decisions`**（§3.6）。

### 2.3 权威性边界（重要）

- **数据库中的动作状态是权威的。** 重试是否发生、忽略是否被记录，以 `card_actions` 为准。
- **卡片更新是 best-effort 的最终呈现。** 飞书 `PATCH` **没有条件版本写**，因此
  **本设计不承诺卡片严格收敛**——理由与缓解见 §8.3。任何依赖"卡片显示什么"来判断
  "系统做了什么"的读法都是错的。

---

## 3. 数据模型

五张表。`schema.sql` 与独立 migration 同步。

### 3.1 card_action_inbox —— 同步段的持久交接

同步应答一旦返回成功，飞书**不再重投**。因此返回之前必须先落盘。

```sql
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
```

**没有指向 `workflow_runs` 或 `card_actions` 的外键**——同步段还没做权威校验，此时唯一确定的
事实是"一个形态合法、身份获授权的点击到达过"。

> **`disposition='rejected'` 的行必须在第一条 INSERT 里就写成终态**：
> `state='processed', processed_at=now()`。CHECK 立即生效（与 §3.2 同一条实证事实），
> 先按默认 `received` 插入、再 UPDATE 成 `processed` 的写法会在 **INSERT 当场** 撞
> `inbox_rejected_is_terminal` 拿到 `23514`。`state` 的 `DEFAULT 'received'` 只服务
> `accepted` 分支。

`event_id` 主键承担三件事：跨进程去重、跨重启去重（进程内 `dedupCache` 重启即失效）、
以及**同步应答重放**——重投读出既有行返回同一条 `ack_toast`，既满足"三次相同 toast"，
又保证审计不重复。

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
```

**`workflow_id` 做主键**一次性消解三个问题：互斥（一次运行最多一个 claim，`action` 只是列）、
不同点击间的竞争、legacy 隔离（FK 让没有权威行的历史运行不可能产生 claim）。

**`target_input JSONB`**：`StartDeviceTest(ctx, in wf.DeviceTestInput)` 收完整输入，
不能按 ID 启动，所以只存 attempt/target 的恢复路径无法真正恢复。首次 claim 时持久化
canonical 序列化的完整输入；**完整性由 §5.5 的逐字段断言保证，不能只靠 `WorkflowID()`**
（后者不含 Version / RuleVersion / Packages / SourceWorkflowID）。
恢复时**逐字段复用**，不重新 Resolve。

三个 CHECK 因为 PostgreSQL 的 CHECK 不可延迟（实证见修订史），把"接受动作必须一次性写入
完整行"变成数据库层面不可违反的约束。

### 3.3 card_action_messages —— 卡片渲染，按消息唯一

`NotifyCard` 是**可重试活动**：飞书已收到、活动 ACK 丢失时会重发，同一 workflow 可能存在
多张卡片。因此按消息实例收敛。

```sql
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
```

**`rejection → action` 是单向升级。** 首次点击遇到 `StillRunning` 会写
`render_kind='rejection'`；运行结束后再点击并成功接受时，**必须整体升级**，否则卡片会永久
停在旧的拒绝理由上：

```sql
render_kind='action', rejection_reason='', buttons_mode='none',
desired_revision=<action.revision>, update_state='pending', reconcile_after=NULL
```

反向不成立：**业务拒绝绝不覆盖已有的 `render_kind='action'`**——卡片应显示胜出者的动作状态，
而不是后来者的拒绝理由。

### 3.4 card_action_snapshots —— PATCH 的原卡来源

飞书 IM `PATCH` 要求提交**完整的 card JSON**。回调载荷里没有原卡，`card_action_messages`
只存渲染状态，而 legacy / 结果不可读这些路径**无法从 Temporal 重建**卡片内容。
因此原卡必须在发送时就落盘。

```sql
CREATE TABLE IF NOT EXISTS card_action_snapshots (
    workflow_id  TEXT PRIMARY KEY,   -- 同一 workflow 的多张卡片内容相同,按 workflow 存一份
    card_json    JSONB       NOT NULL,   -- 规范化的**原展示卡**(不含 action 模块)
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

**写入时机与失败处置是硬约束**：`NotifyCard` 活动在发送**可点击卡片之前**持久化快照；
**快照持久化失败则发送无按钮的展示卡片**。绝不允许"发出了能点的按钮，却存不下原卡"——
那会让重启后的 sweep 无法兑现"其余内容不变"，只能拿一张残缺卡片去覆盖用户的通知。

sweep 渲染时从快照复制，**只替换 action 模块**（追加或替换末尾的 `tag=action` /
状态文本元素），其余元素逐字保留。

**每一个通过校验的回调都登记消息实例**——包括 `conflict` 分支和业务拒绝分支，不只是首次接受。
v2 的伪代码只在首次接受时登记，导致后来者点的那张卡片永远不更新。

**收敛承诺收窄为**：所有**已观测到的**（即产生过回调的）message instance 收敛。
系统无法发现从未被点击的重复通知卡片——那些 message ID 从未进入过任何回调载荷。

### 3.5 audit_log

CLAUDE.md §11 声明、`schema.sql` 从未建过。本轮建表并成为第一个写入方。

```sql
CREATE TABLE IF NOT EXISTS audit_log (
    audit_id                BIGSERIAL   PRIMARY KEY,
    actor                   TEXT        NOT NULL,
    action                  TEXT        NOT NULL,
    target                  TEXT        NOT NULL,
    payload_digest          TEXT        NOT NULL DEFAULT '',
    -- 一个 accepted action 恰好一条审计
    card_action_workflow_id TEXT UNIQUE REFERENCES card_actions(workflow_id),
    -- 一个 inbox 事件恰好一条审计:消费重跑不会写出第二行(v3 新增)
    inbox_event_id          TEXT UNIQUE REFERENCES card_action_inbox(event_id),
    ts                      TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

两个 UNIQUE 是正交的两层保护：`card_action_workflow_id` 挡"同一 workflow 的第二次 accepted"，
`inbox_event_id` 挡"同一事件被消费两次"。

### 3.6 owner 是随机 fencing token

所有 `owner` 列存的是**每次 acquire 新生成的随机 token**（如 128 bit hex），
**不是进程 ID 或主机名**。可复用的 owner 会让"接管后原持有者的迟到写"无法被 fencing 识别。

### 3.7 decisions 不动

**明确否决**"把 `decisions.task_id` 改成可空"——它是全局放宽，会允许 `rule` / `hermes` 的
decision 也变成无归属；且 `decisions` 没有 `workflow_id` 列，run 级归属只能藏进 JSON。

> 将来若确需把 run 级人工决策放进 `decisions`，必须新增
> `workflow_id REFERENCES workflow_runs(workflow_id)` 并加
> `CHECK ((task_id IS NULL) <> (workflow_id IS NULL))`，**不能只放宽 NOT NULL**。

### 3.8 文档同步修正

- **CLAUDE.md:37**（§3 规则 7）——终态卡片确认是 **run 级**动作，记于 `card_actions` +
  `audit_log`，不属于 task-level decision。
- **CLAUDE.md §11**——补五张表。
- **`docs/device-test-sequence.md`**——终态卡片确认**不投 workflow signal、不走 outbox**。

---

## 4. 共享 RerunResolver

文本 `rerun` 与按钮重试共用一份业务语义。新增包 `runtime/internal/rerun`
（不放进 `feishucmd`，否则 `cardaction` 要反向依赖指令包）。

### 4.0 两层,不是一层

`ignore` 是纯记录动作，**不需要任何 artifact**。若两种 action 都跑完整解析，
artifact 缺失时连"我看过了，忽略"都会失败——这是不必要的耦合。因此拆成两层：

```go
// ResolveFailureRun 只做:权威 run + 已关闭 + 失败 summary 集合。
// ignore 只需要这一层。
func (r *Resolver) ResolveFailureRun(ctx context.Context, workflowID string) (*FailureRun, error)

// ResolveRetry = ResolveFailureRun + 精确 artifacts/packages 解析。
// 只有 retry 需要这一层,ArtifactMissing 只可能从这里产生。
func (r *Resolver) ResolveRetry(ctx context.Context, workflowID, variant string) (*Resolution, error)
```

`ignore` 仍要求"权威 run 且已关闭且有失败变体"——否则忽略一个不存在或还在跑的运行没有意义；
但它**不因 artifact 缺失而失败**。

三层都只读：不分配 attempt、不写库、不启动 workflow。

### 4.1 两种模式的精确差异

**这两条是文本 `rerun` 现有行为的规格化，不是新增语义**：

`ResolveRetry` 的两种模式（`ResolveFailureRun` 恒等价于空 variant 模式的前半段）：

| | 显式 variant（文本专用） | 空 variant（按钮唯一使用） |
|---|---|---|
| `WorkflowResult` | **不调用** | 调用 |
| 允许 PASSED / SKIPPED | **允许**（用户显式选择） | 排除 |
| 目标集来源 | 参数本身 | output 中 `verdict ∉ {PASSED,SKIPPED}` 的 summary |
| 空 `Variant` 的 summary | 不适用 | 忽略 |
| 去重与排序 | 不适用 | 去重后按字典序 |
| `Scope` | = variant | = `Run.Scope` |

### 4.2 拒绝原因必须带字段

单一枚举值无法保留既有文案。拒绝原因是**带字段的封闭结构**：

```go
type RejectReason struct {
    Code       string // NotAuthoritative | StillRunning | ResultUnreadable |
                      // NoFailedVariants | VariantNotMember | ArtifactMissing
    WorkflowID string
    Variant    string // VariantNotMember / ArtifactMissing
    Count      int    // ArtifactMissing:实际命中的 artifact 行数
}
```

`ArtifactMissing` 必须带 `Count`，否则文本 `rerun` 现有的
`变体 %s 的 artifact 数量为 %d，要求恰好 1 个` 无法逐字复现——而"回复逐字不变"是本轮验收项。

`Resolve` 含 Temporal 调用，**必须在数据库事务之外执行**。

---

## 5. 接受动作：一个事务

### 5.1 边界

CHECK 不可延迟，所以顺序是**先锁、再算、最后一次性插入完整行**；且 inbox 的 `processed`
必须与业务写入同事务，否则重跑会写出重复审计。

**acquire 只发生一次。** v3 在 sweep 里取过 owner，又要求接受事务里再 CAS 一次
"lease 为空或已过期"——刚取得的租约当然没过期，第二次 UPDATE 必为零行，动作**永远无法完成**。
固定为 claim / resolve / complete 三段，`Complete*` **不再 acquire，只做 fencing 校验**：

```text
① ClaimInbox(eventID) → 新随机 token
      UPDATE card_action_inbox
         SET owner=$token, lease_expires_at=now()+120s, attempts=attempts+1
       WHERE event_id=$e AND state='received'
         AND (lease_expires_at IS NULL OR lease_expires_at < now())
      RETURNING *
      └ 0 行 → 已被他人持有或已处理,本轮跳过

② [事务外·只读] ResolveFailureRun / ResolveRetry(§4)→ 目标集,或 RejectReason

③ CompleteAccept(eventID, token, ...) —— 单事务,不再 acquire
```

```text
BEGIN
  SELECT * FROM card_action_inbox
   WHERE event_id=$e AND state='received'
     AND owner=$token AND lease_expires_at > now()   -- 租约必须仍然有效
   FOR UPDATE
    └ 0 行 → 租约已被接管或已处理,直接返回,不做任何业务写入

  SELECT 1 FROM workflow_runs WHERE workflow_id=$w FOR UPDATE
    └ 无行 → 走 §6.4 的拒绝分支(同一事务内完成)

  SELECT * FROM card_actions WHERE workflow_id=$w      -- 已在上面的行锁保护下
    ├ 无行(首次) →
    │     retry : 推进水位得 N → Go 侧算 target_input 与 target_workflow_id
    │             按 §5.5 逐字段断言 target_input 来自 Resolution
    │     INSERT card_actions(...完整行,含 attempt/target/target_input/revision=1,
    │                         owner=$actionToken, lease_expires_at=now()+120s)
    │     INSERT audit_log(card.<action>.accepted, card_action_workflow_id=$w, inbox_event_id=$e)
    ├ 同 action 且 state='failed' → §5.3 失败后重试
    │     CAS failed→pending,复用原三字段,取新 $actionToken 与租约,revision+1
    │     INSERT audit_log(card.retry.resumed, card_action_workflow_id=NULL, inbox_event_id=$e)
    └ 其余 → INSERT audit_log(card.<action>.rejected.conflict, FK=NULL, inbox_event_id=$e)

  INSERT card_action_messages(workflow_id, open_message_id, render_kind='action', ...)
    ON CONFLICT (workflow_id, open_message_id) DO UPDATE SET
        render_kind='action', rejection_reason='', buttons_mode='none',
        desired_revision=<当前 revision>, update_state='pending', reconcile_after=NULL
    -- 所有分支都登记,包括 conflict;rejection → action 是单向升级(§3.3)

  UPDATE card_action_inbox SET state='processed', processed_at=now() WHERE event_id=$e
COMMIT
  └ 返回 $actionToken 给立即执行方(首次接受与失败后重试两个分支)

[事务外·幂等] retry: StartDeviceTest(target_input) → finalize(带 $actionToken fencing)
[事务外·幂等] 卡片 sweep: 从 §3.4 快照复制 → 替换 action 模块 → PATCH
```

`SELECT ... FOR UPDATE` 加在 `workflow_runs` 而非 `card_actions`：首次点击时 `card_actions`
行**还不存在**，不存在的行锁不住。父表行锁是唯一能串行化"检查是否已有 claim → 决定是否插入"
的位置。`INSERT card_actions` 自身取的 `FOR KEY SHARE` 弱于已持有的 `FOR UPDATE`，
不存在锁升级死锁。

水位分配要能组合进本事务，因此 `NextWorkflowAttemptAll` 需抽出 tx 作用域的内部函数
（当前实现自己 `BeginTx`，见 `postgres_fleet.go:95`）。

**验收要求真实 PG 并发测试**：N 个并发点击同一 workflow → 恰好一次 accepted、
artifacts 水位**恰好推进一次**、其余全部 `rejected.conflict`。

### 5.2 三条 fencing 规则

0. **completion 必须同时校验 owner 与租约有效性。** 只比对 `owner=$token` 不够：
   租约到期到下一个 worker 重新 claim 之间存在窗口，期间**过期的持有者仍然握着匹配的
   token**，可以把一份早已失效的解析结果提交进去。所有 `Complete*` 与 finalize
   都必须带 `AND lease_expires_at > now()`，且该判定在 `FOR UPDATE` 行锁内完成——
   新的 claim 会阻塞在同一把锁上，因此不存在"检查通过后瞬间被抢走"的漏洞。
1. **finalize 必须带 fencing**：
   `UPDATE ... WHERE workflow_id=? AND state='pending' AND owner=? AND lease_expires_at > now()`。
2. **恢复与 `failed → pending` 一律复用原 `attempt` / `target_workflow_id` / `target_input`，
   绝不推进水位、绝不重新 Resolve。** 水位推进只发生在首次接受的事务里。
3. **租约只管活性，不管正确性。** 正确性来自"钉死 + CAS"：重复执行只会用同一个 workflow ID
   撞 Temporal `RejectDuplicate`。租约取 **120 秒**。

### 5.3 已有行的三种归宿

| 既有行 | 本次点击 | 归宿 |
|---|---|---|
| 任意 action，`pending` / `succeeded` | 任意 | **被占**：`rejected.conflict` 审计 + 登记本次 message |
| `action` 相同，`state='failed'` | 相同 action | **失败后重试**：CAS `failed → pending`，取新 owner，**复用原 attempt / target / target_input**，`revision + 1`，重排该 workflow 全部消息行；不写新的 accepted 审计 |
| `action` 不同，`state='failed'` | 另一 action | **被占**：`rejected.conflict` 审计 |

**动作在首次接受时即固定，不可改换。** retry 失败后只能重试 retry——否则钉死的三个字段
就得清空，与两个 CHECK 直接冲突。这也是 §7.4 失败态**只保留「重新重试」**的原因：
摆一个必然 `conflict` 的 ignore 按钮是误导。

### 5.4 revision 与卡片重排

每一次对用户可见的状态变化，都在同一事务内 `revision + 1` 并把该 workflow 全部
`card_action_messages` 置 `update_state='pending'`、`desired_revision = 新 revision`、
**无条件清空 owner 与租约**。触发点恰好四个：

| 变化 | revision |
|---|---|
| 首次接受 | 置 1 |
| retry finalize → `succeeded` | +1 |
| retry finalize → `failed` | +1 |
| `failed → pending` | +1 |

重排时**必须同时清空 `reconcile_after`**（§8.3）：新状态应当立即可更新，
不该被上一轮的延迟复核窗口压住。

这里可以**无条件**清 owner，而 §8.3 第 2 条要求"不得抹掉新 owner"——两者不矛盾：
revision 推进由 §8.3 第 0 条的 `desired_revision` fencing 兜底，任何在途写方的 completion
都会因 revision 不匹配而影响 0 行，清掉它的 owner 不会造成丢更新。
§8.3 第 2 条约束的是**失去租约的写方自己**去改行时不得越权，是另一个场景。

### 5.5 target_input 的完整性断言

`WorkflowID()` 只由 Project / Commit / PipelineID / Scope / Attempt 构成，**不包含**
Version、RuleVersion、Packages、SourceWorkflowID。因此
`target_input.WorkflowID() == target_workflow_id` **证明不了输入完整**，缺字段的输入照样通过。

接受事务内必须**逐字段**断言 `target_input` 来自本次 `Resolution`：

| 字段 | 来源 |
|---|---|
| `Project` / `Commit` / `PipelineID` / `Version` / `RuleVersion` | `Resolution.Run` 逐字段 |
| `Packages` | `Resolution.Packages`，顺序与内容逐元素相等 |
| `Scope` | `Resolution.Scope` |
| `SourceWorkflowID` | = 源 `workflow_id` |
| `Attempt` | = 本事务内分配的 N |

任一不符即中止事务（视为实现缺陷，不是用户错误）。`WorkflowID()` 一致性作为**附加**断言保留。

---

## 6. 回调处理

### 6.1 同步段

飞书响应预算约 3 秒，`Resolve` 含两次 Temporal 调用，不能放进同步段。切分点是 inbox。

同步段**只做无 I/O 校验 + 一次 inbox 写入**，整体带**固定 2 秒 deadline**：

1. §6.2 的来源与载荷校验、身份、白名单、readiness（全部无 I/O）。
2. 单事务写 inbox，**两个分支的 INSERT 形态不同**：
   ```text
   rejected: INSERT ... (disposition='rejected', state='processed', processed_at=now(), ...)
             ON CONFLICT (event_id) DO NOTHING
             ↳ 插入成功 → 同事务 INSERT 拒绝审计(inbox_event_id=$e) → 返回精确 toast
   accepted: INSERT ... (disposition='accepted', state='received', ...)   -- 用列默认值
             ON CONFLICT (event_id) DO NOTHING
             ↳ 插入成功 → 返回 toast「已收到，正在处理」,交异步段
   ```
   - **影响 0 行（重投）** → 读出既有行，**原样重放 `ack_toast`**，不写审计、不重复处理。

   `rejected` 行**绝不能先插 `received` 再 UPDATE**——CHECK 立即生效，INSERT 当场 `23514`。
3. 拿不到 `event_id` 的畸形事件：拒绝 + 只记日志，**不写审计**（无法去重的审计在重投下无界增长）。

**inbox 持久化失败（含 2 秒超时）必须向 SDK 返回 error，不得返回成功 toast。**
`ws/client.go:638` 会把 error 应答成 500，飞书据此重投——这正是我们要的。返回成功 toast
等于把一次点击静默丢弃。

### 6.2 来源与载荷校验（fail-closed）

**全部在任何持久副作用之前完成**：

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

白名单复用 `FEISHU_CMD_WHITELIST`：同一批人有权发指令就有权点按钮。

非白名单提示必须走**同步 callback toast**，绝不能用 `SendText`——后者会发到固定的
`FEISHU_RECEIVE_ID`（可能是群），等于把未授权点击广播给所有人。

### 6.3 异步段

`ClaimInbox` → `ResolveFailureRun` / `ResolveRetry` → `CompleteAccept`（§5.1）或
`CompleteReject`（§6.4）→ 执行 → 卡片 sweep。用 listener 的生命周期 ctx，
**不用回调 ctx**（后者在应答返回时即取消）。

### 6.4 业务拒绝也是一个事务

`Resolve*` 返回 `RejectReason`（legacy / 仍在运行 / 结果不可读 / 无失败变体 / 缺 artifact）时，
**不能只写审计然后各自标记**。收敛为单事务 `CompleteReject(eventID, token, reason)`——
与 `CompleteAccept` 一样**只做 fencing 校验，不再 acquire**：

```text
BEGIN
  SELECT * FROM card_action_inbox
   WHERE event_id=$e AND state='received'
     AND owner=$token AND lease_expires_at > now()   -- 租约必须仍然有效
   FOR UPDATE
    └ 0 行 → 直接返回,不做任何业务写入
  INSERT audit_log(card.<action>.rejected.<code>, inbox_event_id=$e, card_action_workflow_id=NULL)
  INSERT card_action_messages(..., render_kind='rejection', rejection_reason=<渲染文案>,
                              buttons_mode=<§7.5 按原因固定>, update_state='pending')
    ON CONFLICT (workflow_id, open_message_id) DO UPDATE SET ...
      WHERE card_action_messages.render_kind <> 'action'   -- 绝不覆盖已有的 action
  UPDATE card_action_inbox SET state='processed', processed_at=now()
COMMIT
```

这就是 `card_action_messages` **不能引用 `card_actions`** 的原因：这些路径没有 action 行，
但结论仍须呈现到卡片上。

冲突处理：目标行已是 `render_kind='action'` 时**保留 action**——卡片应显示胜出者的动作状态，
而不是后来者的拒绝理由。

### 6.5 审计口径

审计对象是**人的点击尝试与 accepted action**，不是内部执行状态迁移。

| 场景 | action | workflow FK | event FK |
|---|---|---|---|
| 接受 | `card.retry.accepted` / `card.ignore.accepted` | 设置 | 设置 |
| 非白名单 | `card.<action>.rejected.unauthorized` | NULL | 设置 |
| readiness 关闭 | `card.<action>.rejected.disabled` | NULL | 设置 |
| 来源/载荷不合法 | `card.unknown.rejected.payload` | NULL | 设置 |
| 无权威 run / 仍在运行 / 结果不可读 / 无失败变体 / 缺 artifact | `card.<action>.rejected.<code>` | NULL | 设置 |
| 已被占 | `card.<action>.rejected.conflict` | NULL | 设置 |
| **失败后重试**（§5.3） | `card.retry.resumed` | **NULL** | 设置 |
| 恢复、Start、finalize、Patch | **不写审计** | — | — |

**`card.retry.resumed` 是 v4 补的。** 在失败卡片上再点一次是**一次新的真实人工操作**，
不记录它，"审计人的点击尝试"就是不完整的。它不写第二条 `accepted`
（`card_action_workflow_id` 的 UNIQUE 已被首次接受占用），因此 workflow FK 留 NULL、
只设 `inbox_event_id`——这也正是"每个 inbox event 恰好一条审计"与"不重复 accepted"
两条约束能同时成立的方式。

- `actor` 固定 `feishu:<open_id>`；空身份用 `feishu:unknown`。
- `target` = `source_workflow_id`（无法解析时空串）。
- `payload_digest` = 同步段对**经过大小限制后的**原始 payload 做 canonical JSON 的 SHA-256，
  **存进 inbox**；异步段与恢复路径直接取用，**不重新计算**（原始 payload 已不在手上）。

---

## 7. 卡片形态：按钮由 activity 注入

### 7.1 架构决定：workflow 完全不参与

**`buildNotificationCard` 一行不改，`NotifyCard` 的 activity input 逐字节不变。**
按钮由 `NotifyCard` 活动在发送前注入。

这条决定同时解决三个问题：

1. **不需要 Temporal 版本门。** workflow 的 command 序列与活动入参都没变，
   第一段部署产生的 history 在第二段代码下重放无异议，不需要录制新 fixture。
2. **readiness 天然落在正确的位置。** readiness 是运行时状态（§7.2 含 WS 连接活性），
   activity 输入由 workflow 写进 history、worker 装配时改不了——注入本来就只能在活动侧做。
3. **预算路径闭合。** workflow 按**不含按钮**的卡片裁剪（与今天完全一致），
   活动侧再注入。若注入后超预算，**发送原展示卡片（无按钮），不降级为纯文本**。
   反过来做（workflow 按含按钮卡片裁剪、活动再剥掉）会导致 readiness 关闭时详情被多裁，
   违反"其余内容逐字不变"。

DTO 上要能表达 action 元素，但 **workflow 永不设置它**：

```go
// CardElement 是封闭的 tagged union,三种形态互斥:
//   tag=div    → Text 非空, Actions 为 nil
//   tag=hr     → 两者皆 nil
//   tag=action → Text 为 nil, Actions 非空(只由 NotifyCard 活动构造)
type CardElement struct {
    Tag     string       `json:"tag"`
    Text    *CardText    `json:"text,omitempty"`
    Actions []CardButton `json:"actions,omitempty"`
}
```

`omitempty` + workflow 永不设值 ⇒ workflow 产出的卡片序列化**逐字节不变**，
这是"无需版本门"的机械依据，必须有测试锁住（§10.7）。

按钮 `value` 用固定字段结构体，不用 `map[string]string`——map 不是封闭 DTO，
"恰好两个键"的断言无从谈起：

```go
type CardButton struct {
    Tag   string          `json:"tag"`   // 恒为 "button"
    Text  CardText        `json:"text"`  // plain_text
    Type  string          `json:"type"`  // primary | default
    Value CardActionValue `json:"value"`
}

// 序列化后恰好两个键。没有 behaviors、没有 url、没有 multi_url。
type CardActionValue struct {
    Action           string `json:"action"`             // retry | ignore
    SourceWorkflowID string `json:"source_workflow_id"`
}
```

**`config.update_multi`** 恒为 `true`（飞书 `PATCH /open-apis/im/v1/messages/:message_id`
的前置要求）。这是**唯一**的 workflow 侧 DTO 变更，随第一段部署上线（§11）。

#### 注入所依据的两个事实（必须钉死）

活动拿到的只有 `NotifyCardRequest`，没有 `TaskSummary` 列表，所以注入判据必须完全来自
活动自身可信的两个来源：

1. **`source_workflow_id` = `activity.GetInfo(ctx).WorkflowExecution.ID`。**
   不从卡片正文、不从 fallback text、不从任何用户可见字符串里解析——那些是展示内容，
   格式随时可变。ID 为空即**不注入**。
2. **失败资格 = `Card.Header.Template ∈ {red, orange}`。**
   展示卡片轮的 `cardHeaderTemplate` 已经定义：红 = 存在非 INFRA 失败，橙 = 失败全是 INFRA，
   绿 = 无失败。因此 `red|orange` 恰好等价于"存在 `verdict ∉ {PASSED, SKIPPED}` 的变体"。
   `green` 与**任何未知 template** 都不注入（fail-closed）。

**禁止解析 fallback text 或正文元素**来推断上述任一事实。

因为要读 `activity.GetInfo(ctx)`，注入逻辑的测试**必须跑在 Temporal 的 activity testsuite
环境里**——`context.Background()` 缺少 activity interceptor，`GetInfo` 会 panic 或取到零值，
用它写出来的测试证明不了生产行为。

**同步影响**：展示卡片轮的递归键断言（`devicetest_test.go:1007`）把 `config` 键集合锁死为
`{"wide_screen_mode"}`、把带 `actions` 的卡片判为非法。两处**必须同步放宽到新的封闭集合，
而不是删除断言**——反例改为"带 `behaviors`""带 `url`""带 `multi_url`"
"button value 含第三个键""value 含 `variant`""tag=div 同时带 actions"，仍须判红。

### 7.2 readiness —— 五项合取

1. `FEISHU_CARD_ACTIONS_ENABLED=true`（显式开关。**本地注册 handler 不能证明飞书后台已订阅
   `card.action.trigger`**）；
2. `FEISHU_CMD_WHITELIST` 非空（否则 listener 不启动）；
3. `NewSender` 返回的 mode == `"app"`（**webhook 模式收不到回调**，缺 `ReceiveID` 会静默回退）；
4. card action handler 已装配；
5. **WS 当前 ready**：用 SDK 生命周期钩子维护原子布尔——
   `WithOnReady` / `WithOnReconnected` → 真；
   `WithOnReconnecting` / `WithOnDisconnected` / `WithOnError` → 假；ctx 取消 → 假。

**只能承诺"发送瞬间 ready"**。因此回调路径必须**再查一次 readiness**（§6.2），
对历史卡片上残留的按钮返回 toast「按钮已停用」并写 `rejected.disabled`。

### 7.3 更新接口：独立的 CardUpdater

**绝不往 `CardSender` 上加 `PatchCard`**：`webhookSender` 已实现 `SendCard`（`feishu.go:168`），
扩接口会让它掉出 `CardSender`，于是 `notify_card.go` 的类型断言失败，
**webhook 模式的展示卡片静默退化成纯文本**。让 webhook 假实现一个必然失败的 `PatchCard`
同样错误。

```go
// CardUpdater 只由 app sender 实现:webhook 自定义机器人没有消息更新能力。
type CardUpdater interface {
    PatchCard(ctx context.Context, messageID string, card any) error
}
```

### 7.4 动作后的卡片

按钮整块被替换为一行状态文本（`tag=div`, `plain_text`）：

| 状态 | 文本 | 按钮 |
|---|---|---|
| retry pending | `已由 <open_id> 重试，正在启动…` | 无 |
| retry succeeded | `已由 <open_id> 重试 → <target_workflow_id>` | 无 |
| retry failed | `重试启动失败：<last_error>` | **只保留「重新重试」** |
| ignore succeeded | `已由 <open_id> 忽略（仅记录，不改变判定）` | 无 |

括号里"不改变判定"是 §2.2 语义的用户可见落点，不可省略。

### 7.5 业务拒绝的按钮：按原因固定，不一刀切

拒绝态是否留按钮取决于**该拒绝还有没有合法归宿**，因此按原因固定，并把结论持久化成
`card_action_messages.buttons_mode` 这个封闭枚举：

| 拒绝原因 | `buttons_mode` | 理由 |
|---|---|---|
| `StillRunning` | `both` | 运行结束后重试就会成功；也允许直接 ignore |
| `ResultUnreadable` | `both` | 可能是暂时的 |
| `ArtifactMissing`（retry 专有） | `both` | 补齐产物后可重试；ignore 本就不需要 artifact（§4.0） |
| `NotAuthoritative` | `none` | legacy 运行永远不会有权威行，再点没有归宿 |
| `NoFailedVariants` | `none` | 没有可重试的东西，ignore 也无意义 |

`buttons_mode='both'` 时保留原本的两个按钮；`'none'` 时只留状态文本。
`render_kind='action'` 的行不看这一列——它的按钮集合由上表的动作状态决定。

---

## 8. 错误处理与恢复

### 8.1 三个 sweep

worker 启动时各跑一次，之后 30 秒轮询。**租约谓词必须容纳 NULL**——
`lease_expires_at < now()` 配 NULL 默认值时首次 pending 永不命中：

```sql
WHERE <state 列> = '<pending 值>'
  AND (lease_expires_at IS NULL OR lease_expires_at < now())
```

**卡片 sweep 的谓词还要多一条**，否则 §8.3 约定的 60 秒延迟复核形同虚设——
超时后行仍是 `pending`，30 秒的下一轮就会立刻重试：

```sql
  AND (reconcile_after IS NULL OR reconcile_after <= now())
```

| sweep | 表 | 动作 |
|---|---|---|
| inbox | `card_action_inbox` | `ClaimInbox` 取 token → `Resolve*` → `CompleteAccept` / `CompleteReject`（§5.1，**Complete 不再 acquire**） |
| 动作 | `card_actions` | CAS 取 owner → 用**持久化的 `target_input`** 重新 `StartDeviceTest` → finalize |
| 卡片 | `card_action_messages` | CAS 取 owner → 从 §3.4 快照复制并替换 action 模块 → PATCH |

### 8.2 错误分类

| 来源 | 永久（不重试） | 暂时（退避重试） |
|---|---|---|
| `StartDeviceTest` | artifact 缺失、非法输入 | Temporal 不可达、超时 |
| 卡片 PATCH | 超过飞书 14 天更新期限、message 不存在、权限不足 | 网络错误、限流、5xx |

**`StartDeviceTest` 返回 `started=false`（`AlreadyStarted`）是幂等成功，不是失败**：
target 已钉死，说明该 workflow 已在运行，finalize 为 `succeeded`。这条必须进状态机与测试。

永久错误：动作侧 → `state='failed'` + `last_error`；卡片侧 → `update_state='abandoned'`。

**卡片 PATCH 的任何结果都不回写动作 `state`。** toast 的 200671 属于即时层，只记日志。

### 8.3 卡片收敛的诚实边界

飞书 `PATCH` **没有条件版本写**，因此存在一个**无法用数据库手段消除**的反例：

> A 发出 PATCH(rev=1) 后**超时，没有拿到任何响应**；B 接管、PATCH(rev=2) 成功并标 `succeeded`；
> 此后 A 的旧请求才在飞书端生效。系统**永远不会收到 A 的结果**，数据库却认为已收敛，
> 而卡片显示的是旧内容。

对此**不承诺严格收敛**（§2.3 已把权威性交给数据库），只做四件事：

0. **claim 时钉住 revision，completion 双重 fencing。** 卡片 sweep 取得 owner 时把当时的
   `desired_revision` 一并钉住，记为 `r`，渲染的就是 `r` 对应的卡片；PATCH 之后的更新必须
   同时匹配 owner、租约有效性**与 `r`**：

   ```sql
   UPDATE card_action_messages
      SET rendered_revision = $r, update_state = 'succeeded', reconcile_after = NULL
    WHERE workflow_id = ? AND open_message_id = ?
      AND owner = $token AND lease_expires_at > now()
      AND desired_revision = $r          -- ← 缺它会吞掉新状态
   ```

   **`desired_revision = $r` 不可省。** 只比对 owner 时，若 PATCH rev1 期间动作推进到 rev2
   （§5.4 已把该行重置为 `pending`、`desired_revision=2`），旧 worker 仍能凭 owner 把它标成
   `succeeded`，**rev2 从此不再被 sweep 选中**，卡片永久停在 rev1。
   同一条 fencing 也用于超时路径的 `reconcile_after` 写入。

1. **明确超时 ≠ 失败。** PATCH 返回超时或不确定错误时，**不得**直接标 `succeeded` 或
   `abandoned`；保持 `update_state='pending'` 并写入 `reconcile_after = now() + 60s`，
   由卡片 sweep 在到期后**再渲染一次当前 `desired_revision`**，成功后清空 `reconcile_after`。
   重复渲染同一 revision 是幂等的，最坏代价是多一次 PATCH。
   该延迟**由 §8.1 的谓词强制**（`reconcile_after IS NULL OR reconcile_after <= now()`），
   不是靠调用方自觉。
   反过来，§5.4 的每次 revision 重排**必须清空 `reconcile_after`**——新状态比"复核旧状态"
   更值得立即呈现，压着它没有意义。
2. **重排不得抹掉新 owner。** 失去租约的写方只能清除**自己持有的** owner：
   `UPDATE ... SET owner='', lease_expires_at=NULL WHERE ... AND owner = <my token>`。
   owner 已换人时只设 `reconcile_after`，不碰 owner——否则会把新持有者踢掉，形成互相抢占。
   这也是 §3.5 要求 owner 是**不可复用随机 token** 的原因。
3. **文档写明。** 用户可见文档与 §12 都必须写明"卡片是最终呈现、数据库是权威"。

---

## 9. 代码边界

| 位置 | 职责 |
|---|---|
| `runtime/internal/rerun`（新） | 只读 `Resolver`，被 `feishucmd` 与 `cardaction` 共用 |
| `runtime/internal/cardaction`（新） | 回调 handler、异步消费、三个 sweep、卡片渲染与注入 |
| `runtime/internal/store` | 五张表访问层；`ClaimInbox` / `CompleteAccept` / `CompleteReject` 各是**一个**方法，事务边界不外泄 |
| `runtime/internal/feishucmd/listener.go` | 增注册 `OnP2CardActionTrigger` 与五个生命周期钩子；不含业务逻辑 |
| `runtime/internal/feishu` | 新增独立接口 `CardUpdater`，只由 app sender 实现 |
| `runtime/internal/activity/notify_card.go` | readiness 判定与按钮注入 |
| `runtime/internal/workflow/devicetest.go` | **只加 `update_multi` 与 `CardElement.Actions` 字段**；`buildNotificationCard` 逻辑不变 |

---

## 10. 测试与验收

### 10.1 Store conformance（MemStore 与 PGStore 共用）

- 接受事务：首次成功；`retry` 行落库即已钉死 attempt/target/target_input；
- **`retry` 分两步写入必被拒**（先插默认值再 UPDATE → `23514`）；
- `ignore` 行 attempt=0、target 空、target_input NULL、state 恒为 `succeeded`
  （`card_actions_ignore_terminal` 的机械证明）；
- **`target_input` 逐字段断言**（§5.5）：缺 `Version` / `RuleVersion` / `Packages` /
  `SourceWorkflowID` 任一字段的输入必须被拒——**只断言 `WorkflowID()` 相等的实现会放过它们**，
  测试要能证明这一点；
- **`ClaimInbox` → `Complete*` 全路径**：Complete 在**租约仍然有效**时必须成功
  （v3 的"再 acquire 一次"写法会在这里零行，是 v4 阻断 1 的回归测试）；
  token 不匹配时 Complete 影响 0 行且不做任何业务写入；
- **租约过期后 Complete 必须失败**：token 正确但 `lease_expires_at` 已过期 → 影响 0 行、
  零业务写入（v5 第 2 条回归；只比对 owner 的实现会在这里放过一份失效的解析结果）；
- **`rejected` 行首次 INSERT 即为 `processed` + `processed_at`**；
  先插 `received` 再 UPDATE 的写法必须撞 `23514`（v5 第 1 条回归）；
- §5.3 三种归宿逐行覆盖；`failed → pending` 复用原三字段且不重新 Resolve；
- finalize fencing：owner 不匹配影响 0 行；
- 恢复不推进水位（断言 artifacts 计数器不变）；
- 一次接受恰好一行 accepted 审计；**同一 event 重复消费不写第二行**（`inbox_event_id` UNIQUE）；
- `workflow_id` 不在 `workflow_runs` 中 → FK 拒绝；
- revision 四个触发点各 +1 并重排全部消息行；
- **租约谓词命中 NULL**；`attempts` 负值被拒；`processed` 与 `processed_at` 必须配对。

### 10.2 真实 PG 并发

- N 个并发点击同一 workflow → 恰好一次 accepted，**水位恰好推进一次**，其余 `conflict`；
- 并发 retry 与 ignore → 恰好一个胜出；
- 两个 sweep 实例并发取同一行 → 恰好一个取得 owner。

### 10.3 inbox 与崩溃恢复

- 同一 event ID 重投 3 次 → 一次 claim、**一行审计**、**三次相同 toast**（inbox 重放）；
- **ACK 之后、claim 之前进程退出** → inbox 留 `received`，sweep 接管后动作完成（v1 阻断 2 回归）；
- **`CompleteAccept` 提交后、标 processed 前崩溃不可能发生**（同事务），
  以"重跑同一 event 不产生第二行审计"机械证明（v2 阻断 2 回归）；
- **Start 成功、finalize 前崩溃** → 恢复用持久化的 `target_input` 逐字段相同地重提，
  `started=false` → 收敛为 `succeeded`；
- inbox 持久化失败 → handler **返回 error**（断言 SDK 应答非 200），不返回成功 toast。

### 10.4 回调校验

- §6.2 表格逐行覆盖：AppID 不符、tenant 空/不等、`Action.Tag != "button"`、
  `OpenMessageID` 空、host 非消息卡片、超量 payload、value 多一个键；
- 非白名单 → 同步 toast + `rejected.unauthorized`，且 `SendText` **零调用**（机械断言）；
- readiness 五项各自为假 → `rejected.disabled`，不进 claim；
- 无失败变体 / legacy / 源仍在运行 → `NextWorkflowAttempt*` 与 `StartDeviceTest` **零调用**，
  且经 `CompleteReject` 在卡片上留下结论。

### 10.4a 卡片快照与升级（v4 新增）

- `NotifyCard` 发**可点击**卡片前必先写入 `card_action_snapshots`；
  **快照写入失败 → 发送无按钮展示卡片**（机械断言：注入路径零调用）；
- sweep 从快照渲染：除 action 模块外**所有元素与快照逐字节相同**；
- 无快照时不可能进入 PATCH 路径（断言）；
- **`rejection → action` 单向升级**：先 `StillRunning` 写 rejection，运行结束后再点击成功，
  卡片必须显示动作状态而非旧拒绝理由（v4 阻断 5 回归）；
- 反向：已有 `render_kind='action'` 时业务拒绝**不覆盖**；
- `buttons_mode`：§7.5 表格逐行覆盖，`none` 的两种原因不留按钮，`both` 的三种保留两个按钮。

### 10.4b activity 注入（必须在 Temporal activity testsuite 中执行）

- `source_workflow_id` 取自 `activity.GetInfo(ctx).WorkflowExecution.ID`；
- header `red` / `orange` 注入，`green` 与**未知 template** 不注入（fail-closed）；
- 空 workflow ID 不注入；
- **断言注入逻辑不读取 `FallbackText` 与正文元素**（可用只改正文、不改 header 的用例证明
  注入结果不变）；
- 用 `context.Background()` 写的测试不算数——`GetInfo` 在其中取不到真实值。

### 10.5 Resolver 兼容

- **`feishucmd/executor_test.go` 既有全部 rerun 用例一行不改即通过**；
- 显式 variant 模式 **`WorkflowResult` 零调用**，且允许 PASSED / SKIPPED；
- 空 variant 模式忽略空 `Variant` 的 summary，去重并按字典序排序；
- `ArtifactMissing` 携带 `Count`，文案 `变体 %s 的 artifact 数量为 %d，要求恰好 1 个` 逐字复现；
- **artifact 全缺时 `ignore` 仍能成功**（只跑 `ResolveFailureRun`），
  而同一场景下 `retry` 被 `ArtifactMissing` 拒绝——两者不再耦合（v4 阻断 4 回归）。

### 10.6 卡片

- `config.update_multi = true`；
- button `value` 序列化后恰好两个键；含第三个键或含 `variant` 判红；
- 带 `behaviors` / `url` / `multi_url` 判红；`tag=div` 同时带 `actions` 判红；
- 全绿运行不带按钮；readiness 任一项为假 → 不带按钮**且其余内容与 workflow 产出逐字相同**；
- 注入按钮后超预算 → **发送原展示卡片，不降级纯文本**（机械断言 `SendText` 零调用）；
- §7.4 五种状态文本逐字匹配，失败态**只有一个按钮**；
- 同一 workflow 多张已观测卡片 → 全部收敛；
- PATCH 超时 → 不标 `succeeded`，设 `reconcile_after`；
  **`reconcile_after` 未到期时 sweep 选不中该行**（谓词生效的机械证明，v4 阻断 6 回归），
  到期后重渲染一次；
- 新 revision 重排**清空 `reconcile_after`**，新状态立即可更新；
- 失去租约的写方**不清除他人 owner**；
- **revision fencing**：claim 钉住 rev1 → PATCH 期间动作推进到 rev2 → 旧 worker 的
  completion **影响 0 行**，该行仍为 `pending` 且 `desired_revision=2`，随后被 sweep 选中
  并渲染 rev2（v5 第 3 条回归；只比对 owner 的实现会把它标成 `succeeded` 从而永久吞掉 rev2）。

### 10.7 workflow 侧不变性（关键）

- **`buildNotificationCard` 的输出在加入 `Actions` 字段前后序列化逐字节相同**
  （这是"无需 Temporal 版本门"的机械依据）；
- `history-pre-notify-card.json` 继续原样重放通过；
- workflow 代码中不存在对 `CardElement.Actions` 的任何赋值（可用测试断言遍历产出卡片，
  所有元素 `Actions == nil`）。

### 10.8 Migration

- 独立幂等 migration，**连续执行两次结果相同**；
- fresh `schema.sql` 与 upgraded 库最终约束一致（复用 `migration_workflow_runs_test.go` 模式）；
- `pgtest` 的 TRUNCATE 清单补入五张表；
- **前置检查**：断言 `workflow_runs` 已存在，未完成 workflow_runs 生产迁移时明确失败。

---

## 11. 部署顺序（两段，不可合并）

**第一段——并入 `workflow-runs` 的首次部署**：只带 `config.update_multi: true` 与
`CardElement.Actions` 字段声明（workflow 永不设值，序列化不变），以及对应的断言放宽。
卡片仍无按钮，行为不变，但此后发出的卡片**具备被更新的能力**。

**第二段——`workflow-runs` rollout 稳定之后**：迁移五张新表、部署 `cardaction` 与
activity 注入、打开 `FEISHU_CARD_ACTIONS_ENABLED`。因为 workflow 侧无改动，
**第二段不含任何 Temporal 版本门与 fixture 录制**。

**绝不把交互处理混进 workflow_runs 的停写窗口**——那个窗口已同时承担 artifact 唯一键变更
与 analyze_bridge v2 切换，再叠一层新表和新回调路径，故障归因会变得不可能。

---

## 12. 完成定义

- 终态卡片在有失败变体且 readiness 五项全真时带两个按钮，其余情况**与 workflow 产出逐字相同**；
- 一次点击 = 一次 claim = 一行 accepted 审计，与回调到达次数无关；
- ACK 之后进程退出，动作仍由 inbox sweep 完成，不丢失；
- 接受与业务拒绝各是**一个事务**，重跑不产生重复审计；
- inbox 只 acquire 一次：`Complete*` 在租约有效期内必须成功（不再自我阻塞）；
- 可点击卡片必有快照；**快照写不进去就不带按钮发出**；
- `ignore` 不因 artifact 缺失而失败；
- 重试的 attempt / target / **完整输入**在接受事务内一次性写入，恢复逐字段复用，
  永不推进水位、永不重新 Resolve；
- **卡片是 best-effort 最终呈现，数据库动作状态是权威**——不承诺卡片严格收敛（§8.3）；
- 已观测到的 message instance 全部收敛（未被点击的重复卡片不在承诺范围内）；
- webhook 模式的展示卡片未退化（`CardSender` 未被扩展）；
- 未授权点击只得到同步 toast，`SendText` 零调用；
- 文本 `rerun` 外部行为与回复逐字不变；
- `decisions` 表未被修改；**workflow 代码除两个字段声明外未改动**；
- runtime 全测试、store PG conformance、真实 PG 并发测试、`go vet ./...` 全绿；
- CLAUDE.md §3 规则 7 / §11 与 `docs/device-test-sequence.md` 已同步修正。

---

## 13. 后续（不在本轮）

- NL 翻译 `rerun` 的确认/取消改卡片按钮（第三轮，复用本轮的鉴权、inbox、claim 与 callback 基础设施）
- 「隔离」按钮——等设备级信号源落地（差距 #10）
- 通知里带日志/证据链接
- 卡片模板化
