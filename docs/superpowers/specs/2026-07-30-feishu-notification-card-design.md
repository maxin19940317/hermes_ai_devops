# 飞书终态通知卡片化设计（展示部分）

日期：2026-07-30

状态：待批准（v3，2026-07-30 第二轮评审后**拆分**：本轮只做展示卡片，按钮独立一轮，见 §9）

## 1. 背景

终态通知今天是纯文本：`buildNotification`（`workflow/devicetest.go:666`）拼字符串，
`Notify` 活动经 `feishu.Sender.SendText` 发出。8 个变体的结果挤在一段无格式文本里，
verdict 与 category 要逐行读，看不出"这次该查代码还是查环境"。

**顺带更正 CLAUDE.md 的一处架构描述。** §4 写"交互卡片(按钮回调 → Runtime signal)"，
§12 Phase 2 写"重试/忽略/隔离按钮 → Runtime signal"。这个形态不存在：

- `Notify` 是 workflow 做的**最后一件事**（`devicetest.go:286-289`），紧接着 `return`。
- 全仓库只有一个 signal（`SignalTaskResult`，`devicetest.go:19/280`），无人工决策挂起点。

终态通知上的按钮没有 workflow 可以发信号给它。这条更正与本轮是否做按钮无关，必须写进
文档——否则下一个人还会照着不存在的形态设计（本 spec 前两版就是）。

## 2. 关键决策

1. **本轮只做展示卡片，不做按钮**（已确认，2026-07-30）。理由见 §9：按钮的正确性依赖
   四项目前不存在的持久化基础（其中一项是 CLAUDE.md §11 声明却从未建过的 `workflows` 表），
   而展示部分不依赖其中任何一项、且已通过评审无阻断项。
2. **新增独立的 `NotifyCard` 活动，`Notify` 签名一个字节不改**。`Notify` 今天是
   `func (a *Acts) Notify(ctx context.Context, text string) error`（`notify.go:11`）。
   改成结构体参数会让**在途或正在重试的活动**在进入 handler 之前解码失败——它记录的输入
   是字符串。
3. **header 颜色：业务失败优先于基础设施失败**（见 §4.1）。
4. **SKIPPED 变体照常展示，但不参与颜色判定**。运维需要知道哪些变体没测；而"fleet 无匹配
   设备"不是失败，不该让卡片变红。
5. **`SendCard` 失败降级为纯文本**，内容与改动前逐字节相同。Phase 1 的保证是"push 一次
   代码就能收到含 verdict 的通知"，不能因为卡片渲染或接口变更让通知整体消失。

不采用的方案：

- 改 `Notify` 的签名而不新增活动：见决策 2，会打断在途活动的解码。
- 一个变体一张卡片：8 个变体会刷 8 条消息。
- 卡片里带日志/证据链接：需要对外可达的 MinIO 地址与签名读 URL，属独立一轮（§9）。

## 3. 范围

交付：

- `feishu.Sender` 增加 `SendCard`（webhook 与 app 两种实现）
- `buildNotificationCard`：workflow 侧确定性生成卡片结构
- **新增** `NotifyCard` 活动（`Notify` 与 `buildNotification` 原样保留，旧版本分支仍在用）
- `workflow.GetVersion` 分支保护在途 workflow 重放
- 文档：CLAUDE.md §4/§12 的 signal 描述更正、`deploy/README.md` 通知一节

不交付（明确排除）：

- **按钮与其回调**（§9）
- `card_actions` / `workflow_runs` / `audit_log` 三张表
- NL 翻译 `rerun` 待确认改按钮
- 通知里带日志/证据链接
- 卡片模板化

## 4. 卡片形态

一张卡片对应一次 workflow（含全部变体）。

### 4.1 header 颜色

先定义**可判定失败** = `verdict ∉ {PASSED, SKIPPED}`。

| 条件 | 颜色 |
|---|---|
| 无可判定失败（全 PASSED，或只有 SKIPPED，或两者混合） | 绿 |
| 存在非 `INFRA_ERROR` 的可判定失败 | 红 |
| 可判定失败**全部**是 `INFRA_ERROR` | 橙 |

橙色的含义是"这次没测出代码问题，是环境/基础设施拦住了"。**业务失败优先**：一张同时有
`INFRA_ERROR` 与 `TEST_FAILED(CODE)` 的卡片是**红**，不是橙——否则会把"代码有问题"显示成
"环境有问题"，把排查方向指错。

`SKIPPED` 不参与判定（决策 4）。

### 4.2 布局

```text
┌─────────────────────────────────────────────┐
│ [绿/红/橙] algo-super-sdk g9da3b9d9 p56     │  header,底色按 §4.1
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
└─────────────────────────────────────────────┘
```

字段与现有纯文本携带的**完全一致**，不新增数据来源：

