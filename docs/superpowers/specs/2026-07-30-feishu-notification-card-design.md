# 飞书终态通知卡片化设计（展示部分）

日期：2026-07-30

状态：待批准（v5，2026-07-30 第四轮评审后修订：CardSender 而非扩展 Sender、全量动态文本、节点白名单、边界精确化）

## 1. 背景

终态通知今天是纯文本：`buildNotification`（`workflow/devicetest.go:667`）拼字符串，
`Notify` 活动经 `feishu.Sender.SendText` 发出。8 个变体的结果挤在一段无格式文本里，
verdict 与 category 要逐行读，看不出"这次该查代码还是查环境"。

**顺带更正 CLAUDE.md 的一处架构描述。** §4 写"交互卡片(按钮回调 → Runtime signal)"，
§12 Phase 2 写"重试/忽略/隔离按钮 → Runtime signal"。这个形态不存在：`Notify` 是 workflow
做的最后一件事（`devicetest.go:286-289`），紧接着 `return`；全仓库只有一个 signal
（`SignalTaskResult`），无人工决策挂起点。这条更正与本轮是否做按钮无关，必须写进文档——
否则下一个人还会照着不存在的形态设计（本 spec 前两版就是）。

## 2. 关键决策

1. **本轮只做展示卡片，不做按钮**（已确认）。理由与下一轮的完整输入见 §10。
2. **新增独立的 `NotifyCard` 活动，`Notify` 签名一个字节不改**。`Notify` 今天是
   `func (a *Acts) Notify(ctx context.Context, text string) error`（`notify.go:11`）。
   改成结构体参数会让**在途或正在重试的活动**在进入 handler 之前解码失败——它记录的输入
   是字符串。
3. **降级文本由 workflow 生成并随载荷下发**，activity 绝不自行拼文本（见 §5.1）。
4. **header 颜色：业务失败优先于基础设施失败**（见 §4.1）。
5. **卡片字段与纯文本严格对齐，仅一处有意偏离并显式列出**（见 §4.3）。
6. **卡片里**所有**动态文本节点一律 `plain_text` 渲染**，并有 UTF-8 安全的裁剪与总体积
   预算（见 §4.4）。
7. **不扩展 `feishu.Sender`，另加 `CardSender`**（见 §5.4）。往 `Sender` 上加方法会让
   `notify_test.go` 与 `feishucmd/executor_test.go` 里两个只实现 `SendText` 的 fake
   直接编译失败——而"既有测试一条未改"正是本轮的验收判据，那样就自相矛盾了。

不采用的方案：

- 改 `Notify` 的签名而不新增活动：会打断在途活动的解码。
- activity 侧重新拼降级文本：两处实现同一格式必然漂移，而"逐字节相同"是本轮的验收项。
- 动态字段用 `lark_md`：`Reason` 与 Analyzer summary 都是不可信文本（后者是 **LLM 输出**），
  markdown 渲染会让链接、@ 提及、标签被解释。见 §4.4。
- 一个变体一张卡片：8 个变体会刷 8 条消息。

## 3. 范围

交付：

- `feishu` 包新增 `CardSender` 接口（`Sender` 原样不动）+ 两种实现的 `SendCard`（见 §5.4）
- `buildNotificationCard`：workflow 侧确定性生成卡片结构
- **新增** `NotifyCard(ctx, NotifyCardRequest) error` 活动（`Notify` 与 `buildNotification` 原样保留）
- `workflow.GetVersion` 分支 + 旧 history 的 `WorkflowReplayer` 测试（见 §6）
- 文档：CLAUDE.md §4/§12 的 signal 描述更正、`deploy/README.md` 通知一节

不交付（明确排除）：

- **按钮与其回调**（§10）
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
"环境有问题"，把排查方向指错。`SKIPPED` 不参与判定。

### 4.2 布局

