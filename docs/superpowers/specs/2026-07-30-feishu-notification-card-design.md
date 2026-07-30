# 飞书终态通知卡片化设计

日期：2026-07-30

状态：待批准（v2，2026-07-30 评审后重写：去重机制、双模发送、Activity 兼容、可操作 verdict 集合、审计）

## 1. 背景与一处必须先更正的架构描述

终态通知今天是纯文本：`buildNotification`（`workflow/devicetest.go:666`）拼字符串，
`Notify` 活动经 `feishu.Sender.SendText` 发出。

**CLAUDE.md 对这一项的描述是错的，先更正再设计。** §4 写"交互卡片(按钮回调 → Runtime
signal)"，§12 Phase 2 写"重试/忽略/隔离按钮 → Runtime signal"。这个形态不存在：

- `Notify` 是 workflow 做的**最后一件事**（`devicetest.go:286-289`），紧接着 `return`。
- 全仓库只有一个 signal（`SignalTaskResult`，`devicetest.go:19/280`），无人工决策挂起点。

终态通知上的按钮**没有 workflow 可以发信号给它**。按钮只能映射到事后仍成立的动作。

## 2. 关键决策

1. **本轮只做终态通知卡片化**（已确认）。NL 翻译的 `rerun` 待确认仍用 `y`/`n` 文本。
2. **按钮只放「重试失败变体」与「忽略」**（已确认）。不做「隔离」：新能力，且当下没有
   "设备真坏了"的信号源来验证它有用（差距 #10 §7）。
3. **幂等靠持久化 claim，不靠事件 ID 缓存**（v2 修正，见 §6）。
4. **按钮只在 app 凭据 + 白名单 + WS listener 全部就绪时出现**（v2 修正，见 §5）。
5. **新增独立的 `NotifyCard` 活动，不改 `Notify` 的签名**（v2 修正，见 §9）。
6. **可操作 verdict 集合显式排除 PASSED 与 SKIPPED**（v2 修正，见 §4.2）。
7. **回调从库里回读权威终态，不信任按钮携带的业务字段**（v2 修正，见 §7）。
8. **重试必须继承源 workflow 的 `Version` 与 `RuleVersion`**（v2 修正，见 §7.1）。

不采用的方案：

- 让 workflow 在 `Notify` 后挂起等按钮 signal：要把生命周期从"跑完即止"改成"等人处理"，
  牵动租约、超时与 `HARD_TIMEOUT_MARGIN_SEC` 全部语义。
- 按钮走新 HTTP 端点：要再开一个入站面并单独鉴权；现有 WS 长连接已有白名单与异步机制。
- 只靠事件 ID 去重：见 §6，它只能挡传输重投，挡不住真实双击。

## 3. 范围

交付：

- `feishu.Sender` 增加 `SendCard` 与 `UpdateCard`（webhook 与 app 两种实现，能力差异见 §5）
- `buildNotificationCard`：workflow 侧确定性生成卡片
- **新增** `NotifyCard` 活动（`Notify` 原样保留）
- 卡片按钮回调：接入既有 WS listener，白名单 + **持久化 claim** + 回读权威状态 + 执行 + 更新卡片
- **新增 `card_actions` 表**（claim + 每变体状态）与 **`audit_log` 表**（见 §8）
- 修复 `rerun` 指令丢失 `Version`/`RuleVersion` 的既有缺陷（见 §7.1）
- 文档：CLAUDE.md §4/§12 更正、`deploy/README.md` 通知一节

不交付（明确排除）：

- NL 翻译 `rerun` 待确认改按钮
- 「隔离」按钮
- 通知里带日志/证据链接（需对外可达的 MinIO 地址与签名读 URL）
- `audit_log` 的其他写入方（本轮只覆盖卡片动作；全面铺开另案）

## 4. 卡片形态

一张卡片对应一次 workflow（含多个变体），不是一变体一张。

### 4.1 header 颜色

| 条件 | 颜色 |
|---|---|
| 无可操作失败（全 PASSED，或只有 SKIPPED） | 绿 |
| 存在非 INFRA 的可操作失败 | 红 |
| 可操作失败**全部**是 `INFRA_ERROR` | 橙 |

v2 修正：原设计写"含 INFRA 即橙"。那会把一张同时有 CODE 失败的卡片显示成"环境问题"，
误导排查方向。业务失败优先——只有当所有可操作失败都是基础设施类时才是橙。

### 4.2 可操作 verdict 集合

按钮只作用于**可操作失败**，定义为 `verdict ∉ {PASSED, SKIPPED}`。

