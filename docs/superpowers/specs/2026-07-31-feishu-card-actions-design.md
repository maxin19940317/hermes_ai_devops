# 飞书终态卡片按钮设计（重试 / 忽略）

**日期：** 2026-07-31
**状态：** 待评审
**范围：** 三轮交互能力的第二轮。终态通知卡片上加两个**卡片级**按钮——「重试失败变体」「忽略」。
不含 NL 确认/取消按钮（第三轮）、隔离按钮、证据链接、卡片模板化。

前置轮：
- `2026-07-30-feishu-notification-card-design.md`（展示卡片，§10 是本轮的输入清单）
- `2026-07-30-workflow-runs-design.md`（权威运行记录，解除了 §10(3)(4)）

---

## 1. 门禁裁决与实测结论

本轮开工前跑了一次性 spike（`runtime/cmd/spike-cardaction`），对真实飞书应用验证。**裁决：GO。**
四条实测结论是本设计的事实基础，不再重新论证：

1. **WS 通道成立。** `card.action.trigger` 经长连接送达，事件订阅保持现状即可，**不需要公网回调 URL**。
   实现路径唯一：在现有 `dispatcher.EventDispatcher` 上注册 `OnP2CardActionTrigger`，由
   `WithEventHandler` 交给 `larkws` 客户端。（`ws.WithCardHandler` 在 SDK v3.9.9 是注释掉的，
   `MessageTypeCard` 帧被 `ws/client.go:626` 直接丢弃——那条路走不通，不要再试。）
2. **反馈必须分两层。** 即时层 = toast，**best-effort**（实测存在错误码 200671）；
   权威层 = 卡片延迟更新（按钮 → 状态文本），**不受 3 秒响应预算限制，这层必须做**。
   任何"只发 toast 就算完成"的设计都不可接受。
3. **重复投递是常态。** 实测一次 ignore 点击收到 **3 次**回调。幂等必须进设计，
   按动作幂等键消费，**不以到达次数为准**。
4. **载荷最小化验证通过。** `value` 只放 `action` + `source_workflow_id`，
   完整到达无损；其余一律从权威记录派生。

---

## 2. 语义

### 2.1 重试

**完整复用 `rerun` 的业务语义**，但**不复用其调用路径**——这两件事必须分开承诺。

失败集合的定义与 `rerun` 逐字一致：源 workflow 已关闭，从 Temporal `DeviceTestOutput` 中选
`verdict ∉ {PASSED, SKIPPED}` 的 `TaskSummary`。**不从 tasks 缺行推断 SKIPPED**——
`AcquireDevice` 耗尽、`CreateTask` 活动自身失败都会产出没有 task 行的失败 summary。

重试产生**新的 workflow，并发出它自己的终态卡片**。若仍失败，从新卡片继续重试。
因此原卡片只允许一次 chosen action，不需要"重试计数"或"重置按钮"。

### 2.2 忽略

**只表示人工确认**：某人看过这次失败并决定不处理。

- 不修改 `tasks.verdict`；
- 不触发任何策略；
- **当下没有任何消费方读取它**。

这三条必须写在用户可见文档里。不写清楚，第一个用的人会以为按钮坏了。

权威记录是 `card_actions` + `audit_log`。**不写 `decisions`**——理由见 §3.3。

### 2.3 按钮出现条件

两个条件合取，缺一不出按钮：

1. 该次运行至少有一个 `verdict ∉ {PASSED, SKIPPED}` 的变体（全绿卡片不带按钮）；
2. `FEISHU_CARD_ACTIONS_ENABLED=true` **且** `FEISHU_CMD_WHITELIST` 非空
   （后者非空才会启动 listener，listener 不启动就没有任何进程能接回调）。

这两项都是**装配期可读的静态配置**，不是从"handler 是否注册"推导出来的运行时状态——
§7.1 解释了为什么必须如此。

---

## 3. 数据模型

### 3.1 card_actions