```text
┌──────────────────────────────────────────────────┐
│ [绿/红/橙] algo-super-sdk g9da3b9d9 p56 (v1.4.2) │  header,含 Version
├──────────────────────────────────────────────────┤
│ aarch64_Android_SNPE_2.21  PASSED                │
│   412.3s · cases 38/38 · attempt 1               │
│                                                  │
│ aarch64_Android_SNPE_1.68  TEST_FAILED(CODE)     │
│   380.1s · cases 35/38 · attempt 2               │
│   three cases crashed at DSP init                │
│   hermes: 三个用例在 DSP 初始化处崩溃             │
│                                                  │
│ aarch64_Linux_RKNN_2.3.2   SKIPPED               │
│   fleet 无匹配设备                                │
└──────────────────────────────────────────────────┘
```

### 4.3 字段一致性：逐条对齐纯文本

`buildNotification`（`devicetest.go:667-690`）的确切行为，卡片必须逐条对齐：

| 纯文本 | 条件 | 卡片 |
|---|---|---|
| `[hermes-devops] {project} g{commit} p{pipeline} (v{version})` | 总是 | header 标题，**含 Version**（v3 的示意图漏了它） |
| `- {variant}: {verdict}` | 总是 | 变体行主标题 |
| `({category})` | `Category != "" && Verdict != PASSED` | 附在 verdict 后，同条件 |
| ` {duration}s cases={passed}/{total}` | **`CasesTotal > 0`（duration 与 cases 同一个门控）** | 指标行，**同一个门控**（v3 误把 duration 写成无条件） |
| ` attempt={attempt}` | 总是（因此 SKIPPED 会输出 `attempt=0`） | 见下方唯一偏离 |
| ` {reason}` | 总是（可能为空串） | 独立一行，空则不出该行 |
| `  · hermes: {summary}` | `Analysis != nil && Summary != ""` | 独立一行，同条件 |
| `无可测变体(Android 包缺失或未配置)` | `len(out.Tasks) == 0` | 卡片正文同文案 |

**唯一有意偏离**：`verdict == SKIPPED` 时卡片**不显示 attempt**。纯文本会打
`attempt=0`——一个从未执行过的变体谈"第 0 次尝试"是噪声。

这处偏离只作用于卡片。**降级文本是 `buildNotification` 的逐字节原样输出**（§5.1），
不受此影响——所以"逐字节相同"这个验收项与本偏离不冲突。

### 4.4 渲染安全与体积边界

**卡片里的每一处动态文本都是不可信输入，不只是 `Reason` 与 `Analysis.Summary`。**
完整清单：

| 字段 | 来源 | 备注 |
|---|---|---|
| `TaskSummary.Reason` | 设备/Client 的任意错误文本 | 最不可控 |
| `Analysis.Summary` | **LLM 输出** | 模型产物 |
| `in.Project` | GitLab webhook / bundle | **没有字符白名单** |
| `in.Version` | 业务仓库 CMakeLists | 形态约定为 X.Y.Z，但未强制校验 |
| `in.Commit` | bundle | hex，形态受限 |
| `TaskSummary.Variant` | `variants.yaml` | 受配置约束但仍是运行时输入 |
| `TaskSummary.Verdict` / `Category` | 规则引擎枚举 | 取值有界 |

**规则：卡片里所有文本节点一律 `plain_text`，不用 `lark_md`。** 理由不是洁癖：
markdown 渲染会让 `[text](url)`、`<at user_id="...">`、标签语法被解释。Analyzer summary
是模型产物，用 markdown 渲染等于允许模型在通知里插链接和 @ 提及；而 `Project` 连字符
白名单都没有。若将来某处确需 markdown，必须显式转义后再放。

验证方式是**递归断言整张卡片的每个文本节点类型**（§8），而不是逐字段列举——列举会随
字段增加而漏。

**裁剪与预算**（超出即截断并加省略标记）：