排除 `SKIPPED` 是硬性的，不是偏好：`devicetest.go:273-278` 给被跳过的变体构造
`TaskSummary` 时**只填 `TestID`/`Variant`/`Verdict`/`Reason`，没有 `TaskID`**——因为
根本没创建 task。而 `decisions.task_id` 是 `NOT NULL REFERENCES tasks(task_id)`
（`schema.sql:102`），对 SKIPPED 变体点「忽略」会直接违反外键。重试它也无意义：
它被跳过的原因是 fleet 里没有匹配设备，重跑还是没有。

卡片仍**展示** SKIPPED 变体（运维需要知道哪些没测），只是不纳入按钮的作用范围。

### 4.3 布局

```text
┌─────────────────────────────────────────────┐
│ [绿/红/橙] algo-super-sdk g9da3b9d9 p56     │
├─────────────────────────────────────────────┤
│ aarch64_Android_SNPE_2.21  PASSED           │
│   38/38 cases · 412.3s · attempt 1          │
│                                             │
│ aarch64_Android_SNPE_1.68  TEST_FAILED(CODE)│
│   35/38 cases · 380.1s · attempt 2          │
│   hermes: 三个用例在 DSP 初始化处崩溃        │
│                                             │
│ aarch64_Linux_RKNN_2.3.2   SKIPPED          │
│   fleet 无匹配设备                           │
├─────────────────────────────────────────────┤
│ [ 重试失败变体 ]  [ 忽略 ]                   │  仅当存在可操作失败 **且** 交互就绪
└─────────────────────────────────────────────┘
```

字段与现有纯文本携带的完全一致，不新增数据来源。附件 key 仍不进通知（§12.6 现状）。

## 5. 双模发送与"交互就绪"

`feishu.NewSender`（`feishu.go:53`）在 AppID+AppSecret+ReceiveID 齐全时选 app 模式，
否则退 webhook。而 WS listener 另有条件：`FEISHU_CMD_WHITELIST` 非空**且** AppID/AppSecret
齐全（`cmd/worker/main.go:136` 附近）。

于是存在一个真实组合：**只配了 webhook**。此时卡片能发（自定义机器人支持
`msg_type: interactive`），但**没有 WS listener 接收按钮回调**，按钮按下去毫无反应。
飞书官方也把自定义 webhook 机器人与应用机器人区分开，卡片回调需要应用订阅
`card.action.trigger`。

**因此按钮的出现条件是"交互就绪"三者合取：**

1. app 模式（AppID + AppSecret + ReceiveID 齐全）
2. `FEISHU_CMD_WHITELIST` 非空
3. 卡片回调事件已注册（本轮实现，与 listener 同生命周期）

不就绪时：**发不带按钮的卡片**（排版与颜色照旧有价值），并在启动日志打印
`feishu card buttons=disabled (原因)`——与既有 `feishu cmd listener=disabled (…)` 同风格。
`SendCard` 本身失败则降级纯文本（§8）。

工程上的落法：`NotifyCard` 活动的载荷带一个 `interactive bool`，由 worker 装配时按上述
三条合取算出；workflow 侧不感知配置。

## 6. 幂等：持久化 claim，而非事件 ID 缓存

**v2 修正的核心。** 原设计说"按事件 ID 去重"，那是错的：

- 两次**真实点击**产生两个不同的事件 ID，缓存拦不住
- `listener.go:87` 的 `dedupCache` 是进程内 10 分钟缓存，worker 重启即失效
- 多人同时点同一张卡片同样是不同事件

事件 ID 缓存只能解决**传输重投**，仍然保留，但它不是幂等的依据。

真正的幂等键是业务身份，落库原子 claim：

```sql
CREATE TABLE IF NOT EXISTS card_actions (
    action_key   TEXT PRIMARY KEY,   -- {workflow_id}:{action}:{variant}
    workflow_id  TEXT        NOT NULL,
    action       TEXT        NOT NULL,   -- retry | ignore
    variant      TEXT        NOT NULL,
    task_id      TEXT        NOT NULL DEFAULT '',
    state        TEXT        NOT NULL,   -- pending | succeeded | failed
    actor_open_id TEXT       NOT NULL,
    detail       TEXT        NOT NULL DEFAULT '',   -- 失败原因
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

claim 语义：`INSERT ... ON CONFLICT (action_key) DO NOTHING` + 检查影响行数。

- 抢到（插入成功，state=pending）→ 执行，成功后 `state=succeeded`，失败 `state=failed` 并记 `detail`
- 没抢到 → 读现有行：
  - `pending`/`succeeded` → 不重复执行，回执告知"已在处理/已处理"
  - `failed` → **允许重试这个动作**（把 state 改回 pending 并执行；失败的动作本身该可重试）

**per-variant 粒度是必需的**：一次「重试失败变体」可能覆盖 3 个变体，若其中 1 个起
workflow 失败，再次点击必须只重试那一个，而不是把已经成功启动的 2 个再起一遍。
按变体 claim 天然给出这个语义。

`action_key` 用 `workflow_id` 而非事件 ID，因此**跨 worker 重启依然有效**，也天然
覆盖多人同时点击。

## 7. 回调路径

```text
用户点按钮
  → 飞书 WS 事件(card.action.trigger) → feishucmd.Listener
  → ① 白名单校验(open_id ∉ Whitelist → 只回提示,不执行)
  → ② 事件 ID 去重(仅挡传输重投)
  → 异步执行(回调立即 ack)
      对每个目标变体:
      → ③ card_actions claim(§6)
      → ④ 回读权威状态(见下)
      → ⑤ 执行 retry / ignore
      → ⑥ 写 audit_log(§8)
  → 更新卡片(按钮置灰 + "已由 @某人 重试/忽略",逐变体结果)