```sql
CREATE TABLE IF NOT EXISTS card_actions (
    workflow_id        TEXT PRIMARY KEY REFERENCES workflow_runs(workflow_id),
    action             TEXT        NOT NULL CHECK (action IN ('retry','ignore')),
    actor_open_id      TEXT        NOT NULL,
    open_message_id    TEXT        NOT NULL,

    -- 动作本体
    state              TEXT        NOT NULL CHECK (state IN ('pending','succeeded','failed')),
    owner              TEXT        NOT NULL DEFAULT '',
    lease_expires_at   TIMESTAMPTZ,
    target_workflow_id TEXT        NOT NULL DEFAULT '',
    attempt            INTEGER     NOT NULL DEFAULT 0,
    last_error         TEXT        NOT NULL DEFAULT '',

    -- 卡片更新:与动作状态完全正交,Patch 结果绝不回写 state
    card_update_state            TEXT NOT NULL DEFAULT 'pending'
        CHECK (card_update_state IN ('pending','succeeded','abandoned')),
    card_update_owner            TEXT        NOT NULL DEFAULT '',
    card_update_lease_expires_at TIMESTAMPTZ,
    card_update_attempts         INTEGER     NOT NULL DEFAULT 0,
    card_update_last_error       TEXT        NOT NULL DEFAULT '',
    card_updated_at              TIMESTAMPTZ,

    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT card_actions_retry_pinned CHECK (
        action <> 'retry'  OR (attempt >  0 AND target_workflow_id <> '')),
    CONSTRAINT card_actions_ignore_unpinned CHECK (
        action <> 'ignore' OR (attempt =  0 AND target_workflow_id =  ''))
);

-- 两个 sweep 各自的部分索引:谓词已限定状态,索引列只需租约到期时间
CREATE INDEX IF NOT EXISTS card_actions_action_sweep_idx
    ON card_actions(lease_expires_at) WHERE state = 'pending';
CREATE INDEX IF NOT EXISTS card_actions_card_sweep_idx
    ON card_actions(card_update_lease_expires_at) WHERE card_update_state = 'pending';
```

**`workflow_id` 做主键**是本设计的支点，它一次性消解了三个问题：

- **互斥**（§10(5)）：一次运行最多一个 claim，`action` 只是列。两人同时点不同按钮，
  由一次 `INSERT ... ON CONFLICT DO NOTHING` 决出胜负，输的一方读到既有行并被告知
  「已由 XXX 重试/忽略」。不需要额外的互斥规则。
- **幂等**（实测结论 3）：3 次重投中只有第一次插入成功，另外两次读到既有行。
  幂等来自主键冲突，不来自到达次数。
- **legacy 隔离**（§10(3) 遗留约束）：`REFERENCES workflow_runs(workflow_id)` 让没有权威
  行的历史运行不可能产生 claim。

**两个 CHECK 是 §10(2) 的机械化。** `retry` 行只要存在就必然已钉死 attempt 与
target_workflow_id；`ignore` 行则必然没有钉任何东西。任何"先插 pending 再补 attempt"的
实现都会在 COMMIT 时撞穿 CHECK——这正是我们要的：**接受动作必须是单事务**。

### 3.2 audit_log

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

### 3.3 decisions 不动

**明确否决**"把 `decisions.task_id` 改成可空"这一方案。它是全局放宽：会允许 `rule` /
`hermes` 的 decision 也写成无归属的 NULL task。而且 `decisions` 表没有 `workflow_id` 列，
run 级归属只能藏进 JSON，FK 与查询接口都无法保证。

卡片级 ignore 的权威记录就是 `card_actions` + `audit_log`。

> 若将来确有消费方需要把 run 级人工决策放进 `decisions`，必须新增
> `workflow_id REFERENCES workflow_runs(workflow_id)` 并加
> `CHECK ((task_id IS NULL) <> (workflow_id IS NULL))`，**不能只放宽 NOT NULL**。

### 3.4 文档同步修正