| 项 | 上限 |
|---|---|
| 单个 `Reason` | 500 rune |
| 单个 `Analysis.Summary` | 500 rune |
| 卡片总大小 | `len(json.Marshal(card)) <= 30*1024` |

总大小的处置顺序，必须按此执行：

1. 超限 → 按变体顺序从末尾丢弃可选行（reason 行、hermes 行）
2. 加上"（N 个变体的详情已省略）"标注后**重新测量**——标注本身也占字节
3. 仍超限 → 直接走降级纯文本（§5.2）

第 2 步是容易漏的一环：加标注后不重测，等于把一个刚裁到边界的卡片又推回超限。

裁剪必须**按 rune 边界**，不能按字节——中文在 UTF-8 里是 3 字节，按字节切会产生半个字符，
飞书侧渲染成乱码甚至拒收。

总预算的兜底是：**任何裁剪都不得让卡片发送失败**。真的超了，降级纯文本（§5.2）仍然可用。

## 5. 载荷与降级

### 5.1 `NotifyCardRequest` 携带降级文本

`buildNotification` 是 **workflow 包内的非导出函数**，activity 拿不到它。所以降级文本
必须由 workflow 生成并随载荷下发：

```go
// NotifyCardRequest 是 NotifyCard 活动的输入(进 workflow history)。
// FallbackText 由 workflow 调既有的 buildNotification 生成并随载荷下发:
// activity 侧**绝不自行拼文本**——两处实现同一格式必然漂移,而"降级内容与改动前
// 逐字节相同"是本轮的验收项。
type NotifyCardRequest struct {
	Card         NotificationCard `json:"card"`
	FallbackText string           `json:"fallback_text"`
}
```

workflow 侧：

```go
req := NotifyCardRequest{
	Card:         buildNotificationCard(in, out),
	FallbackText: buildNotification(in, out), // 原函数,原样调用
}
```

`buildNotification` 因此**保留且仍在主路径上被调用**，不是死代码。

### 5.2 降级顺序

1. `NotifyCard` → `Feishu.SendCard(req.Card)`
2. 失败（接口变更 / 渲染错误 / 限流 / 超体积）→ 记日志 → `Feishu.SendText(req.FallbackText)`
3. 降级也失败 → 记 error 并返回错误（与今天 `Notify` 失败一致：workflow 记日志、不改结论）

降级不是可选项：它保证"通知不会因为卡片这层新东西而整体消失"。

### 5.3 两种 Sender 的载荷不对称

现状（`feishu.go:151` / `feishu.go:247`）：

- **webhook**：`{"msg_type":"text","content":{"text":…}}`——`content` 是**对象**
- **app**：`content` 是 `json.Marshal` 后的**字符串**，随 `msg_type` 一起 POST

卡片版本沿用各自形态：

- webhook：`{"msg_type":"interactive","card":{…卡片对象…}}`
- app：`{"receive_id":…,"msg_type":"interactive","content":"{…序列化后的卡片…}"}`

app 侧的 token 过期重试逻辑（`SendText` 里那段"强制刷新重试一次"）必须同样覆盖 `SendCard`。

### 5.4 用 `CardSender` 而不是扩展 `Sender`

`feishu.Sender` 今天只有 `SendText`，而**两处测试 fake 只实现了它**：
`activity/notify_test.go:12` 的 `fakeSender` 与 `feishucmd/executor_test.go` 的同名 fake。
往 `Sender` 接口上加 `SendCard` 会让这两处直接编译失败——与本轮"既有 `notify_test.go`
一条未改即通过"的验收判据正面冲突。

因此：

```go
// Sender 原样不动。
type Sender interface {
	SendText(ctx context.Context, text string) error
}

// CardSender 是能发交互卡片的 Sender(差距:终态通知卡片化)。
// 单独成接口而非扩展 Sender:后者会让只实现 SendText 的既有 fake 编译失败。
type CardSender interface {
	Sender
	SendCard(ctx context.Context, card any) error
}
```

