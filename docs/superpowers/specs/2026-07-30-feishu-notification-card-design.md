# 飞书终态通知卡片化设计

日期：2026-07-30

状态：待批准

## 1. 背景与一处必须先更正的架构描述

终态通知今天是纯文本：`buildNotification`（`workflow/devicetest.go:666`）拼一个字符串，
`Notify` 活动经 `feishu.Sender.SendText` 发出。§12.6 与该函数注释都写明"交互卡片属 Phase 2"。

**但 CLAUDE.md 对这一项的描述是错的，先更正再设计。** §4 写"交互卡片(按钮回调 → Runtime
signal)"，§12 Phase 2 写"重试/忽略/隔离按钮 → Runtime signal"。这个形态不存在：

- `Notify` 是 workflow 做的**最后一件事**（`devicetest.go:286-289`），紧接着 `return`。
  用户看到通知时 workflow 已经结束。
- 全仓库只有一个 signal（`SignalTaskResult`，`devicetest.go:19/280`），没有任何等待人工
  决策的挂起点。

所以终态通知上的按钮**没有 workflow 可以发信号给它**。按钮只能映射到事后仍成立的动作。
本设计据此重新定义按钮语义，并要求同步更正 CLAUDE.md（见 §8）。

## 2. 关键决策

1. **本轮只做终态通知卡片化**（已与负责人确认，2026-07-30）。NL 翻译的 `rerun` 待确认仍用
   `y`/`n` 文本——那是唯一真正"等人点"的场景，语义已经清楚（内存槽 + TTL + 审计），
   换成按钮属独立一轮。
2. **按钮只放「重试」与「忽略」**（已确认）。两者都复用现成机制：
   - 重试 → 走 `feishucmd` 的 `rerun` 路径（`ListArtifacts` → `NextWorkflowAttempt` →
     `StartDeviceTest`，含 `-r{N}` 递增与包齐整性校验）
   - 忽略 → 写一行 `decisions`，`actor=human`。§11 早就定义了这个取值，而**至今没有任何
     代码写入过它**；忽略按钮是它的第一个写入方
   
   不做「隔离」：它是新能力，而上一轮把自动隔离关掉了（device 归因无信号源），
   当下没有"设备真坏了"的信号源来验证这个按钮有用。解隔离已有 `unquarantine` 指令。
3. **按钮回调必须沿用指令层白名单**。卡片发往 `FEISHU_RECEIVE_ID`，那可能是个群；群里任何
   人都点得到按钮。不校验 `open_id` 就等于把 `rerun` 权限开给整个群。复用
   `feishucmd.Executor.Whitelist`，非白名单点击只回一句提示、不执行。
4. **卡片发送失败降级为纯文本**。Phase 1 的保证是"push 一次代码就能收到含 verdict 的通知"，
   不能因为卡片渲染/接口变更让通知整体消失。
5. **按钮动作按事件去重**。卡片按钮比文本指令容易连点，且飞书长连接会重投事件
   （`d8ded86` 已实证过一次）。重试若被执行两次会起两个**不同** `-r{N}` 的 workflow，
   Temporal 的 `RejectDuplicate` 挡不住——必须在动作层去重。

不采用的方案：

- 让 workflow 在 `Notify` 后挂起等按钮 signal：那要把 workflow 生命周期从"跑完即止"改成
  "等人处理"，牵动租约、超时与 `HARD_TIMEOUT_MARGIN_SEC` 的全部语义，代价与收益不成比例。
- 按钮走一个新的 HTTP 端点：飞书按钮回调本可配 URL，但那要再开一个入站面并单独鉴权；
  现有 WS 长连接已经有白名单、异步与去重机制（`feishucmd/listener.go`），复用它更省。

## 3. 范围

交付：

- `feishu.Sender` 增加 `SendCard`（webhook 与 app 两种实现）
- `buildNotificationCard`：workflow 侧生成卡片结构（确定性，与 `buildNotification` 并存）
- `Notify` 活动支持卡片载荷，失败降级纯文本
- 卡片按钮回调：接入既有 WS listener，白名单校验 + 事件去重 + 执行 + 更新卡片
- 「忽略」写 `decisions(actor=human)`
- 文档：CLAUDE.md §4/§12 的 signal 描述更正、`deploy/README.md` 通知一节

不交付（明确排除）：