- **CLAUDE.md:37**（§3 规则 7）"所有 Hermes/人工决策落 `decisions` 表"——增加口子：
  终态卡片确认是 **run 级**动作，记于 `card_actions` + `audit_log`，不属于 task-level decision。
- **CLAUDE.md §11 数据模型**——补 `card_actions`，并把 `audit_log` 标注为已实现。
- **`docs/device-test-sequence.md`**——写明终态卡片确认**不投 workflow signal、不走 outbox**。

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

**硬约束：`Resolver` 必须支持 optional variant**，使文本 `rerun` 的显式单变体语义**和回复文案
逐字不变**。本轮不改文本指令的任何外部行为——`executor_test.go` 里既有的 rerun 用例
一行不改即通过，是本轮的验收项之一。

`Resolve` 含 Temporal 调用（`WorkflowClosed` / `WorkflowResult`），**必须在数据库事务之外执行**。

拒绝原因是封闭枚举，供两个调用方各自渲染文案：`NotAuthoritative` / `StillRunning` /
`ResultUnreadable` / `NoFailedVariants` / `VariantNotMember` / `ArtifactMissing`。

---

## 5. 接受动作：单事务

### 5.1 边界

```text
[事务外·只读] 传输层去重 → 载荷校验 → 身份/白名单 → GetWorkflowRun → Resolve
                                                    (显式先查,不把 FK 违反当控制流)
BEGIN
  INSERT card_actions(...) ON CONFLICT (workflow_id) DO NOTHING
      retry  → state='pending'   (同事务内钉死 attempt/target,见下)
      ignore → state='succeeded' (无外部副作用)

  ├ 插入成功:
  │   retry : 锁 artifacts 推进水位得 N → Go 侧算 target_workflow_id → 同事务钉死
  │   INSERT audit_log(..., card_action_workflow_id = workflow_id)
  │
  └ 影响 0 行(已有行) → 读出既有行,按 §5.3 判定是"重投/被占"还是"失败后重试"
COMMIT

[事务外·幂等] retry: StartDeviceTest(钉死的 ID) → finalize succeeded / failed
[事务外·幂等] 两者: Patch 卡片 → card_update_state
```

水位分配要能组合进 claim 事务，因此 `NextWorkflowAttemptAll` 需抽出 tx 作用域的内部函数
（当前实现自己 `BeginTx`，见 `postgres_fleet.go:95`），独立方法与 claim 事务共用同一份实现。

### 5.2 三条 fencing 规则

全部落在 store API 上，不靠调用方自律：

1. **finalize 必须带 fencing**：
   `UPDATE ... WHERE workflow_id=? AND state='pending' AND owner=?`。
   租约过期被接管后，原 owner 迟到的 finalize 影响 0 行——这正是需要的。
2. **恢复与 `failed → pending` 一律复用原 `attempt` / `target_workflow_id`，绝不推进水位。**
   水位推进只发生在接受事务里，一次 claim 一次；此后该行的 attempt 是只读的。
3. **租约只管活性，不管正确性。** 正确性来自"钉死 + CAS"：重复执行只会用同一个 workflow ID
   撞 Temporal `RejectDuplicate`。因此租约可以宽松——取 **120 秒**（与 §10 设备租约同量级），
   过期接管不会造成重复测试。

`ignore` 不进这个状态机：接受事务里直接 `succeeded`，只留卡片更新子状态机。

### 5.3 已有行的三种归宿

`ON CONFLICT DO NOTHING` 影响 0 行时，读出既有行，按 `(既有 action, 既有 state)` 判定：

| 既有行 | 本次点击 | 归宿 |
|---|---|---|
| 任意 action，`pending` / `succeeded` | 相同或不同 action | **被占**：不改任何状态，写 `rejected.conflict` 审计 |
| `action` 相同，`state='failed'` | 相同 action | **失败后重试**：CAS `state='failed' → 'pending'`，取新 owner 与租约，**复用原 `attempt` / `target_workflow_id`**，不写新的 accepted 审计 |
| `action` 不同，`state='failed'` | 另一个 action | **被占**：写 `rejected.conflict` 审计 |