- 变体名、verdict，非 PASSED 时附 category
- `CasesTotal > 0` 时：`{passed}/{total} cases`
- 耗时、attempt
- `Reason`
- Analyzer 结论（`tk.Analysis.Summary`）沿用现有条件：仅非 PASSED 且分析成功时出现

`out.Tasks` 为空（无可测变体）时，卡片正文与现有纯文本的提示一致（"无可测变体…"）。

附件 key 仍不进通知（与 §12.6 现状一致）。

### 4.3 两种 Sender 都能发

自定义 webhook 机器人与企业自建应用机器人都支持 `msg_type: interactive`，所以展示卡片在
两种模式下都能发，**不需要 readiness 门禁**。（按钮不同——它需要应用订阅
`card.action.trigger`，这是它被拆到 §9 的原因之一。）

## 5. Temporal 兼容

1. `Notify(ctx, string)` **原样保留**，签名与行为一个字节不改。
2. **新增** `NotifyCard(ctx, NotifyCardRequest) error`，独立活动名，独立注册。
3. workflow 侧
   `workflow.GetVersion(ctx, "notify-card", workflow.DefaultVersion, 1)` 分支：
   旧版本调 `Notify(buildNotification(...))`，新版本调 `NotifyCard(buildNotificationCard(...))`。

`buildNotification` 因此**保留**，不是死代码——旧分支仍在用它，且降级路径（§6）也用它。

`NotifyCardRequest` 是 workflow 构造的确定性结构，进 history。它**只携带渲染所需的数据**，
不携带任何运行时配置（配置由 activity 侧的 `Acts` 持有）——activity 输入由 workflow 写入，
worker 无从修改，把配置放进载荷是行不通的。

## 6. 错误处理

| 情形 | 行为 |
|---|---|
| `SendCard` 失败（接口变更/渲染错误/限流） | 记日志 → 降级 `SendText` 发 `buildNotification` 的原文本，内容与改动前逐字节相同 |
| 降级发送也失败 | 记 error 日志，返回错误（与今天 `Notify` 失败的行为一致：workflow 记日志但不改结论） |
| 两种 Sender 都未配置（`Acts.Feishu == nil`） | 静默成功，与今天一致（开发模式） |
| `out.Tasks` 为空 | 发卡片，正文是"无可测变体"提示 |

降级不是可选项：它保证"通知不会因为卡片这层新东西而整体消失"。

## 7. 测试

**卡片构造（workflow 包，纯函数，表驱动）**——颜色优先级是重点：

| 用例 | 期望 |
|---|---|
| 全 PASSED | 绿 |
| 全 SKIPPED | 绿 |
| PASSED + SKIPPED 混合 | 绿 |
| 只有 `INFRA_ERROR` | 橙 |
| **`INFRA_ERROR` + `TEST_FAILED`** | **红**（业务失败优先，§4.1 的判据） |
| 只有 `TEST_FAILED` | 红 |
| `INFRA_ERROR` + `SKIPPED` | 橙（SKIPPED 不参与判定） |
| `out.Tasks` 为空 | 绿，正文为"无可测变体"提示 |

以及：Analyzer 结论存在/不存在两种情形；每个变体行携带的字段与
`buildNotification` 对同一输入产出的字段集合一致（防止卡片化时悄悄丢字段）。

**Sender（两种实现）**：`SendCard` 成功；`SendCard` 失败时调用方降级到 `SendText`，
且**发出的文本与 `buildNotification` 的输出逐字节相同**。

**活动兼容**：`Notify(ctx, string)` 的签名与行为逐字未变（既有 `notify_test.go` 用例
一条都不该改——这是判据）；`NotifyCard` 是独立注册的活动名。

**重放**：旧版本分支仍调 `Notify` 并收到字符串载荷（用既有的 fake activity 断言）。

## 8. 验收标准

- 终态通知是卡片，header 底色按 §4.1
- **`INFRA_ERROR` 与 `TEST_FAILED` 混合时是红色，不是橙色**（有测试）
- SKIPPED 变体在卡片上可见，但不影响颜色（有测试）
- 卡片携带的字段与改动前纯文本一致，无遗漏（有测试）
- `SendCard` 失败时纯文本通知照发，内容与改动前逐字节相同（有测试）
- `Notify(ctx, string)` 未改签名，既有 `notify_test.go` 一条未改即通过
- 在途 workflow 重放不因本次改动失败
- CLAUDE.md §4/§12 的 signal 描述已更正，并注明按钮未实现及其前置

## 9. 按钮为何拆出去（下一轮的输入）

两轮评审把按钮的前置条件挖清楚了。这些结论必须留在这里，否则下一轮会重新发现一遍：

**（1）幂等不能靠事件 ID。** 两次真实点击是两个不同的飞书事件 ID；
`listener.go` 的 `dedupCache` 是进程内 10 分钟缓存、重启即失效；多人同点同样是不同事件。
事件 ID 只能挡传输重投。