`Acts.Feishu` 的静态类型仍是 `feishu.Sender`。`NotifyCard` 做类型断言：

```go
cs, ok := a.Feishu.(feishu.CardSender)
if !ok {
    // 注入的 Sender 不支持卡片(如老的测试 fake):直接走降级文本,不是错误
    return a.Feishu.SendText(ctx, req.FallbackText)
}
```

`NewSender` 返回的两种真实实现都满足 `CardSender`，所以生产路径总是走卡片；
断言失败只会发生在注入了旧 fake 的测试里，而那时降级正是想要的行为。

## 6. Temporal 兼容

1. `Notify(ctx, string)` **原样保留**，签名与行为一个字节不改。
2. **新增** `NotifyCard(ctx, NotifyCardRequest) error`，独立活动名，独立注册。
3. workflow 侧
   `workflow.GetVersion(ctx, "notify-card", workflow.DefaultVersion, 1)` 分支：
   旧版本调 `Notify(buildNotification(...))`，新版本调 `NotifyCard(...)`。

**重放验证不能只靠 fake activity。** 用 fake 只能证明"旧分支仍传字符串"，证明不了
version marker 的位置不会与历史不匹配。

**本轮选定：做 `WorkflowReplayer` 测试**（不留二选一——一个分叉的验收标准等于没有标准）。
可行性已确认：仓库有 `internal/testtemporal` 的内嵌 Temporal，`spike` 包里已有跑真实
workflow 的先例。

实施顺序是关键，必须先录后改：

1. **在动任何代码之前**，用当前（改动前）的 workflow 代码跑一次完整 `DeviceTestWorkflow`，
   导出 history 为 JSON，作为 fixture 提交（如
   `runtime/internal/workflow/testdata/history-pre-notify-card.json`）
2. 再做本轮改动
3. 加 `WorkflowReplayer` 测试，喂该 fixture，断言重放无非确定性错误

顺序反了就录不到"改动前"的 history 了——这是本轮唯一一处对任务内步骤顺序有硬要求的地方，
实施计划里要写明。

## 7. 错误处理

| 情形 | 行为 |
|---|---|
| `SendCard` 失败 | 记日志 → `SendText(req.FallbackText)`（§5.2） |
| 降级发送也失败 | 记 error 日志，返回错误 |
| 两种 Sender 都未配置（`Acts.Feishu == nil`） | 静默成功，与今天一致（开发模式） |
| 卡片超总预算 | 按 §4.4 丢弃末尾变体的可选行并标注；仍超则直接走降级 |
| `Reason` / `Summary` 超单项上限 | 按 rune 边界截断并加省略标记 |
| `out.Tasks` 为空 | 发卡片，正文是"无可测变体"提示 |

## 8. 测试

**卡片构造（workflow 包，纯函数，表驱动）**

颜色优先级：

| 用例 | 期望 |
|---|---|
| 全 PASSED | 绿 |
| 全 SKIPPED | 绿 |
| PASSED + SKIPPED | 绿 |
| 只有 `INFRA_ERROR` | 橙 |
| **`INFRA_ERROR` + `TEST_FAILED`** | **红**（业务失败优先） |
| 只有 `TEST_FAILED` | 红 |
| `INFRA_ERROR` + `SKIPPED` | 橙 |
| `out.Tasks` 为空 | 绿，正文为"无可测变体"提示 |

字段一致性（对齐 §4.3 逐条）：

- header 含 Version
- `CasesTotal == 0` → 指标行**不含** duration 也不含 cases（同一门控）
- `CasesTotal > 0` → 两者都有
- `Category == ""` 或 `Verdict == PASSED` → 不显示 category
- `Reason == ""` → 不出 reason 行
- `Analysis == nil` 或 `Summary == ""` → 不出 hermes 行
- `Verdict == SKIPPED` → **不显示 attempt**（唯一有意偏离）
- 其余 verdict → 显示 attempt

渲染安全与边界：