**动作在首次接受时即固定，此后不可改换。** retry 失败后只能重试 retry，不能改成 ignore——
否则 `attempt` / `target_workflow_id` 就得被清空，与"钉死后只读"和两个 CHECK 直接冲突。
想改主意的人可以什么都不点：ignore 的全部效果只是留一条记录。

失败后重试**不写新的 accepted 审计行**（`card_action_workflow_id` 有 UNIQUE 约束，
一个 accepted action 恰好一条审计）。重试次数体现在 `card_actions.last_error` 被覆盖，
以及卡片状态文本的变化上。

---

## 6. 回调处理

### 6.1 同步段与异步段的切分

飞书的响应预算约 3 秒，超时即重投（实测 3 次）。而 `Resolve` 含两次 Temporal 调用与多次
数据库查询，**不能放进同步段**。切分依据就是实测结论 2 的两层反馈：

**同步段**（毫秒级，最多一次单行 INSERT，返回 toast）：

1. 传输层 event ID 去重（进程内缓存，复用 `listener.go` 的 `dedupCache` 模式）；
   **命中即静默返回，不写任何审计**——否则 3 次重投会写出 3 行拒绝审计。
2. 载荷大小上限（4 KiB）与形态：`action ∈ {retry, ignore}`、`source_workflow_id` 非空、
   无多余键。
3. 身份：`operator.open_id` 非空。
4. 白名单：复用 `FEISHU_CMD_WHITELIST`（同一批人有权发指令，就有权点按钮；
   再引入第二份名单只会漂移）。
5. 2–4 任一不过 → 写一行拒绝审计（单行 INSERT，带 2 秒 context 超时；超时则只记日志，
   审计尽力而为不阻塞应答）+ 返回精确 toast。
6. 全过 → 交给异步段，返回 toast「已收到，正在处理」。

**异步段**（无时限，结果经卡片 Patch 呈现）：`GetWorkflowRun` → `Resolve` → 接受事务 →
执行 → Patch。异步段用 listener 的生命周期 ctx，**不用回调 ctx**——后者在应答返回时即取消
（`listener.go` 已有同样的处理）。

> 这个切分正是两层反馈的直接后果：toast 是**临时回执**，卡片才是**权威结论**。
> 任何需要 I/O 才能判定的结果（legacy、无失败变体、已被占、启动失败）都由卡片承载，
> **不由 toast 承载**——异步段跑完时同步应答早已返回，那时没有 toast 可发。
> 已被占的情形异步段照常写 `rejected.conflict` 审计，卡片则显示胜出者的状态文本。

### 6.2 fail-closed 清单（§10(8)）

全部在 claim 之前完成：空身份、未知 action、超量 payload、非白名单、
无权威 run、源 workflow 未关闭、Temporal 结果不可读、无失败变体、artifact 缺失。
**任一不满足即不 claim、不分配 attempt、不启动 workflow。**

非白名单提示必须走**同步 callback toast**，绝不能用 `SendText`——后者会发到固定的
`FEISHU_RECEIVE_ID`（可能是群），等于把一次未授权点击广播给所有人。

### 6.3 审计口径

审计对象是**人的点击尝试与 accepted action**，不是内部执行状态迁移。

| 场景 | action | FK |
|---|---|---|
| 接受（claim 事务内，恰好一行） | `card.retry.accepted` / `card.ignore.accepted` | 设置 |
| 非白名单 | `card.<action>.rejected.unauthorized` | NULL |
| 无权威 run | `card.<action>.rejected.legacy` | NULL |
| 已被占 | `card.<action>.rejected.conflict` | NULL |
| 载荷/身份/枚举不合法 | `card.unknown.rejected.payload` | NULL |
| readiness 关闭时点历史按钮 | `card.<action>.rejected.disabled` | NULL |
| 源未关闭 / 结果不可读 / 无失败变体 / 缺 artifact | `card.<action>.rejected.<reason>` | NULL |
| 失败后重试（CAS `failed → pending`） | 不写审计（accepted 已有唯一行） | — |