**（2）幂等也不能只靠"业务键 claim"。** 重试会调 `NextWorkflowAttempt`（递增计数器）。
若 Temporal 已启动成功、而写 `succeeded` 前崩溃，恢复时再调一次就会拿到新的 `-r{N+1}`、
起第二个 workflow 重复测试。所以**首次 claim 必须把它的产物固定下来**：
`attempt`、`target_workflow_id`、`claim_token`/`lease_until`。恢复只能用**同一个
workflow ID** 再调 `StartDeviceTest`，让 Temporal 自己去重；绝不能再次递增 attempt。
`failed → pending` 必须是带状态条件的 CAS（`UPDATE … WHERE state='failed' RETURNING`），
`pending` 必须有 owner 与租约，否则 claim 后崩溃会永久卡在"处理中"。

**（3）`Version` / `RuleVersion` 没有权威来源。** `artifacts` 与 `tasks` **都没有**这两列
（`schema.sql` 的 `version` 在 `clients` 表，是 agent 版本）。这不只是历史数据问题——
新写入的数据同样恢复不出来。而 `rerun` 指令今天就在丢它们（`executor.go:339` 只填
Project/Commit/PipelineID），于是对同一产物的重跑可能用**不同规则版本**重新判定，
而 `rule_version` 存在的全部目的就是让判定可回放。
需要新建持久化模型（`workflow_runs`：workflow_id / project / commit / pipeline / version /
rule_version），定义写入点、唯一键与历史行 fallback。**这实际就是 CLAUDE.md §11 声明却
从未建过的 `workflows` 表**——它已经绞了两轮设计（差距 #10 那轮的 `RecentRuns` 也是因为
它缺失才退化成 workflow_id 字符串前缀匹配）。

**（4）目标绑定必须完全从权威记录派生。** 只校验 task 的 workflow_id 不够：载荷仍可用
workflow A 的合法 task 通过校验，却拿 workflow B 的 project/commit/pipeline 去查 artifacts
并启动。按钮应只携带 `source_workflow_id` + `action`，其余全部从权威 run/task 记录派生；
查询必须精确匹配 workflow_id，**不能复用 `RecentRuns` 的 base-prefix 语义**。

**（5）retry 与 ignore 是否互斥要先定义。** 若 action key 含 action，两人同时点 retry 与
ignore 都能 claim 成功；而卡片"点任一按钮后两个都置灰"的观感暗示互斥。互斥就按
`workflow_id + variant` 共用 claim 并记录 chosen action。

**（6）ignore 的三次写入需要原子边界。** `decisions` 写完、`audit_log` 或 `succeeded`
更新前崩溃，恢复会重复写 human decision。三者应在一个 store 事务内完成，用 action key 保幂等。
另注意 `decisions.output` 是 `JSONB NOT NULL`（`schema.sql:109`），必须给出具体 JSON。

**（7）readiness 是运行时配置，不能放进 activity 载荷。** activity 输入由 workflow 写入
history，worker 装配时改不了。按钮的可用性判定应是 `Acts`/sender 的运行时配置，由
`NotifyCard` handler 在发送前决定是否剥掉按钮；并且**本地注册 handler 不能证明飞书后台已
订阅 `card.action.trigger`**，需要显式门禁（如 `FEISHU_CARD_ACTIONS_ENABLED`），
WS 退出后也要关掉交互能力。

**（8）副作用回调需要 fail-closed 输入约束**，且全部在 claim 之前完成：拒绝空身份、
未知 action、重复/超量 variants、非终态 task、过大 payload、错误的 app/tenant。
非白名单提示必须是**同步 callback toast**——不能用 `SendText`，那会发到固定的
`FEISHU_RECEIVE_ID`（可能是群），等于把一次未授权点击广播给所有人。

**（9）`audit_log` 表不存在。** CLAUDE.md §37 要求"所有操作落 audit_log"、§11 给了结构，
但 `schema.sql` 里没有这张表、全仓库无写入方。按钮那轮应建表并成为第一个写入方——
人工触发的重跑正是审计日志存在的理由。

**（10）「忽略」的语义要写清**：它只记录"人看过并决定忽略"，不修改 `tasks.verdict`、
不影响任何后续策略；当下**没有任何消费方读取 human decision**。不写明的话，第一个用它的人
会以为按钮坏了。

建议下一轮的顺序：先做 `workflow_runs`（它已经绞了两轮设计，且不只服务按钮），再做按钮。

## 10. 后续（不在本轮，也不在按钮那轮）

- NL 翻译 `rerun` 待确认改卡片按钮
- 「隔离」按钮——等设备级信号源落地（差距 #10 §7）
- 通知里带日志/证据链接
- 卡片模板化（卡片 JSON 直接内联会随字段增多变长）