- **递归遍历整张卡片，断言每个文本节点的类型都是 `plain_text`**。这是主断言——逐字段
  列举会随字段增加而漏，递归不会。
- 含 markdown / `<at>` 语法的输入，**逐字段各一例**，都断言渲染成字面文本：
  `Reason`、`Analysis.Summary`（LLM 输出）、**`Project`**（无字符白名单）、**`Variant`**。
  例如 `Project = "a[x](http://evil)b"`、`Variant = "v<at user_id=\"all\">"`。
- **纯中文的超长 `Reason` 截断后仍是合法 UTF-8**（按 rune 切的判据；按字节切这条会红）
- 500 rune 以上的 `Reason` / `Summary` 被截断并带省略标记
- 超总预算的输入 → 卡片被裁到 `len(json.Marshal(card)) <= 30*1024` 以内，带"详情已省略"标注，
  **且标注计入后重新测量仍在预算内**
- 裁到极限仍超预算 → 走降级纯文本（断言 `SendCard` 未被调用或其失败被降级接住）

节点类型白名单（而非黑名单）：

- 递归遍历卡片树，断言每个节点的类型都在**本轮实际使用的白名单**内
  （如 `header` / `div` / `plain_text` / `hr` —— 以实现为准）。
- 用白名单而不是禁 `button`/`action`/`callback`/`value`：黑名单挡不住 `select`、
  `overflow`、`picker`、带 `open-url` behavior 的节点等等。白名单守的是"本轮不做按钮"
  这个范围边界，且不需要穷举飞书的全部交互组件类型。

**Sender（两种实现，精确断言）**

- webhook：`msg_type == "interactive"`，且 `card` 是**对象**
- app：`msg_type == "interactive"`，且 `content` 是**序列化后的卡片字符串**（解析回来与原卡片相等）
- app：首次返回 token 失效错误码 → 强制刷新并**重试一次**，第二次成功即整体成功
- `SendCard` 失败 → 调用方降级 `SendText`，且发出的文本与 `buildNotification` 的输出
  **逐字节相同**

**活动兼容**

- `Notify(ctx, string)` 签名与行为逐字未变：**既有 `notify_test.go` 一条都不该改**（判据）
- `NotifyCard` 是独立注册的活动名
- `NotifyCardRequest.FallbackText` 等于同一输入下 `buildNotification` 的输出

**重放**：§6 的 `WorkflowReplayer` 测试；若不可行，按 §6 收窄验收表述。

## 9. 验收标准

- 终态通知是卡片，header 底色按 §4.1，且**含 Version**
- `INFRA_ERROR` 与 `TEST_FAILED` 混合时是红色，不是橙色（有测试）
- SKIPPED 变体在卡片上可见，不影响颜色，且不显示 attempt（有测试）
- 卡片字段与 §4.3 的对照表逐条一致，除表中列出的唯一偏离（有测试）
- **递归断言**整张卡片每个文本节点均为 `plain_text`；`Project` / `Variant` / `Reason` / `Summary` 各有含 markdown、`<at>` 语法的用例（有测试）
- 超长中文 `Reason` 截断后仍是合法 UTF-8（按 rune 切，有测试）
- 卡片总大小判据是 `len(json.Marshal(card)) <= 30*1024`，且省略标注计入后重新测量（有测试）
- 卡片树每个节点类型都在本轮的白名单内（而非黑名单排除，有测试）
- `SendCard` 失败时降级文本与 `buildNotification` 输出逐字节相同（有测试）
- `Notify(ctx, string)` 未改签名；`feishu.Sender` 未扩展；`notify_test.go` 与 `feishucmd/executor_test.go` 的既有 fake 一行未改即编译通过
- 在途 workflow 重放不失败——由改动前录制的 history fixture + `WorkflowReplayer` 测试证明（§6 已选定此项，不留二选一）
- CLAUDE.md §4/§12 的 signal 描述已更正，并注明按钮未实现及其前置