- **恢复、Start、succeeded/failed、Patch 一律不追加 human audit**；这些状态由 `card_actions` 承载。
- `actor` 固定格式 `feishu:<open_id>`；空身份用固定值 `feishu:unknown`。
- `target` = `source_workflow_id`（无法解析时用空串）。
- `payload_digest` = 对**经过大小限制后的**原始 action payload 做 canonical JSON 的
  SHA-256。**不保存不可信原文。**
- 传输层去重跑在拒绝审计**之前**；不同的真实点击仍各留一行。

---

## 7. 卡片形态

### 7.1 readiness 门禁（§10(7)）

按钮可用性是**运行时配置**，不能进 activity 载荷——activity 输入由 workflow 写进 history，
worker 装配时改不了。

- 显式开关 `FEISHU_CARD_ACTIONS_ENABLED`（缺省 `false`）。**本地注册 handler 不能证明飞书
  后台已订阅 `card.action.trigger`**，所以必须是独立开关，不能由"handler 是否注册"推导。
- 判定发生在 `NotifyCard` 活动侧发送前：不满足就**剥掉按钮**发展示卡片，其余内容不变。
- 开关关闭时，**历史卡片上仍然存在的按钮**（配置改前发出的）点击后返回 toast
  「按钮已停用」并写 `rejected.disabled` 审计，不进入 claim。这是关掉交互能力的实际含义——
  WS 断开时回调根本到不了本进程，无需也无法处理。

### 7.2 卡片 DTO 变更

两处改动，都是对已上线展示卡片的**有意契约变更**：

1. `CardConfig` 增加 `UpdateMulti bool \`json:"update_multi"\``，**恒为 `true`**。
   飞书 `PATCH /open-apis/im/v1/messages/:message_id` 要求原卡片带
   `config.update_multi = true`，否则不可更新。
2. 新增封闭的 action 模块：

```go
// CardActionModule 是唯一允许出现的交互元素。它没有 behaviors、没有 url、
// 没有 multi_url——按钮只能回调,不可能变成跳转或表单。
type CardActionModule struct {
    Tag     string       `json:"tag"`     // 恒为 "action"
    Actions []CardButton `json:"actions"`
}

type CardButton struct {
    Tag   string            `json:"tag"`   // 恒为 "button"
    Text  CardText          `json:"text"`  // plain_text
    Type  string            `json:"type"`  // primary | default
    Value map[string]string `json:"value"` // 恰好 {action, source_workflow_id}
}
```

**同步影响**：展示卡片轮的递归键断言（`devicetest_test.go:1007`）把 `config` 的键集合精确
锁死为 `{"wide_screen_mode"}`，且反例用例把带 `actions` 的卡片判为非法。这两处**必须同步
放宽到新的封闭集合**，而不是删除断言——反例改为"带 `behaviors`""带 `url`""按钮 value 含
第三个键""value 含 variant"仍须判红。

**30 KiB 门禁不变**，但 action 模块与主行同级**不可裁剪**：现有 trim 只丢 reason/hermes 行，
需确保裁剪到底仍保留按钮；若含按钮后仍超预算，按既有规则整卡降级为纯文本（此时自然无按钮）。

### 7.3 部署顺序（重要）

卡片由 **workflow 内**的 `buildNotificationCard` 构造，输出直接进 `NotifyCard` 活动入参、
写入 history。改 DTO 对**已含 NotifyCard 的 history** 有重放风险（本仓库在
`replay_test.go:28-31` 记过一次活动入参不一致导致 `TMPRL1100` 的教训）。

关键事实：**当前生产上不存在这样的 history**——线上是 `origin/master`，notify-card 仍在未合入的
`workflow-runs` 分支；`history-pre-notify-card.json` 走 DefaultVersion 文本分支，根本不调
`buildNotificationCard`。