- NL 翻译 `rerun` 待确认改按钮（决策 1）
- 「隔离」按钮（决策 2）
- 卡片模板化/多语言
- 通知渠道扩展（仍是单一 `FEISHU_RECEIVE_ID`）

## 4. 卡片形态

一张卡片对应一次 workflow（可能含多个变体），而不是一个变体一张——避免 8 个变体刷 8 条。

```text
┌─────────────────────────────────────────────┐
│ [绿/红/橙] algo-super-sdk g9da3b9d9 p56     │  header,底色按整体 verdict
├─────────────────────────────────────────────┤
│ aarch64_Android_SNPE_2.21  PASSED           │
│   38/38 cases · 412.3s · attempt 1          │
│                                             │
│ aarch64_Android_SNPE_1.68  TEST_FAILED(CODE)│
│   35/38 cases · 380.1s · attempt 2          │
│   hermes: 三个用例在 DSP 初始化处崩溃        │
├─────────────────────────────────────────────┤
│ [ 重试失败变体 ]  [ 忽略 ]                   │  仅在存在非 PASSED 变体时出现
└─────────────────────────────────────────────┘
```

- header 底色：全 PASSED → 绿；含 `INFRA_ERROR` → 橙（基础设施问题，不是代码问题）；
  其余非 PASSED → 红。这个映射让人一眼分清"要查代码"与"要查环境"。
- 每个变体一行主信息 + 一行指标，与现有纯文本携带的字段一致（不新增数据来源）。
- Analyzer 结论（`tk.Analysis.Summary`）沿用现有条件：仅非 PASSED 且分析成功时出现。
- 全 PASSED 时**不放按钮**：没有要重试的，也没有要忽略的。
- 附件 key 仍不进通知（与 §12.6 现状一致）——本轮不引入日志链接，那需要能对外访问的
  MinIO 地址与签名读 URL，属独立一轮。

## 5. 按钮语义

按钮 `value` 携带执行所需的全部上下文（卡片是无状态的，回调时 Runtime 不查缓存）：

```json
{
  "action": "retry",
  "project": "algo-super-sdk",
  "commit": "9da3b9d9",
  "pipeline_iid": 56,
  "variants": ["aarch64_Android_SNPE_1.68"],
  "task_ids": ["device-test-...:aarch64_Android_SNPE_1.68:a2"],
  "event_key": "notify:device-test-algo-super-sdk-g9da3b9d9-p56"
}
```

| 动作 | 行为 | 复用 |
|---|---|---|
| `retry` | 对 `variants` 里的每个变体起一次新 workflow | `feishucmd` 的 `rerun` 单变体路径 |
| `ignore` | 对 `task_ids` 里的每个任务写一行 `decisions`，`actor=human` | `store.SaveDecision` |

**重试只重试非 PASSED 的变体**，不整包重跑：整包重跑会把已经通过的变体再占一遍设备，
而 Phase 1 只有 1~2 块板。按钮文案因此写"重试失败变体"而不是"重试"。

`ignore` 写入的 `decisions.output` 形态：

```json
{"ignored_by": "ou_xxx", "verdict": "TEST_FAILED", "category": "CODE", "at": "2026-07-30T09:12:00Z"}
```

`input_digest` 留空（无 evidence 输入），`model`/`prompt_version` 留空，
`evidence_snapshot_id` 留空——与 `rule` 裁决同形（§11 已允许这几列为空）。

## 6. 回调路径与两条硬性约束

```text
用户点按钮
  → 飞书 WS 事件(card action) → feishucmd.Listener
  → ① 白名单校验(open_id 不在 Whitelist → 只回提示,不执行)
  → ② 事件去重(同一 event id 已处理过 → 直接返回)
  → 异步执行(回调必须立即 ack,起 workflow 是秒级但不是零)
  → 执行 retry / ignore
  → 更新卡片(按钮置灰 + 追加"已由 @某人 重试/忽略")
```

**① 白名单**：与文本指令同一份 `Whitelist`。卡片发往群时，群成员都能看到按钮；
非白名单点击必须拒绝。这是本设计唯一的鉴权。

**② 去重**：卡片动作事件与消息事件一样会被重投（`listener.go` 已有 `dedupCache`，
TTL 10min + 上限 1000）。复用它，键取事件自带的 id。