## 10. 按钮为何拆出去（下一轮的输入）

前两轮评审把按钮的前置条件挖清楚了。这些结论必须留在这里，否则下一轮会重新发现一遍：

**（1）幂等不能靠事件 ID。** 两次真实点击是两个不同的飞书事件 ID；`listener.go` 的
`dedupCache` 是进程内 10 分钟缓存、重启即失效；多人同点同样是不同事件。事件 ID 只能挡
传输重投。

**（2）幂等也不能只靠"业务键 claim"。** 重试会调 `NextWorkflowAttempt`（递增计数器）。
若 Temporal 已启动成功、而写 `succeeded` 前崩溃，恢复时再调一次就会拿到新的 `-r{N+1}`、
起第二个 workflow 重复测试。**首次 claim 必须把产物固定下来**：`attempt`、
`target_workflow_id`、`claim_token`/`lease_until`。恢复只能用**同一个 workflow ID** 再调
`StartDeviceTest`，让 Temporal 去重；绝不能再次递增 attempt。`failed → pending` 必须是带
状态条件的 CAS（`UPDATE … WHERE state='failed' RETURNING`），`pending` 必须有 owner 与租约，
否则 claim 后崩溃会永久卡在"处理中"。

**（3）`Version` / `RuleVersion` 没有权威来源。** `artifacts` 与 `tasks` **都没有**这两列
（`schema.sql` 的 `version` 在 `clients` 表，是 agent 版本）。这不只是历史数据问题——
新写入的数据同样恢复不出来。而 `rerun` 指令今天就在丢它们（`executor.go:339` 只填
Project/Commit/PipelineID），于是对同一产物的重跑可能用**不同规则版本**重新判定，
而 `rule_version` 存在的全部目的就是让判定可回放。需要新建持久化模型（`workflow_runs`：
workflow_id / project / commit / pipeline / version / rule_version），定义写入点、唯一键与
历史行 fallback。**这实际就是 CLAUDE.md §11 声明却从未建过的 `workflows` 表**——
它已经绞了两轮设计（差距 #10 那轮的 `RecentRuns` 也是因为它缺失才退化成 workflow_id
字符串前缀匹配）。

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
history，worker 装配时改不了。按钮可用性应是 `Acts`/sender 的运行时配置，由 `NotifyCard`
handler 在发送前决定是否剥掉按钮；并且**本地注册 handler 不能证明飞书后台已订阅
`card.action.trigger`**，需要显式门禁（如 `FEISHU_CARD_ACTIONS_ENABLED`），WS 退出后也要
关掉交互能力。

**（8）副作用回调需要 fail-closed 输入约束**，且全部在 claim 之前完成：拒绝空身份、
未知 action、重复/超量 variants、非终态 task、过大 payload、错误的 app/tenant。
非白名单提示必须是**同步 callback toast**——不能用 `SendText`，那会发到固定的
`FEISHU_RECEIVE_ID`（可能是群），等于把一次未授权点击广播给所有人。

**（9）`audit_log` 表不存在。** CLAUDE.md §37 要求"所有操作落 audit_log"、§11 给了结构，
但 `schema.sql` 里没有这张表、全仓库无写入方。按钮那轮应建表并成为第一个写入方。

**（10）「忽略」的语义要写清**：它只记录"人看过并决定忽略"，不修改 `tasks.verdict`、
不影响任何后续策略；当下**没有任何消费方读取 human decision**。不写明的话，第一个用它的人
会以为按钮坏了。

建议下一轮的顺序：先做 `workflow_runs`（它已经绞了两轮设计，且不只服务按钮），再做按钮。

## 11. 后续（不在本轮，也不在按钮那轮）

- NL 翻译 `rerun` 待确认改卡片按钮
- 「隔离」按钮——等设备级信号源落地（差距 #10 §7）
- 通知里带日志/证据链接
- 卡片模板化（卡片 JSON 直接内联会随字段增多变长）