因此固定如下顺序，**二选一，不可含糊**：

- **首选**：把 `update_multi` 的 DTO 改动并入 `workflow-runs` 分支，**赶在它首次部署之前**。
  零重放风险、零版本门。
- **备选**（`workflow-runs` 已先上线）：为 DTO 改动单开 `workflow.GetVersion` 门
  （change ID `card-actions-v1`），并补录一份含 NotifyCard 的 history fixture。

### 7.4 动作后的卡片

按钮整块被替换为一行状态文本（`tag=div`, `plain_text`），其余内容不变：

| 状态 | 文本 |
|---|---|
| retry succeeded | `已由 <open_id> 重试 → <target_workflow_id>` |
| retry pending | `已由 <open_id> 重试，正在启动…` |
| retry failed | `重试启动失败：<last_error>（可重新点击）` |
| ignore succeeded | `已由 <open_id> 忽略（仅记录，不改变判定）` |

`retry failed` 是唯一保留按钮的终态——它对应 §5.2 的 `failed → pending` CAS 重试路径。
括号里那句"不改变判定"是 §2.2 语义的用户可见落点，不可省略。

---

## 8. 错误处理与恢复

### 8.1 两个 sweep

worker 启动时各跑一次，之后按固定间隔（30 秒）轮询：

| sweep | 选取条件 | 动作 |
|---|---|---|
| 动作 | `state='pending' AND lease_expires_at < now()` | CAS 取得 owner + 新租约 → 用**钉死的** target 重新 `StartDeviceTest` → finalize |
| 卡片 | `card_update_state='pending' AND card_update_lease_expires_at < now()` | CAS 取得 owner + 新租约 → Patch → succeeded / abandoned / 退避重试 |

飞书长连接在多个 worker 实例间负载均衡，点击可能落到任一实例；owner + 租约就是为了让
A 实例崩溃后 B 实例能接管。**单实例部署下这套机制同样必要**——进程重启就是一次接管。

### 8.2 错误分类

| 来源 | 永久（不重试） | 暂时（退避重试） |
|---|---|---|
| `StartDeviceTest` | artifact 缺失、非法输入 | Temporal 不可达、超时 |
| 卡片 Patch | 消息超过飞书 14 天更新期限、message 不存在、权限不足 | 网络错误、限流、5xx |

永久错误：动作侧 → `state='failed'` + `last_error`（卡片显示可重新点击）；
卡片侧 → `card_update_state='abandoned'` + `card_update_last_error`。

**卡片 Patch 的任何结果都不回写动作 `state`。** 卡片没更新成功不代表重试没发生——
把两者耦合会让一次成功的重试因为一次 Patch 失败而显示为失败。

`card_update_attempts` 累加，退避上限后转 `abandoned`。toast 的 200671 属于即时层，
**不影响任何状态**，只记日志。

---

## 9. 代码边界

| 位置 | 职责 |
|---|---|
| `runtime/internal/rerun`（新） | 只读 `Resolver`：权威校验、失败筛选、artifact 解析。被 `feishucmd` 与 `cardaction` 共用 |
| `runtime/internal/cardaction`（新） | 回调 handler、接受事务的调用方、两个 sweep、卡片 Patch |
| `runtime/internal/store` | `card_actions` / `audit_log` 访问层；接受事务是**一个** store 方法，不暴露事务对象给上层 |
| `runtime/internal/feishucmd/listener.go` | 增注册 `OnP2CardActionTrigger`，转调 `cardaction`；本身不含业务逻辑 |
| `runtime/internal/feishu` | `CardSender` 增 `PatchCard(ctx, messageID string, card any) error` |
| `runtime/internal/workflow/devicetest.go` | 卡片 DTO 增 `update_multi` 与 action 模块 |

接受事务作为**单个 store 方法**暴露（如 `AcceptCardAction`），是为了让 §5.2 的三条 fencing
规则无法被调用方绕过——事务边界不外泄，就不存在"调用方忘了带 owner"这种失败模式。