去重之所以是硬性约束而非优化：重试若被执行两次，`NextWorkflowAttempt` 递增两次，
产生两个**不同** workflow ID（`-r2`、`-r3`），Temporal 的 `RejectDuplicate` 完全挡不住，
结果是同一个变体被并发测两次、抢同一块板。

**卡片更新**是第三道防线（也是给人的反馈）：动作执行后把按钮置灰，UI 上不再可点。

## 7. 错误处理

| 情形 | 行为 |
|---|---|
| `SendCard` 失败（接口变更/渲染错误） | 记日志 → 降级 `SendText` 发原纯文本（决策 4） |
| 两种 Sender 都未配置 | 与今天一致：静默跳过通知 |
| 按钮点击者非白名单 | 回一句"无权限"，不执行，记 info 日志 |
| 事件重投 | 去重命中，直接返回，不重复执行 |
| `retry` 时 artifacts 查无记录 | 回执明确报错（与 `rerun` 指令同一文案） |
| `retry` 起 workflow 失败 | 回执报错，不更新卡片（按钮保持可点，允许重试这个动作本身） |
| `ignore` 写库失败 | 回执报错，不更新卡片 |
| 卡片更新失败 | 记日志，不影响动作本身已执行的事实 |

## 8. Temporal 重放与文档更正

**重放**：workflow 侧从"调 `Notify(text)`"改为"调 `Notify(卡片载荷)`"会改变 activity 输入，
在途 workflow 重放时命令与历史不匹配。与差距 #10 那轮同样处理：
`workflow.GetVersion(ctx, "notify-card", workflow.DefaultVersion, 1)` 分支，
旧分支原样发文本。`buildNotification` 因此**保留**，不是死代码。

**文档更正**（必做，否则下一个人还会照着不存在的形态设计）：

- CLAUDE.md §4 通知一行：`交互卡片(按钮回调 → Runtime signal)` → 改为
  `交互卡片(按钮回调 → 经 WS listener 执行,非 workflow signal:终态通知发出时 workflow 已结束)`
- CLAUDE.md §12 Phase 2：`重试/忽略/隔离按钮 → Runtime signal` 同上更正，并注明
  隔离按钮未实现及其原因（无设备级信号源，见差距 #10）

## 9. 测试

**卡片构造（workflow 包，纯函数）**：
- 全 PASSED → 绿色 header、**无按钮**
- 含 `INFRA_ERROR` → 橙色
- 含其他非 PASSED → 红色
- 按钮 `value` 里的 `variants` **只含非 PASSED 变体**（这是"不整包重跑"的判据）
- 无可测变体（`out.Tasks` 为空）→ 与纯文本一致的提示，无按钮
- Analyzer 结论存在/不存在两种情形

**Sender（两种实现）**：`SendCard` 成功；失败时调用方降级到 `SendText` 且**文本内容与
改动前逐字节相同**。

**回调（fake store + fake starter）**：
- 白名单内 `retry` → 起 workflow，且只对非 PASSED 变体起
- **非白名单点击 → 不执行**（断言 fake starter 零调用）
- 同一事件 id 重投 → 只执行一次（断言 fake starter 只被调一次）
- `ignore` → `decisions` 落一行 `actor=human`
- `retry` 起 workflow 失败 → 不更新卡片
- 卡片更新失败 → 动作仍算已执行（不回滚）

**重放兼容**：旧分支仍发文本（断言 fake activity 收到的是字符串载荷）。

## 10. 验收标准

- 终态通知是卡片，header 底色按 verdict 分三色
- 全 PASSED 的卡片没有按钮
- 「重试失败变体」只对非 PASSED 变体起 workflow（有测试）
- 非白名单点击按钮不执行任何动作（有测试）
- 同一按钮事件重投只执行一次（有测试）
- 「忽略」在 `decisions` 留下 `actor=human` 的行——该取值的第一个写入方（有测试）
- `SendCard` 失败时纯文本通知照发，内容与改动前一致（有测试）
- 在途 workflow 重放不因本次改动失败
- CLAUDE.md §4/§12 的 signal 描述已更正

## 11. 后续（不在本轮）

- NL 翻译 `rerun` 待确认改卡片按钮（替代 `y`/`n` 文本）
- 「隔离」按钮——等设备级信号源落地（差距 #10 §7）后再评估
- 通知里带日志/证据链接：需要对外可达的 MinIO 地址与签名读 URL
- 卡片模板化（飞书卡片 JSON 直接内联会随字段增多变长）