```

**① 白名单**：与文本指令同一份 `Whitelist`。卡片可能发到群里，群成员都看得到按钮；
不校验 `open_id` 等于把 `rerun` 权限开给整个群。这是本设计唯一的鉴权。

**④ 回读权威状态（v2 修正）**：按钮 `value` 只携带**定位信息**
（`workflow_id`、`project`、`commit`、`pipeline_iid`、`variants`），**不携带业务字段**。
verdict/category 由回调按 `task_id` 从 `tasks` 表回读，并校验该 task 的 `workflow_id`
与载荷一致——否则一个伪造的载荷可以让「忽略」写进任意任务的 `decisions`。

按钮载荷：

```json
{
  "action": "retry",
  "workflow_id": "device-test-algo-super-sdk-g9da3b9d9-p56",
  "project": "algo-super-sdk",
  "commit": "9da3b9d9",
  "pipeline_iid": 56,
  "variants": ["aarch64_Android_SNPE_1.68"]
}
```

`task_id` 由 `{workflow_id}:{variant}:a{attempt}` 规则从库中定位该变体最新一次 attempt
（复用差距 #10 那轮为 `RecentRuns` 建立的 `wf.BaseWorkflowID` + `test_id` 过滤语义）。

### 7.1 retry 必须继承 Version 与 RuleVersion

`DeviceTestInput`（`types.go:22`）有 `Version` 与 `RuleVersion` 两个字段。而现有
`rerun` 指令重建输入时**只填 Project/Commit/PipelineID**（`executor.go:339`），两者留空
→ `RuleVersion` 落到缺省 `verdict-rules-v1`。

后果：对同一产物的重跑可能用**不同的规则版本**重新判定，而原则 2/差距 #7 引入
`rule_version` 的全部目的就是让判定可回放。这是既有缺陷，卡片重试会原样继承。

本轮一并修：从源 workflow 的 `tasks`/`artifacts` 记录回读并填入。若源记录里没有
（历史行），显式使用缺省值并在 `audit_log.detail` 里注明"rule_version 缺失，用缺省"，
不静默。

`Version` 同理（它进通知文案，缺失只影响可读性，但没有理由丢）。

## 8. 审计与 ignore 的语义

CLAUDE.md §37 要求"所有操作落 `audit_log`"，§11 给了表结构
`audit_log(actor, action, target, payload_digest, ts)`。**但这张表在 `schema.sql` 里
不存在，全仓库无写入方。** 本轮建表并成为第一个写入方——人工触发的重跑正是审计日志
存在的理由，把本轮新增的能力留在审计之外是说不过去的。

```sql
CREATE TABLE IF NOT EXISTS audit_log (
    id             BIGSERIAL PRIMARY KEY,
    actor          TEXT        NOT NULL,   -- open_id / system
    action         TEXT        NOT NULL,   -- card.retry | card.ignore
    target         TEXT        NOT NULL,   -- workflow_id 或 task_id
    payload_digest TEXT        NOT NULL DEFAULT '',
    detail         TEXT        NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS audit_log_target_idx ON audit_log(target, created_at DESC);
```

**retry 与 ignore 都写 `audit_log`**（v2 修正：原设计只为 ignore 写 `decisions`，
retry 完全没有留痕）。

**ignore 额外写一行 `decisions`，`actor=human`**。§11 定义了这个取值，而至今无人写入过。

**ignore 的语义必须写清，否则会被误解为"改判"**：

- 它只记录"人看过并决定忽略"，**不修改** `tasks.verdict`、不修改 `error_category`、
  不影响任何后续策略
- 当下**没有任何消费方读取 human decision**。它的价值是留痕与将来可回放，不是当下生效
- 因此点了「忽略」之后，同一次失败在 `status`/`devices` 等视图里看起来毫无变化——这是
  预期行为。不写明的话，第一个用它的人会以为按钮坏了

## 9. Temporal 兼容：新增活动而非改签名

`Notify` 今天的签名是 `func (a *Acts) Notify(ctx context.Context, text string) error`
（`notify.go:11`）。**不能改成结构体参数**：在途/正在重试的活动，其记录的输入是字符串，
worker 用新签名去解码会在进入 handler 之前就失败。

因此：

1. `Notify(ctx, string)` **原样保留**，一个字节不改
2. **新增** `NotifyCard(ctx, NotifyCardRequest) error`，独立活动名
3. workflow 侧 `workflow.GetVersion(ctx, "notify-card", workflow.DefaultVersion, 1)` 分支：
   旧版本调 `Notify(text)`，新版本调 `NotifyCard(card)`

`buildNotification` 因此**保留**，不是死代码——旧分支仍在用。

## 10. 错误处理

| 情形 | 行为 |
|---|---|
| `SendCard` 失败 | 记日志 → 降级 `SendText` 发原纯文本，内容与改动前逐字节相同 |
| 两种 Sender 都未配置 | 与今天一致：静默跳过通知 |
| 交互未就绪（§5） | 发不带按钮的卡片；启动日志说明原因 |
| 点击者非白名单 | 回"无权限"，不执行，记 info 日志 |
| 事件重投 | 事件 ID 去重命中，直接返回 |
| 重复点击 / 多人同时点 | `card_actions` claim 未抢到 → 不重复执行，回执告知状态（§6） |
| 上次 `failed` 的动作再次点击 | 允许重试（state 回 pending 再执行） |
| 目标变体是 SKIPPED | 不在按钮作用范围内（§4.2），不会到达这里 |
| `retry` 查无 artifacts | 回执明确报错，claim 置 `failed` 并记 `detail` |
| `retry` 部分变体失败 | 成功的置 `succeeded`，失败的置 `failed`；回执逐变体列出 |
| `ignore` 写库失败 | claim 置 `failed`，回执报错 |
| 卡片更新失败 | 记日志；动作已执行的事实不回滚 |

## 11. 测试

**卡片构造（workflow 包，纯函数，表驱动）**：
- 全 PASSED → 绿、无按钮
- 全 SKIPPED → 绿、无按钮（SKIPPED 不是可操作失败）
- 混合 PASSED + SKIPPED → 绿、无按钮
- 只有 INFRA_ERROR 失败 → 橙
- **INFRA_ERROR + CODE 混合 → 红**（v2 修正的判据）
- 按钮 `variants` 只含可操作失败，**不含 SKIPPED**
- 按钮载荷**不含** verdict/category（权威状态回读，§7）
- `interactive=false` → 无按钮
- Analyzer 结论存在/不存在

**store（双实现 + conformance）**：
- `card_actions` claim：首次抢到、二次抢不到、`failed` 可再抢
- per-variant 独立：同 workflow 不同变体互不影响
- `audit_log` 写入与按 target 查询
- `decisions` 写入 `actor=human`

**回调（fake store + fake starter）**：
- 白名单内 retry → 只对可操作失败变体起 workflow
- **非白名单 → fake starter 零调用**
- **同一变体连点两次 → fake starter 只被调一次**（这是 v2 的核心断言）
- 3 变体中 1 个失败 → 再次点击只重试那 1 个
- ignore → `decisions(actor=human)` + `audit_log` 各一行，且 `tasks.verdict` **未被修改**
- 伪造载荷（task 的 workflow_id 与载荷不符）→ 拒绝

**Notify 兼容**：`Notify(ctx,string)` 签名与行为逐字未变；旧版本分支仍调它。

**Sender**：`SendCard` 失败 → 调用方降级 `SendText`，文本与改动前逐字节相同。

## 12. 验收标准

- 终态通知是卡片；颜色按 §4.1（INFRA+CODE 混合是红，不是橙）
- 无可操作失败时无按钮；SKIPPED 不进按钮作用范围
- 同一变体重复点击只执行一次，**且跨 worker 重启仍然如此**（有测试）
- 部分失败后再次点击只重试失败的那部分（有测试）
- 非白名单点击不执行任何动作（有测试）
- 只配 webhook 时发无按钮卡片，启动日志说明原因
- retry 继承源 workflow 的 `Version` 与 `RuleVersion`（有测试）
- retry 与 ignore 都在 `audit_log` 留痕；ignore 另在 `decisions` 留 `actor=human`
- ignore 不修改 `tasks.verdict`（有测试）
- `Notify(ctx,string)` 未改签名；在途 workflow 重放不失败
- CLAUDE.md §4/§12 的 signal 描述已更正

## 13. 后续（不在本轮）

- NL 翻译 `rerun` 待确认改卡片按钮
- 「隔离」按钮——等设备级信号源落地（差距 #10 §7）
- 通知里带日志/证据链接
- `audit_log` 铺开到其他操作（本轮只覆盖卡片动作）
- human decision 的消费方（当下只留痕，无人读取）