---

## 10. 测试与验收

### 10.1 Store conformance（MemStore 与 PGStore 共用）

- 接受事务：首次成功；并发第二次影响 0 行并读回既有行；`retry` 行落库即已钉死；
- `retry` 插入时不钉 attempt/target → CHECK 违反（证明"单事务"不可绕过）；
- `ignore` 行 attempt=0 且 target 为空，且落库即 `succeeded`；
- 一次接受恰好一行 accepted 审计；重复接受不追加；
- 拒绝审计可多行且 FK 为 NULL；
- finalize fencing：owner 不匹配影响 0 行；
- 恢复复用原 attempt/target，**水位不再推进**（断言 artifacts 计数器不变）；
- `failed → pending` CAS：同 action 成功且复用原 attempt/target；
  异 action 被拒；`pending` / `succeeded` 行任何 action 都被拒；
- 失败后重试不产生第二行 accepted 审计（`card_action_workflow_id` UNIQUE 的机械证明）；
- 卡片子状态机与动作 state 相互不影响；
- `workflow_id` 不在 `workflow_runs` 中 → FK 拒绝。

### 10.2 回调处理

- 同一 event ID 重投 3 次 → 一次 claim、**一行审计**、三次同样的 toast；
- 不同真实点击各留一行审计；
- 非白名单 → 同步 toast + `rejected.unauthorized` 审计，且 `SendText` **零调用**
  （机械断言，防止未授权点击被广播到群）；
- 超量 payload / 未知 action / 空身份 → 拒绝且不进异步段；
- 两人同时点 retry 与 ignore → 恰好一个 accepted，另一个 `rejected.conflict`；
- 无失败变体 / legacy run / 源仍在运行 → 拒绝，`NextWorkflowAttempt*` 与 `StartDeviceTest`
  均**零调用**。

### 10.3 卡片

- 卡片含 `config.update_multi = true`；
- 按钮 `value` 恰好两个键，含第三个键或含 `variant` 的用例判红；
- 带 `behaviors` / `url` / `multi_url` 的用例判红；
- 全绿运行不带按钮；readiness 关闭时不带按钮且其余内容逐字不变；
- 含按钮后仍受 30 KiB 门禁；裁剪到底仍保留按钮；
- 四种动作后状态文本逐字匹配（§7.4 表格是契约）。

### 10.4 文本 rerun 不回归

`feishucmd/executor_test.go` 中既有的全部 rerun 用例**一行不改即通过**。这是
`RerunResolver` 抽取正确的判据——语义共用而非复制。

### 10.5 重放

- 若走 §7.3 首选路径：`history-pre-notify-card.json` 继续原样重放通过（它不含 NotifyCard，
  不受 DTO 影响）；
- 若走备选路径：另补一份含 NotifyCard 的 fixture，改动前录制、改动后重放通过。

---

## 11. 完成定义

- 终态卡片在有失败变体且 readiness 开启时带两个按钮，其余情况逐字保持展示卡片形态；
- 一次点击 = 一次 claim = 一行 accepted 审计，与回调到达次数无关；
- 重试的 attempt 与 workflow ID 在接受事务内钉死，恢复路径永不推进水位；
- 卡片最终反映动作结果，且 Patch 失败不污染动作状态；
- 未授权点击只得到同步 toast，`SendText` 零调用；
- 文本 `rerun` 外部行为与回复逐字不变；
- `decisions` 表未被修改；
- runtime 全测试、store PG conformance、`go vet ./...` 全绿；
- CLAUDE.md §3 规则 7 / §11 与 `docs/device-test-sequence.md` 已同步修正。

---

## 12. 后续（不在本轮）

- NL 翻译 `rerun` 的确认/取消改卡片按钮（第三轮，复用本轮的鉴权、claim 与 callback 基础设施）
- 「隔离」按钮——等设备级信号源落地（差距 #10）
- 通知里带日志/证据链接
- 卡片模板化
