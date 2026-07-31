# 飞书终态卡片按钮（重试 / 忽略）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在终态通知卡片上加两个卡片级按钮（重试失败变体 / 忽略），点击经飞书 WS 长连接回调，
以持久化 claim 保证"一次点击 = 一次动作"，结果异步写回卡片。

**Architecture:** 三段式。**同步段**只做无 I/O 校验并把点击写进 `card_action_inbox`（写不进去就返回
error 让飞书重投），**异步段**从 inbox claim → 只读 Resolver 解析 → 单事务接受动作 → 执行，
**三个 sweep** 负责崩溃恢复与卡片收敛。按钮由 `NotifyCard` **活动**注入，workflow 完全不参与，
因此不需要 Temporal 版本门。

**Tech Stack:** Go 1.22+、PostgreSQL 15、Temporal Go SDK、`larksuite/oapi-sdk-go/v3 v3.9.9`、
zerolog、`embedded-postgres`（测试）。

**权威 spec：** `docs/superpowers/specs/2026-07-31-feishu-card-actions-design.md`（v6，已批准）。
本计划中每处"为什么"都可回溯到该 spec；实施中若发现契约缺陷，按 CLAUDE.md 要求在代码注释里
标 `CONTRACT-ISSUE:` 并提出修改建议，**不要静默偏离**。

## Global Constraints

- Go 1.22+；错误用 wrapped errors（`%w`）；所有跨网络调用带 context 超时。
- 时间一律 UTC 存储。提交信息用英文，注释中文英文皆可；公共 API 必须有 godoc。
- 契约只加字段不删字段。
- **禁止给 `feishu.CardSender` 增加方法**——`webhookSender` 已实现 `SendCard`（`feishu.go:168`），
  扩接口会让它掉出 `CardSender`，`notify_card.go` 的类型断言随即失败，webhook 模式的展示卡片
  静默退化成纯文本。新能力一律走独立接口。
- **禁止修改 `decisions` 表**（含不得放宽 `task_id` 的 NOT NULL）。
- **禁止在 workflow 代码里给 `CardElement.Actions` 赋值**——workflow 产出的卡片序列化必须逐字节不变。
- **禁止用 `context.Background()` 测试需要 `activity.GetInfo` 的代码**，必须用 Temporal activity testsuite。
- 数值常量（spec §5.2、§6.1、§7.2、§8.1、§8.3）：
  - 租约 `120s`；sweep 轮询 `30s`；`reconcile_after` 延迟 `60s`
  - 同步段 deadline `2s`；回调 payload 上限 `4 KiB`；卡片总预算 `30 * 1024` 字节
  - `owner` 是每次 acquire 新生成的 **128 bit 随机 hex token**，绝不可复用
- **PostgreSQL 的 CHECK 立即生效且不可 `DEFERRABLE`**（已实证：`23514` / `0A000`）。
  任何"先插默认值再 UPDATE 补齐"的写法都会失败——所有受 CHECK 约束的行必须一次性写入完整值。
- 所有 completion（`Complete*`、action finalize、卡片 completion）必须**同时**校验
  `owner = $token AND lease_expires_at > now()`；卡片 completion 还要加 `desired_revision = $r`。
- 失去持有权的写方**零写入**，只记日志。

---

## 文件结构

**新建：**

| 文件 | 职责 |
|---|---|
| `runtime/internal/rerun/resolver.go` | 只读 `Resolver`：`ResolveFailureRun` / `ResolveRetry` / `RejectReason` |
| `runtime/internal/rerun/resolver_test.go` | 两种模式差异、拒绝原因字段 |
| `runtime/internal/store/card_actions.go` | 五张表的 Go 类型 + MemStore 实现 |
| `runtime/internal/store/postgres_card_actions.go` | PGStore 实现（含三个单事务） |
| `runtime/internal/store/migration_card_actions_test.go` | 迁移幂等 / fresh 与 upgraded 一致 |
| `runtime/internal/cardaction/handler.go` | 同步回调 handler（校验 + inbox + toast） |
| `runtime/internal/cardaction/consume.go` | 异步消费（claim → resolve → complete → 执行） |
| `runtime/internal/cardaction/sweep.go` | 三个 sweep |
| `runtime/internal/cardaction/render.go` | 从快照渲染卡片（注入按钮 / 状态文本） |
| `runtime/internal/cardaction/readiness.go` | 五项合取 + WS 生命周期原子布尔 |
| `deploy/postgres/migrations/2026-08-01-card-actions.sql` | 五张表的独立迁移 |

**修改：**

| 文件 | 改动 |
|---|---|
| `runtime/internal/workflow/devicetest.go` | `CardConfig.UpdateMulti`、`CardElement.Actions`、`CardButton`、`CardActionValue` |
| `runtime/internal/workflow/devicetest_test.go` | 递归键断言放宽到新封闭集合；序列化不变性断言 |
| `runtime/internal/store/schema.sql` | 五张新表 |
| `runtime/internal/store/pgtest_test.go` | TRUNCATE 清单 |
| `runtime/internal/store/conformance_test.go` | 新增 conformance 用例 |
| `runtime/internal/feishu/feishu.go` | 新增 `CardUpdater` 接口 + `appSender.PatchCard` |
| `runtime/internal/feishucmd/executor.go` | `rerun` 改调共享 Resolver |
| `runtime/internal/feishucmd/listener.go` | 注册 `OnP2CardActionTrigger` + 五个生命周期钩子 |
| `runtime/internal/activity/notify_card.go` | 快照落盘、readiness 判定、按钮注入 |
| `runtime/cmd/worker/main.go` | 装配 `cardaction`，启动三个 sweep |
| `CLAUDE.md` / `docs/device-test-sequence.md` / `deploy/README.md` | 文档同步 |

---

## Task 1: 卡片 DTO 的第一段变更

**这个 task 单独可部署**，随 `workflow-runs` 分支首次上线（spec §11 第一段）。它只加字段、不加行为，
workflow 产出的卡片序列化**逐字节不变**——这是"第二段不需要 Temporal 版本门"的全部依据。

**Files:**
- Modify: `runtime/internal/workflow/devicetest.go`（`CardConfig` 约 776 行、`CardElement` 约 780 行）
- Test: `runtime/internal/workflow/devicetest_test.go`

**Interfaces:**
- Produces: `CardConfig{WideScreenMode, UpdateMulti bool}`、
  `CardElement{Tag string, Text *CardText, Actions []CardButton}`、
  `CardButton{Tag string, Text CardText, Type string, Value CardActionValue}`、
  `CardActionValue{Action, SourceWorkflowID string}`

- [ ] **Step 1: 写序列化不变性测试（最关键的一条）**

在 `devicetest_test.go` 追加：

```go
// TestWorkflowCardSerializationUnchangedByActionField 锁住"第二段无需 Temporal 版本门"的前提:
// workflow 产出的卡片里 Actions 恒为 nil,omitempty 使其不出现在 JSON 中,
// 因此 NotifyCard 的 activity input 逐字节不变,旧 history 重放不会失配。
func TestWorkflowCardSerializationUnchangedByActionField(t *testing.T) {
	in := DeviceTestInput{Project: "grp/p", Commit: "abcd1234", PipelineID: 42, Version: "1.2.3"}
	out := &DeviceTestOutput{Tasks: []TaskSummary{
		{Variant: "v1", Verdict: "TEST_FAILED", Category: "CODE", Attempt: 1, Reason: "boom"},
		{Variant: "v2", Verdict: "PASSED", Attempt: 1, CasesTotal: 3, DurationSec: 1.5},
	}}
	card := buildNotificationCard(in, out)

	// 1) 每个元素的 Actions 必须为 nil —— workflow 永不构造交互元素
	for i, el := range card.Elements {
		if el.Actions != nil {
			t.Fatalf("elements[%d].Actions = %#v, workflow 侧必须恒为 nil", i, el.Actions)
		}
	}
	// 2) 序列化后不得出现 actions 键
	raw, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(raw, []byte(`"actions"`)) {
		t.Fatalf("workflow 产出的卡片不应含 actions 键: %s", raw)
	}
}

// TestCardConfigCarriesUpdateMulti:飞书 PATCH 要求原卡带 config.update_multi=true,
// 否则消息不可更新,整个按钮轮的权威反馈层无从谈起。
func TestCardConfigCarriesUpdateMulti(t *testing.T) {
	card := buildNotificationCard(DeviceTestInput{Project: "p", Commit: "c", PipelineID: 1, Version: "v"},
		&DeviceTestOutput{Tasks: []TaskSummary{{Variant: "v1", Verdict: "PASSED"}}})
	if !card.Config.UpdateMulti {
		t.Fatal("config.update_multi 必须为 true")
	}
	raw, _ := json.Marshal(card)
	if !bytes.Contains(raw, []byte(`"update_multi":true`)) {
		t.Fatalf("序列化后缺 update_multi: %s", raw)
	}
}
```

确保测试文件已 import `bytes`、`encoding/json`。

- [ ] **Step 2: 运行，确认失败**

Run: `cd runtime && go test ./internal/workflow/ -run 'TestWorkflowCardSerializationUnchangedByActionField|TestCardConfigCarriesUpdateMulti' -v`
Expected: 编译失败——`card.Config.UpdateMulti` 与 `el.Actions` 未定义。

- [ ] **Step 3: 加字段**

`devicetest.go`：

```go
type CardConfig struct {
	WideScreenMode bool `json:"wide_screen_mode"`
	// UpdateMulti 恒为 true。飞书 PATCH /open-apis/im/v1/messages/:message_id
	// 要求原卡带 config.update_multi=true,否则消息不可更新(设计 §7.2)。
	UpdateMulti bool `json:"update_multi"`
}

// CardElement 是封闭的 tagged union,三种形态互斥:
//   tag=div    → Text 非空, Actions 为 nil
//   tag=hr     → 两者皆 nil
//   tag=action → Text 为 nil, Actions 非空
//
// **Actions 只由 NotifyCard 活动构造,workflow 永不赋值**——omitempty 使
// workflow 产出的卡片序列化逐字节不变,这是第二段部署无需 Temporal 版本门的依据(设计 §7.1)。
type CardElement struct {
	Tag     string       `json:"tag"`
	Text    *CardText    `json:"text,omitempty"`
	Actions []CardButton `json:"actions,omitempty"`
}

// CardButton 没有 behaviors、没有 url、没有 multi_url——按钮只能回调,
// 不可能变成跳转或表单(设计 §7.2)。
type CardButton struct {
	Tag   string          `json:"tag"`   // 恒为 "button"
	Text  CardText        `json:"text"`  // 恒为 plain_text
	Type  string          `json:"type"`  // primary | default
	Value CardActionValue `json:"value"`
}

// CardActionValue 序列化后恰好两个键。用固定字段而非 map[string]string:
// map 不是封闭 DTO,"恰好两个键"的断言无从谈起(设计 §7.2)。
type CardActionValue struct {
	Action           string `json:"action"`             // retry | ignore
	SourceWorkflowID string `json:"source_workflow_id"`
}
```

在 `buildNotificationCard` 里把 `Config` 构造改为
`CardConfig{WideScreenMode: true, UpdateMulti: true}`（约 devicetest.go:870 处）。

- [ ] **Step 4: 运行，确认通过**

Run: `cd runtime && go test ./internal/workflow/ -run 'TestWorkflowCardSerializationUnchangedByActionField|TestCardConfigCarriesUpdateMulti' -v`
Expected: PASS

- [ ] **Step 5: 放宽递归键断言（不是删除）**

`devicetest_test.go:950` 附近的允许键集合与 `:1007` 的 config 键集合要同步扩展。
把 config 键集合改为 `map[string]bool{"wide_screen_mode": true, "update_multi": true}`，
并要求 `update_multi` 是 bool；顶层允许键补 `"actions"`。
**反例用例必须补强而非削弱**——在既有反例表里追加：

```go
{"带 behaviors", `{"config":{"wide_screen_mode":true,"update_multi":true},"header":{"title":{"tag":"plain_text","content":"h"},"template":"green"},"elements":[{"tag":"div","text":{"tag":"plain_text","content":"x"},"behaviors":[]}]}`},
{"按钮带 url", `{"config":{"wide_screen_mode":true,"update_multi":true},"header":{"title":{"tag":"plain_text","content":"h"},"template":"green"},"elements":[{"tag":"action","actions":[{"tag":"button","text":{"tag":"plain_text","content":"b"},"type":"primary","url":"http://x","value":{"action":"retry","source_workflow_id":"w"}}]}]}`},
{"按钮带 multi_url", `{"config":{"wide_screen_mode":true,"update_multi":true},"header":{"title":{"tag":"plain_text","content":"h"},"template":"green"},"elements":[{"tag":"action","actions":[{"tag":"button","text":{"tag":"plain_text","content":"b"},"type":"primary","multi_url":{},"value":{"action":"retry","source_workflow_id":"w"}}]}]}`},
{"value 含第三个键", `{"config":{"wide_screen_mode":true,"update_multi":true},"header":{"title":{"tag":"plain_text","content":"h"},"template":"green"},"elements":[{"tag":"action","actions":[{"tag":"button","text":{"tag":"plain_text","content":"b"},"type":"primary","value":{"action":"retry","source_workflow_id":"w","extra":"x"}}]}]}`},
{"value 含 variant", `{"config":{"wide_screen_mode":true,"update_multi":true},"header":{"title":{"tag":"plain_text","content":"h"},"template":"green"},"elements":[{"tag":"action","actions":[{"tag":"button","text":{"tag":"plain_text","content":"b"},"type":"primary","value":{"action":"retry","source_workflow_id":"w","variant":"v1"}}]}]}`},
{"div 同时带 actions", `{"config":{"wide_screen_mode":true,"update_multi":true},"header":{"title":{"tag":"plain_text","content":"h"},"template":"green"},"elements":[{"tag":"div","text":{"tag":"plain_text","content":"x"},"actions":[]}]}`},
```

在键检查函数里加规则：`tag=div` 时 `actions` 必须缺席；`tag=action` 时 `text` 必须缺席且
`actions` 非空；button 的 `value` 键集合恰好 `{action, source_workflow_id}`。

- [ ] **Step 6: 全量回归**

Run: `cd runtime && go vet ./... && go test ./internal/workflow/ -count=1`
Expected: 全绿，**含 `TestReplayPreNotifyCardHistory`**（它走 DefaultVersion 文本分支，不受影响）。

- [ ] **Step 7: 提交**

```bash
git add runtime/internal/workflow/devicetest.go runtime/internal/workflow/devicetest_test.go
git commit -m "feat(card): declare update_multi and the action element shape

Stage one of the button rollout: config gains update_multi, which Feishu
requires before a card message can be patched at all, and CardElement
gains an omitempty Actions field so the activity has somewhere to put
buttons later.

The workflow never sets Actions, so its cards serialize byte for byte as
before. That is asserted, not assumed -- it is the whole reason stage two
needs no Temporal version gate."
```

---

## Task 2: 抽出共享 RerunResolver

**Files:**
- Create: `runtime/internal/rerun/resolver.go`, `runtime/internal/rerun/resolver_test.go`
- Modify: `runtime/internal/feishucmd/executor.go`（`rerun` 方法，约 322-434 行）

**Interfaces:**
- Consumes: `store.WorkflowRun`、`store.Artifact`、`wf.DeviceTestOutput`、`wf.PackageRef`
- Produces:
  ```go
  type Resolver struct {
      Store   Store          // GetWorkflowRun / ListArtifacts
      Starter WorkflowLookup // WorkflowClosed / WorkflowResult
  }
  func (r *Resolver) ResolveFailureRun(ctx context.Context, workflowID string) (*FailureRun, error)
  func (r *Resolver) ResolveRetry(ctx context.Context, workflowID, variant string) (*Resolution, error)
  type FailureRun struct { Run store.WorkflowRun; Targets []string }
  type Resolution struct { Run store.WorkflowRun; Targets []string; Packages []wf.PackageRef; Scope string }
  type RejectReason struct { Code, WorkflowID, Variant string; Count int }
  func (e *RejectReason) Error() string
  ```
  `Code` 取值：`NotAuthoritative` / `StillRunning` / `ResultUnreadable` / `NoFailedVariants` /
  `VariantNotMember` / `ArtifactMissing`

- [ ] **Step 1: 写两层差异的失败测试**

`runtime/internal/rerun/resolver_test.go`：

```go
// TestExplicitVariantSkipsWorkflowResult:显式 variant 是用户的明确选择,
// 只校验成员关系与 artifact,不读 Temporal output——因此允许重跑 PASSED/SKIPPED。
func TestExplicitVariantSkipsWorkflowResult(t *testing.T) {
	st := newFakeStore(t, run("wf1", "grp/p", "abcd1234", 42, "v1", "v2"), art("v1"), art("v2"))
	lookup := &fakeLookup{closed: true, resultErr: errors.New("must not be called")}
	r := &Resolver{Store: st, Starter: lookup}

	res, err := r.ResolveRetry(ctx, "wf1", "v1")
	if err != nil {
		t.Fatalf("ResolveRetry: %v", err)
	}
	if lookup.resultCalls != 0 {
		t.Fatalf("WorkflowResult 被调用 %d 次,显式模式必须零调用", lookup.resultCalls)
	}
	if !reflect.DeepEqual(res.Targets, []string{"v1"}) || res.Scope != "v1" {
		t.Fatalf("targets=%v scope=%q", res.Targets, res.Scope)
	}
}

// TestEmptyVariantFiltersDedupesSorts:空 variant 模式从 output 取失败集合,
// 忽略空 Variant、去重、按字典序排序。
func TestEmptyVariantFiltersDedupesSorts(t *testing.T) {
	st := newFakeStore(t, run("wf1", "grp/p", "abcd1234", 42, "v1", "v2"), art("v1"), art("v2"))
	lookup := &fakeLookup{closed: true, result: &wf.DeviceTestOutput{Tasks: []wf.TaskSummary{
		{Variant: "v2", Verdict: "TEST_FAILED"},
		{Variant: "", Verdict: "INFRA_ERROR"},   // 空 Variant 必须忽略
		{Variant: "v2", Verdict: "INFRA_ERROR"}, // 重复必须去重
		{Variant: "v1", Verdict: "INFRA_ERROR"},
		{Variant: "v3", Verdict: "PASSED"},      // 排除
		{Variant: "v4", Verdict: wf.VerdictSkipped},
	}}}
	r := &Resolver{Store: st, Starter: lookup}

	res, err := r.ResolveRetry(ctx, "wf1", "")
	if err != nil {
		t.Fatalf("ResolveRetry: %v", err)
	}
	if !reflect.DeepEqual(res.Targets, []string{"v1", "v2"}) {
		t.Fatalf("targets = %v, want [v1 v2]", res.Targets)
	}
}

// TestArtifactMissingCarriesCount:文本 rerun 的既有文案
// "变体 %s 的 artifact 数量为 %d，要求恰好 1 个" 需要 Count,
// 单一枚举值复现不了它。
func TestArtifactMissingCarriesCount(t *testing.T) {
	st := newFakeStore(t, run("wf1", "grp/p", "abcd1234", 42, "v1"))  // 无 artifact
	r := &Resolver{Store: st, Starter: &fakeLookup{closed: true}}

	_, err := r.ResolveRetry(ctx, "wf1", "v1")
	var reason *RejectReason
	if !errors.As(err, &reason) {
		t.Fatalf("err = %v, want *RejectReason", err)
	}
	if reason.Code != "ArtifactMissing" || reason.Variant != "v1" || reason.Count != 0 {
		t.Fatalf("reason = %#v", reason)
	}
}

// TestResolveFailureRunIgnoresArtifacts:ignore 是纯记录动作,
// artifact 全缺时它必须仍然成功——与 retry 解耦(设计 §4.0)。
func TestResolveFailureRunIgnoresArtifacts(t *testing.T) {
	st := newFakeStore(t, run("wf1", "grp/p", "abcd1234", 42, "v1"))  // 无 artifact
	lookup := &fakeLookup{closed: true, result: &wf.DeviceTestOutput{
		Tasks: []wf.TaskSummary{{Variant: "v1", Verdict: "TEST_FAILED"}}}}
	r := &Resolver{Store: st, Starter: lookup}

	fr, err := r.ResolveFailureRun(ctx, "wf1")
	if err != nil {
		t.Fatalf("ResolveFailureRun 不应因缺 artifact 失败: %v", err)
	}
	if !reflect.DeepEqual(fr.Targets, []string{"v1"}) {
		t.Fatalf("targets = %v", fr.Targets)
	}
	// 同一场景下 retry 必须被拒
	if _, err := r.ResolveRetry(ctx, "wf1", ""); err == nil {
		t.Fatal("ResolveRetry 缺 artifact 时必须失败")
	}
}
```

同文件写 `fakeStore` / `fakeLookup` / `run()` / `art()` helper。

- [ ] **Step 2: 运行，确认失败**

Run: `cd runtime && go test ./internal/rerun/ -v`
Expected: 包不存在 / 编译失败。

- [ ] **Step 3: 实现 Resolver**

`resolver.go` 骨架（把 `executor.go:340-410` 的逻辑原样搬过来，**不改语义**）：

```go
// Package rerun 是文本 rerun 指令与卡片重试按钮共用的只读解析层。
// 抽出来是为了让两个入口共享一份业务语义:复制必然漂移,而"文本 rerun 回复逐字不变"
// 是本轮的验收项(设计 §4)。
package rerun

type RejectReason struct {
	Code       string
	WorkflowID string
	Variant    string
	Count      int
}

func (e *RejectReason) Error() string {
	return fmt.Sprintf("rerun rejected: %s (workflow=%s variant=%s count=%d)",
		e.Code, e.WorkflowID, e.Variant, e.Count)
}

// ResolveFailureRun 只做:权威 run + 已关闭 + 失败 summary 集合。
// ignore 只需要这一层——它是纯记录动作,不应因 artifact 缺失而失败。
func (r *Resolver) ResolveFailureRun(ctx context.Context, workflowID string) (*FailureRun, error) {
	run, err := r.Store.GetWorkflowRun(ctx, workflowID)
	if errors.Is(err, store.ErrWorkflowRunNotFound) {
		return nil, &RejectReason{Code: "NotAuthoritative", WorkflowID: workflowID}
	}
	if err != nil {
		return nil, fmt.Errorf("get workflow run %s: %w", workflowID, err)
	}
	closed, err := r.Starter.WorkflowClosed(ctx, workflowID)
	if err != nil {
		return nil, &RejectReason{Code: "ResultUnreadable", WorkflowID: workflowID}
	}
	if !closed {
		return nil, &RejectReason{Code: "StillRunning", WorkflowID: workflowID}
	}
	out, err := r.Starter.WorkflowResult(ctx, workflowID)
	if err != nil {
		return nil, &RejectReason{Code: "ResultUnreadable", WorkflowID: workflowID}
	}
	seen := map[string]struct{}{}
	for _, s := range out.Tasks {
		if s.Variant == "" || s.Verdict == string(rules.VerdictPassed) || s.Verdict == wf.VerdictSkipped {
			continue
		}
		seen[s.Variant] = struct{}{}
	}
	targets := make([]string, 0, len(seen))
	for v := range seen {
		targets = append(targets, v)
	}
	sort.Strings(targets)
	if len(targets) == 0 {
		return nil, &RejectReason{Code: "NoFailedVariants", WorkflowID: workflowID}
	}
	return &FailureRun{Run: *run, Targets: targets}, nil
}
```

`ResolveRetry` 分两条路：`variant != ""` 时**不调 `WorkflowResult`**（直接
`GetWorkflowRun` + `WorkflowClosed` + 成员校验），`variant == ""` 时调用
`ResolveFailureRun`；两条路最后都做 artifact 四元组解析（每个目标恰好命中一行，
否则 `ArtifactMissing{Variant, Count}`）。

- [ ] **Step 4: 运行，确认通过**

Run: `cd runtime && go test ./internal/rerun/ -v`
Expected: PASS

- [ ] **Step 5: 把 executor.rerun 改接 Resolver**

`executor.go` 的 `rerun` 保留全部**外部行为**：旧语法迁移提示、参数校验、
所有回复文案逐字不变。内部把"取 run → 查关闭 → 取失败集 → 解析 artifact"
替换成一次 `Resolver.ResolveRetry(ctx, workflowID, variant)`，
再按 `RejectReason.Code` 渲染既有文案：

```go
var reason *rerun.RejectReason
if errors.As(err, &reason) {
	switch reason.Code {
	case "NotAuthoritative":
		return fmt.Sprintf("查无权威 workflow 运行记录: %s", reason.WorkflowID), nil
	case "StillRunning":
		return fmt.Sprintf("workflow 尚未结束: %s", reason.WorkflowID), nil
	case "ResultUnreadable":
		return fmt.Sprintf("读取 workflow 结果失败: %s", reason.WorkflowID), nil
	case "NoFailedVariants":
		return fmt.Sprintf("workflow 没有失败变体: %s", reason.WorkflowID), nil
	case "VariantNotMember":
		return fmt.Sprintf("变体 %s 不属于源 workflow %s", reason.Variant, reason.WorkflowID), nil
	case "ArtifactMissing":
		return fmt.Sprintf("变体 %s 的 artifact 数量为 %d，要求恰好 1 个", reason.Variant, reason.Count), nil
	}
}
```

- [ ] **Step 6: 验证既有测试一行不改即通过**

Run: `cd runtime && go test ./internal/feishucmd/ -count=1 -v -run TestRerun`
Expected: **既有全部 rerun 用例 PASS，且 `executor_test.go` 未被修改**
（用 `git diff --stat runtime/internal/feishucmd/executor_test.go` 确认为空）。
这是语义共用而非复制的判据。

- [ ] **Step 7: 提交**

```bash
git add runtime/internal/rerun runtime/internal/feishucmd/executor.go
git commit -m "refactor(rerun): share the resolver between text and buttons

Two entry points computing the same failure set from two copies would
drift, and the text command's replies are a byte-for-byte acceptance
item, so the resolver is extracted rather than reimplemented. Its test
file is untouched, which is the check that this is a share and not a
rewrite.

Split in two layers on purpose: ignore is a pure record and has no
business failing because an artifact is missing, so it stops at the
failure-run layer and never reaches artifact resolution."
```

---

## Task 3: schema 与迁移（五张表）

**Files:**
- Modify: `runtime/internal/store/schema.sql`
- Create: `deploy/postgres/migrations/2026-08-01-card-actions.sql`
- Create: `runtime/internal/store/migration_card_actions_test.go`
- Modify: `runtime/internal/store/pgtest_test.go:86`（TRUNCATE 清单）

**Interfaces:**
- Produces: 五张表 `card_action_inbox` / `card_actions` / `card_action_messages` /
  `card_action_snapshots` / `audit_log`，DDL 逐字取自 spec §3.1–§3.5。

  **一处对 spec 的加强**：`audit_log` 增加 `CHECK (actor <> '' AND action <> '')`。
  spec 只写了 `NOT NULL`，但空串同样是无归属的审计行；这条 CHECK 同时给 Task 4 的
  "审计失败整笔回滚"用例提供了不需要生产代码开测试后门的故障注入手段。

- [ ] **Step 1: 写迁移测试**

`migration_card_actions_test.go`，照抄 `migration_workflow_runs_test.go` 的隔离库骨架：

```go
// TestCardActionsMigrationIsIdempotent:迁移连跑两次结果相同。
func TestCardActionsMigrationIsIdempotent(t *testing.T) {
	s := openIsolatedMigrationPG(t)
	applyFile(t, s, "../../../deploy/postgres/migrations/2026-08-01-card-actions.sql")
	first := captureCardActionsShape(t, s)
	applyFile(t, s, "../../../deploy/postgres/migrations/2026-08-01-card-actions.sql")
	if !reflect.DeepEqual(first, captureCardActionsShape(t, s)) {
		t.Fatal("迁移不幂等")
	}
}

// TestCardActionsMigrationMatchesFreshSchema:fresh schema.sql 与 upgraded 库
// 的最终约束必须一致,否则新库与老库行为不同。
func TestCardActionsMigrationMatchesFreshSchema(t *testing.T) {
	fresh := openIsolatedMigrationPG(t)
	applyFile(t, fresh, "schema.sql")

	upgraded := openIsolatedMigrationPG(t)
	applySchemaWithoutCardActions(t, upgraded) // 剥掉五张表的建表语句
	applyFile(t, upgraded, "../../../deploy/postgres/migrations/2026-08-01-card-actions.sql")

	if !reflect.DeepEqual(captureCardActionsShape(t, fresh), captureCardActionsShape(t, upgraded)) {
		t.Fatal("fresh 与 upgraded 的约束不一致")
	}
}

// TestCardActionsMigrationRequiresWorkflowRuns:card_actions 的 FK 依赖 workflow_runs,
// 未完成上一轮生产迁移时必须明确失败,而不是静默建出一张没有 FK 的表。
func TestCardActionsMigrationRequiresWorkflowRuns(t *testing.T) {
	s := openIsolatedMigrationPG(t)  // 空库,没有 workflow_runs
	err := applyFileErr(s, "../../../deploy/postgres/migrations/2026-08-01-card-actions.sql")
	if err == nil {
		t.Fatal("缺 workflow_runs 时迁移必须失败")
	}
	if !strings.Contains(err.Error(), "workflow_runs") {
		t.Fatalf("错误信息应指明缺 workflow_runs: %v", err)
	}
}
```

- [ ] **Step 2: 运行，确认失败**

Run: `cd runtime && go test ./internal/store/ -run TestCardActionsMigration -v`
Expected: FAIL——迁移文件不存在。

- [ ] **Step 3: 写迁移文件**

`deploy/postgres/migrations/2026-08-01-card-actions.sql`，`BEGIN; ... COMMIT;` 包裹，
开头加前置断言：

```sql
BEGIN;

-- 前置检查:本轮的 card_actions.workflow_id 外键依赖 workflow_runs。
-- 上一轮生产迁移未完成时必须明确失败,而不是静默建出一张缺 FK 的表。
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_tables WHERE tablename = 'workflow_runs') THEN
        RAISE EXCEPTION 'card-actions migration requires table workflow_runs; run the workflow_runs migration first';
    END IF;
END $$;

-- 五张表的 DDL 逐字取自 spec §3.1-§3.5(含全部 CHECK 与部分索引)
CREATE TABLE IF NOT EXISTS card_action_inbox ( ... );
CREATE TABLE IF NOT EXISTS card_actions ( ... );
CREATE TABLE IF NOT EXISTS card_action_messages ( ... );
CREATE TABLE IF NOT EXISTS card_action_snapshots ( ... );
CREATE TABLE IF NOT EXISTS audit_log ( ... );
-- 五个部分索引

COMMIT;
```

同样的 DDL 写进 `schema.sql`（不含前置断言——fresh 库里 `workflow_runs` 就在同一文件的上方）。

- [ ] **Step 4: 更新 TRUNCATE 清单**

`pgtest_test.go:86` 的 TRUNCATE 追加五张表：
`..., command_translations, card_action_inbox, card_actions, card_action_messages, card_action_snapshots, audit_log CASCADE`

- [ ] **Step 5: 运行，确认通过**

Run: `cd runtime && go test ./internal/store/ -run TestCardActionsMigration -count=1 -v`
Expected: PASS（三条全绿）

- [ ] **Step 6: 提交**

```bash
git add runtime/internal/store/schema.sql runtime/internal/store/pgtest_test.go \
        runtime/internal/store/migration_card_actions_test.go \
        deploy/postgres/migrations/2026-08-01-card-actions.sql
git commit -m "feat(store): add card action tables and migration

Five tables, with every CHECK from the spec carried into the DDL rather
than left to application code. The constraints are load-bearing: because
PostgreSQL evaluates CHECK per statement and refuses DEFERRABLE, a retry
row cannot exist without its pinned attempt, target and input, so no
implementation can insert a placeholder and fill it in later.

The migration refuses to run without workflow_runs instead of quietly
creating a table whose foreign key it cannot satisfy."
```

---

## Task 4: store —— card_action_inbox

**Files:**
- Create: `runtime/internal/store/card_actions.go`（本 task 只写 inbox 部分）
- Create: `runtime/internal/store/postgres_card_actions.go`（同上）
- Modify: `runtime/internal/store/conformance_test.go`

**Interfaces:**
- Produces:
  ```go
  type InboxRow struct {
      EventID, Disposition, AckToast, Action, WorkflowID, ActorOpenID, OpenMessageID string
      PayloadDigest, State, Owner, LastError string
      LeaseExpiresAt, ProcessedAt *time.Time
      Attempts int
  }
  type AuditRow struct {
      Actor, Action, Target, PayloadDigest string
      CardActionWorkflowID string // 空 = FK 写 NULL
      InboxEventID         string
  }
  // PutInbox 写入一次点击。rejected 行必须在同一条 INSERT 里写成终态。
  // 返回 (existing *InboxRow, inserted bool, err error):inserted=false 时 existing 非 nil。
  func (s *MemStore) PutInbox(ctx context.Context, row InboxRow, auditOnReject *AuditRow) (*InboxRow, bool, error)
  func (s *MemStore) GetInbox(ctx context.Context, eventID string) (*InboxRow, error)
  func (s *MemStore) ClaimInbox(ctx context.Context, eventID, token string, lease time.Duration) (*InboxRow, error)
  ```
  错误值：`ErrInboxNotClaimable`、`ErrInboxNotFound`，均供 `errors.Is` 判定。

- [ ] **Step 1: 写 conformance 测试**

在 `conformance_test.go` 的 `runConformance` 里追加：

```go
t.Run("RejectedInboxIsTerminalOnInsert", func(t *testing.T) {
	s := newStore(t)
	// rejected 行必须首次 INSERT 即为 processed。先插 received 再 UPDATE 会撞 23514。
	row := InboxRow{EventID: "e1", Disposition: "rejected", AckToast: "无权限",
		Action: "retry", WorkflowID: "wf1", ActorOpenID: "ou_x", State: "processed"}
	audit := &AuditRow{Actor: "feishu:ou_x", Action: "card.retry.rejected.unauthorized",
		Target: "wf1", InboxEventID: "e1"}
	if _, inserted, err := s.PutInbox(ctx, row, audit); err != nil || !inserted {
		t.Fatalf("PutInbox = (%v, %v)", inserted, err)
	}
	got := mustGetInbox(t, s, "e1")
	if got.State != "processed" || got.ProcessedAt == nil {
		t.Fatalf("rejected 行必须落库即终态: %#v", got)
	}
})

t.Run("RejectedInboxRollsBackWhenAuditFails", func(t *testing.T) {
	s := newStore(t)
	// 审计写不进去时整笔回滚:否则该 event 被永久当作"已处理",审计里却查无此人。
	//
	// 故障注入用**非法审计行**(actor 为空,撞 audit_log 的 CHECK),
	// 而不是在生产类型上加 ForceFailForTest 之类的测试开关——
	// 那种接缝会永久留在生产结构体里,而且 MemStore 与 PGStore 各写一份必然漂移。
	bad := &AuditRow{Actor: "", Action: "card.retry.rejected.unauthorized",
		Target: "wf1", InboxEventID: "e1"}
	if _, _, err := s.PutInbox(ctx, InboxRow{EventID: "e1", Disposition: "rejected",
		AckToast: "x", State: "processed"}, bad); err == nil {
		t.Fatal("审计失败时 PutInbox 必须返回错误")
	}
	if _, err := s.GetInbox(ctx, "e1"); !errors.Is(err, ErrInboxNotFound) {
		t.Fatalf("整笔必须回滚,inbox 不得留行: %v", err)
	}
})

t.Run("DuplicateRejectedEventReplaysToast", func(t *testing.T) {
	s := newStore(t)
	row := InboxRow{EventID: "e1", Disposition: "rejected", AckToast: "按钮已停用",
		Action: "retry", WorkflowID: "wf1", State: "processed"}
	audit := &AuditRow{Actor: "feishu:ou_x", Action: "card.retry.rejected.disabled",
		Target: "wf1", InboxEventID: "e1"}
	for i := 0; i < 3; i++ {
		existing, inserted, err := s.PutInbox(ctx, row, audit)
		if err != nil {
			t.Fatalf("第 %d 次: %v", i, err)
		}
		if i == 0 && !inserted {
			t.Fatal("首次必须插入")
		}
		if i > 0 {
			if inserted {
				t.Fatalf("第 %d 次不应插入", i)
			}
			if existing.AckToast != "按钮已停用" {
				t.Fatalf("toast 必须原样重放,got %q", existing.AckToast)
			}
		}
	}
	if n := countAudit(t, s, "e1"); n != 1 {
		t.Fatalf("审计行数 = %d, want 1", n)
	}
})

t.Run("ClaimInboxTakesLeaseOnce", func(t *testing.T) {
	s := newStore(t)
	_, _, _ = s.PutInbox(ctx, InboxRow{EventID: "e1", Disposition: "accepted",
		AckToast: "已收到，正在处理", Action: "retry", WorkflowID: "wf1",
		ActorOpenID: "ou_x", OpenMessageID: "om_1", State: "received"}, nil)

	got, err := s.ClaimInbox(ctx, "e1", "tokA", 120*time.Second)
	if err != nil || got == nil {
		t.Fatalf("首次 claim 失败: %v", err)
	}
	// 租约未过期时第二个 worker 抢不到
	if _, err := s.ClaimInbox(ctx, "e1", "tokB", 120*time.Second); !errors.Is(err, ErrInboxNotClaimable) {
		t.Fatalf("租约有效期内第二次 claim 应失败, got %v", err)
	}
})
```

- [ ] **Step 2: 运行，确认失败**

Run: `cd runtime && go test ./internal/store/ -run 'Conformance/(RejectedInbox|DuplicateRejected|ClaimInbox)' -v`
Expected: 编译失败。

- [ ] **Step 3: 实现 MemStore 与 PGStore**

PG 侧 `PutInbox` 单事务：

```go
func (s *PGStore) PutInbox(ctx context.Context, row InboxRow, auditOnReject *AuditRow) (*InboxRow, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("put inbox %s: begin: %w", row.EventID, err)
	}
	defer func() { _ = tx.Rollback() }()

	// rejected 分支在这一条 INSERT 里就写成终态:CHECK 立即生效,
	// 先插 received 再 UPDATE 会当场 23514(设计 §6.1)。
	var processedAt any
	if row.State == "processed" {
		processedAt = time.Now().UTC()
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO card_action_inbox
			(event_id, disposition, ack_toast, action, workflow_id, actor_open_id,
			 open_message_id, payload_digest, state, processed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (event_id) DO NOTHING`, ...)
	...
	if inserted && auditOnReject != nil {
		if err := insertAuditTx(ctx, tx, *auditOnReject); err != nil {
			return nil, false, fmt.Errorf("put inbox %s: audit: %w", row.EventID, err)
		}
	}
	// 未插入 → 读出既有行返回,供调用方重放 ack_toast
	...
	return existing, inserted, tx.Commit()
}
```

`ClaimInbox`：

```go
UPDATE card_action_inbox
   SET owner=$2, lease_expires_at=now()+$3::interval, attempts=attempts+1
 WHERE event_id=$1 AND state='received'
   AND (lease_expires_at IS NULL OR lease_expires_at < now())
RETURNING ...
```
零行 → `ErrInboxNotClaimable`。

- [ ] **Step 4: 运行，确认通过**

Run: `cd runtime && go test ./internal/store/ -run 'Conformance/(RejectedInbox|DuplicateRejected|ClaimInbox)' -count=1 -v`
Expected: MemStore 与 PGStore 两套 conformance 全 PASS。

- [ ] **Step 5: 提交**

```bash
git add runtime/internal/store/card_actions.go runtime/internal/store/postgres_card_actions.go \
        runtime/internal/store/conformance_test.go
git commit -m "feat(store): persist card action clicks before acknowledging

Feishu stops redelivering once the handler answers successfully, so the
click has to be on disk before that answer goes out. The inbox row is
keyed by event ID, which also gives dedup across processes and across
restarts that the in-process cache never could, and lets a redelivery
replay the identical toast.

Rejections are written terminal in their first INSERT, since CHECK fires
per statement and a two-step write would fail on the spot. Audit shares
that transaction: an event marked processed with nothing in the audit log
is worse than a failed write."
```

---

## Task 5: store —— 接受动作的三个单事务

**这是本轮最关键的 task。** 三条 fencing 规则全部落在这里，调用方绕不过去。

**Files:**
- Modify: `runtime/internal/store/card_actions.go`、`postgres_card_actions.go`、`conformance_test.go`

**Interfaces:**
- Produces:
  ```go
  type CardAction struct {
      WorkflowID, Action, ActorOpenID, State, Owner, TargetWorkflowID, LastError string
      LeaseExpiresAt *time.Time
      Attempt, Revision int
      TargetInput []byte // canonical JSON of wf.DeviceTestInput
  }
  type AcceptRequest struct {
      EventID, Token, WorkflowID, Action, ActorOpenID, OpenMessageID, PayloadDigest string
      ActionToken string             // 接受成功后写入 card_actions.owner
      Project, CommitSHA string      // retry 专用,用于水位分配
      PipelineID int
      // BuildTarget 只在 retry 分支、事务内分配到水位 N 之后调用,
      // 返回 (canonical target_input JSON, target_workflow_id, error)。
      //
      // **它必须是回调而不能是预先算好的 []byte**:target_input 含 Attempt,
      // 而 Attempt 只有在事务内推进水位后才知道;同时 CHECK 不可延迟,
      // 完整行必须一次性 INSERT。§5.5 的逐字段断言在这个回调里完成。
      // 两个约束共同逼出这个形状,不是可选设计。
      BuildTarget func(attempt int) (targetInput []byte, targetWorkflowID string, err error)
  }
  type AcceptOutcome struct {
      Kind string // accepted | resumed | conflict | legacy
      ActionToken string
      Attempt int
  }
  func (s *PGStore) CompleteAccept(ctx context.Context, req AcceptRequest) (*AcceptOutcome, error)
  func (s *PGStore) CompleteReject(ctx context.Context, eventID, token string, r RejectRender) error
  func (s *PGStore) FinalizeAction(ctx context.Context, workflowID, token, state, lastErr string) (bool, error)
  ```

- [ ] **Step 1: 写 conformance 测试（六条，逐条对应一个 fencing 规则）**

```go
t.Run("AcceptWritesCompleteRetryRowInOneStatement", func(t *testing.T) {
	// retry 行落库即已钉死 attempt/target/target_input。
	// 反面:分两步写入必被 CHECK 拒绝——证明"单事务一次性写入"不可绕过。
	s := newStore(t)
	seedRunAndArtifacts(t, s, "wf1", "grp/p", "abcd1234", 42, "v1")
	tok := claimForTest(t, s, "e1", "wf1", "retry")

	out, err := s.CompleteAccept(ctx, acceptReq("e1", tok, "wf1", "retry"))
	if err != nil || out.Kind != "accepted" {
		t.Fatalf("CompleteAccept = (%#v, %v)", out, err)
	}
	got := mustGetAction(t, s, "wf1")
	if got.Attempt <= 0 || got.TargetWorkflowID == "" || len(got.TargetInput) == 0 {
		t.Fatalf("retry 行必须落库即钉死: %#v", got)
	}
	// 分两步写入的反面证明
	if err := rawInsertRetryWithoutPins(s, "wf2"); err == nil {
		t.Fatal("先插默认值再补 attempt 的写法必须被 CHECK 拒绝")
	}
})

t.Run("AcceptIsSerializedAndBumpsWaterlineOnce", func(t *testing.T) {
	// 并发点击同一 workflow:恰好一次 accepted,水位恰好推进一次。
	s := newStore(t)
	seedRunAndArtifacts(t, s, "wf1", "grp/p", "abcd1234", 42, "v1", "v2")
	before := artifactAttempts(t, s)

	// claim 在 goroutine **之外**完成:claimForTest 内部会 t.Fatalf,
	// 而从非测试 goroutine 调 t.Fatal 是未定义行为(go vet 会报)。
	const n = 8
	toks := make([]string, n)
	for i := 0; i < n; i++ {
		toks[i] = claimForTest(t, s, fmt.Sprintf("e%d", i), "wf1", "retry")
	}

	kinds := make(chan string, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := s.CompleteAccept(ctx, acceptReq(fmt.Sprintf("e%d", i), toks[i], "wf1", "retry"))
			if err != nil {
				errs <- err
				return
			}
			kinds <- out.Kind
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent accept: %v", err)
	}
	close(kinds)
	accepted := 0
	for k := range kinds {
		if k == "accepted" {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted = %d, want 1", accepted)
	}
	if diff := artifactAttempts(t, s) - before; diff != 1 {
		t.Fatalf("水位推进 %d 次, want 1", diff)
	}
})

t.Run("FinalizeRequiresOwnerAndLiveLease", func(t *testing.T) {
	// 七个 completion 入口里最基础的一个:token 对但租约过期 → 零写入。
	s := newStore(t)
	seedAcceptedRetry(t, s, "wf1", "tokA")
	expireActionLease(t, s, "wf1")

	ok, err := s.FinalizeAction(ctx, "wf1", "tokA", "succeeded", "")
	if err != nil {
		t.Fatalf("FinalizeAction: %v", err)
	}
	if ok {
		t.Fatal("租约过期后 finalize 必须失败")
	}
	got := mustGetAction(t, s, "wf1")
	if got.State != "pending" || got.Revision != 1 || got.LastError != "" {
		t.Fatalf("失败的 finalize 不得改动任何字段: %#v", got)
	}
})

t.Run("FailedActionResumesReusingPins", func(t *testing.T) {
	// failed → pending 复用原 attempt/target/target_input,水位不再推进,
	// 且不写第二行 accepted 审计(只写 card.retry.resumed)。
	s := newStore(t)
	seedAcceptedRetry(t, s, "wf1", "tokA")
	_, _ = s.FinalizeAction(ctx, "wf1", "tokA", "failed", "temporal down")
	before := mustGetAction(t, s, "wf1")
	waterBefore := artifactAttempts(t, s)

	tok := claimForTest(t, s, "e2", "wf1", "retry")
	out, err := s.CompleteAccept(ctx, acceptReq("e2", tok, "wf1", "retry"))
	if err != nil || out.Kind != "resumed" {
		t.Fatalf("CompleteAccept = (%#v, %v), want resumed", out, err)
	}
	after := mustGetAction(t, s, "wf1")
	if after.Attempt != before.Attempt || after.TargetWorkflowID != before.TargetWorkflowID ||
		!bytes.Equal(after.TargetInput, before.TargetInput) {
		t.Fatalf("resume 必须复用原钉死值: before=%#v after=%#v", before, after)
	}
	if after.State != "pending" || after.Revision != before.Revision+1 {
		t.Fatalf("resume 后 state/revision 不对: %#v", after)
	}
	if artifactAttempts(t, s) != waterBefore {
		t.Fatal("resume 不得推进水位")
	}
	if n := countAuditByAction(t, s, "card.retry.accepted"); n != 1 {
		t.Fatalf("accepted 审计 = %d, want 1", n)
	}
	if n := countAuditByAction(t, s, "card.retry.resumed"); n != 1 {
		t.Fatalf("resumed 审计 = %d, want 1", n)
	}
})

t.Run("ActionCannotChangeAfterAccept", func(t *testing.T) {
	// 动作首次接受即固定:retry failed 后点 ignore 只能得到 conflict。
	s := newStore(t)
	seedAcceptedRetry(t, s, "wf1", "tokA")
	_, _ = s.FinalizeAction(ctx, "wf1", "tokA", "failed", "boom")

	tok := claimForTest(t, s, "e3", "wf1", "ignore")
	out, err := s.CompleteAccept(ctx, acceptReq("e3", tok, "wf1", "ignore"))
	if err != nil || out.Kind != "conflict" {
		t.Fatalf("异 action 必须 conflict, got (%#v, %v)", out, err)
	}
})

t.Run("IgnoreLandsTerminalWithoutPins", func(t *testing.T) {
	s := newStore(t)
	seedRunAndArtifacts(t, s, "wf1", "grp/p", "abcd1234", 42, "v1")
	tok := claimForTest(t, s, "e1", "wf1", "ignore")
	if _, err := s.CompleteAccept(ctx, acceptReq("e1", tok, "wf1", "ignore")); err != nil {
		t.Fatalf("ignore accept: %v", err)
	}
	got := mustGetAction(t, s, "wf1")
	if got.State != "succeeded" || got.Attempt != 0 ||
		got.TargetWorkflowID != "" || got.TargetInput != nil {
		t.Fatalf("ignore 行必须终态且无钉死值: %#v", got)
	}
})
```

- [ ] **Step 2: 运行，确认失败**

Run: `cd runtime && go test ./internal/store/ -run 'Conformance/(Accept|Finalize|Failed|Action|Ignore)' -v`
Expected: 编译失败。

- [ ] **Step 3: 实现 CompleteAccept**

严格按 spec §5.1 的顺序，**不得调换**：

```go
func (s *PGStore) CompleteAccept(ctx context.Context, req AcceptRequest) (*AcceptOutcome, error) {
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	...
	// ① fencing:只校验,不 acquire。租约必须仍然有效——
	//    过期到被重新 claim 之间,旧持有者的 token 仍然匹配,只比对 owner 会放过失效结果。
	var inbox InboxRow
	err = tx.QueryRowContext(ctx, `
		SELECT event_id FROM card_action_inbox
		 WHERE event_id=$1 AND state='received'
		   AND owner=$2 AND lease_expires_at > now()
		 FOR UPDATE`, req.EventID, req.Token).Scan(&inbox.EventID)
	if errors.Is(err, sql.ErrNoRows) {
		return &AcceptOutcome{Kind: "lost"}, nil   // 零业务写入
	}

	// ② 锁父行串行化。首次点击时 card_actions 行还不存在,不存在的行锁不住,
	//    所以锁必须加在 workflow_runs 上。
	var exists int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM workflow_runs WHERE workflow_id=$1 FOR UPDATE`, req.WorkflowID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		// legacy:写拒绝审计 + rejection message + processed,同事务
		...
		return &AcceptOutcome{Kind: "legacy"}, tx.Commit()
	}

	// ③ 读既有 action(已在父行锁保护下)
	//    无行 → 首次接受;同 action 且 failed → resume;其余 → conflict
	...
	// 首次接受(retry):推进水位 → 算 target_workflow_id → 一次性 INSERT 完整行
	//                  (§5.5 的逐字段断言在调用方完成,这里只信任已校验的入参)
	// 首次接受(ignore):state='succeeded',不钉任何值

	// ④ 登记 message,所有分支都做;rejection → action 单向升级
	_, err = tx.ExecContext(ctx, `
		INSERT INTO card_action_messages
			(workflow_id, open_message_id, render_kind, desired_revision, update_state)
		VALUES ($1,$2,'action',$3,'pending')
		ON CONFLICT (workflow_id, open_message_id) DO UPDATE SET
			render_kind='action', rejection_reason='', buttons_mode='none',
			desired_revision=$3, update_state='pending',
			reconcile_after=NULL, owner='', lease_expires_at=NULL,
			updated_at=now()`, req.WorkflowID, req.OpenMessageID, revision)

	// ⑤ inbox 置 processed,同事务
	_, err = tx.ExecContext(ctx, `
		UPDATE card_action_inbox SET state='processed', processed_at=now()
		 WHERE event_id=$1`, req.EventID)

	return outcome, tx.Commit()
}
```

`FinalizeAction` 带三重条件并在成功时 `revision+1` + 重排 message：

```go
UPDATE card_actions
   SET state=$3, last_error=$4, revision=revision+1,
       owner='', lease_expires_at=NULL, updated_at=now()
 WHERE workflow_id=$1 AND state='pending'
   AND owner=$2 AND lease_expires_at > now()
```
影响 0 行即返回 `false`，**调用方不得再写任何字段**。

- [ ] **Step 4: 运行，确认通过**

Run: `cd runtime && go test ./internal/store/ -run 'Conformance/(Accept|Finalize|Failed|Action|Ignore)' -count=1 -v`
Expected: PASS（MemStore + PGStore）

- [ ] **Step 5: 真实 PG 并发专项**

Run: `cd runtime && go test ./internal/store/ -run 'TestPGStoreConformance/AcceptIsSerializedAndBumpsWaterlineOnce' -count=3 -v`
Expected: 连跑三次全绿（并发用例必须稳定，不能靠运气）。

- [ ] **Step 6: 提交**

```bash
git add runtime/internal/store/card_actions.go runtime/internal/store/postgres_card_actions.go \
        runtime/internal/store/conformance_test.go
git commit -m "feat(store): accept a card action in a single transaction

Accept locks the workflow_runs row rather than card_actions, because on a
first click the action row does not exist yet and a row that does not
exist cannot be locked. That lock is what serializes the check-then-insert
and keeps the waterline advancing exactly once per claim.

Completion verifies the fencing token together with lease liveness. Token
alone is not enough: between expiry and the next claim the stale holder
still matches, and would be allowed to commit a resolution computed
against state that has since moved.

Resume reuses the pinned attempt, target and input rather than resolving
again, so a retry that failed and was clicked a second time cannot start
a second workflow."
```

---

## Task 6: store —— messages、snapshots 与 sweep 查询

**Files:**
- Modify: `runtime/internal/store/card_actions.go`、`postgres_card_actions.go`、`conformance_test.go`

**Interfaces:**
- Produces:
  ```go
  func (s *PGStore) PutCardSnapshot(ctx context.Context, workflowID string, cardJSON []byte) error
  func (s *PGStore) GetCardSnapshot(ctx context.Context, workflowID string) ([]byte, error)
  func (s *PGStore) ClaimMessage(ctx context.Context, token string, lease time.Duration) (*MessageClaim, error)
  type MessageClaim struct {
      WorkflowID, OpenMessageID, RenderKind, RejectionReason, ButtonsMode string
      DesiredRevision int
      Action *CardAction // render_kind='action' 时非 nil
  }
  func (s *PGStore) CompleteMessageRender(ctx context.Context, c MessageClaim, token string) (bool, error)
  func (s *PGStore) DeferMessageRender(ctx context.Context, c MessageClaim, token string, after time.Duration, lastErr string) (bool, error)
  func (s *PGStore) AbandonMessageRender(ctx context.Context, c MessageClaim, token string, lastErr string) (bool, error)
  func (s *PGStore) ClaimStaleAction(ctx context.Context, token string, lease time.Duration) (*CardAction, error)
  func (s *PGStore) ClaimStaleInbox(ctx context.Context, token string, lease time.Duration) (*InboxRow, error)
  ```

- [ ] **Step 1: 写 conformance 测试（收敛与 fencing）**

```go
t.Run("MessageSweepPredicateAcceptsNullLease", func(t *testing.T) {
	// 首次 pending 的租约是 NULL。谓词写成 lease < now() 会永不命中,
	// 卡片永远不更新。
	s := newStore(t)
	seedAcceptedRetry(t, s, "wf1", "tokA")   // 顺带建了 message 行
	c, err := s.ClaimMessage(ctx, "tokM", 120*time.Second)
	if err != nil || c == nil {
		t.Fatalf("NULL 租约的行必须能被 claim: (%v, %v)", c, err)
	}
})

t.Run("MessageCompletionNeedsOwnerLeaseAndRevision", func(t *testing.T) {
	// revision fencing:PATCH rev1 期间动作推进到 rev2,
	// 旧 worker 的 completion 必须影响 0 行,否则 rev2 永远不再被 sweep。
	s := newStore(t)
	seedAcceptedRetry(t, s, "wf1", "tokA")
	c, _ := s.ClaimMessage(ctx, "tokM", 120*time.Second)
	if c.DesiredRevision != 1 {
		t.Fatalf("claimed revision = %d", c.DesiredRevision)
	}
	// 动作推进 → revision=2 且 message 被重排
	_, _ = s.FinalizeAction(ctx, "wf1", "tokA", "succeeded", "")

	ok, err := s.CompleteMessageRender(ctx, *c, "tokM")
	if err != nil {
		t.Fatalf("CompleteMessageRender: %v", err)
	}
	if ok {
		t.Fatal("revision 已推进,旧 completion 必须影响 0 行")
	}
	m := mustGetMessage(t, s, "wf1", "om_1")
	if m.UpdateState != "pending" || m.DesiredRevision != 2 {
		t.Fatalf("行必须留在 pending/rev2 等待重渲染: %#v", m)
	}
})

t.Run("LostLeaseWriterWritesNothing", func(t *testing.T) {
	// 失租写方零写入:连 reconcile_after 也不许写。
	s := newStore(t)
	seedAcceptedRetry(t, s, "wf1", "tokA")
	c, _ := s.ClaimMessage(ctx, "tokM", 120*time.Second)
	expireMessageLease(t, s, "wf1", "om_1")
	_, _ = s.ClaimMessage(ctx, "tokN", 120*time.Second) // 被接管
	before := mustGetMessage(t, s, "wf1", "om_1")

	for _, call := range []func() (bool, error){
		func() (bool, error) { return s.CompleteMessageRender(ctx, *c, "tokM") },
		func() (bool, error) { return s.DeferMessageRender(ctx, *c, "tokM", time.Minute, "timeout") },
		func() (bool, error) { return s.AbandonMessageRender(ctx, *c, "tokM", "gone") },
	} {
		ok, err := call()
		if err != nil || ok {
			t.Fatalf("失租写方必须零写入, got (%v, %v)", ok, err)
		}
	}
	if !reflect.DeepEqual(before, mustGetMessage(t, s, "wf1", "om_1")) {
		t.Fatal("失租写方改动了字段")
	}
})

t.Run("ReconcileAfterDelaysNextClaim", func(t *testing.T) {
	// 超时后设 reconcile_after,未到期时 sweep 选不中该行。
	s := newStore(t)
	seedAcceptedRetry(t, s, "wf1", "tokA")
	c, _ := s.ClaimMessage(ctx, "tokM", 120*time.Second)
	if ok, err := s.DeferMessageRender(ctx, *c, "tokM", time.Minute, "patch timeout"); err != nil || !ok {
		t.Fatalf("DeferMessageRender = (%v, %v)", ok, err)
	}
	expireMessageLease(t, s, "wf1", "om_1")

	if got, err := s.ClaimMessage(ctx, "tokN", 120*time.Second); err == nil && got != nil {
		t.Fatal("reconcile_after 未到期时不得被 claim")
	}
})

t.Run("SnapshotRoundTrip", func(t *testing.T) {
	s := newStore(t)
	if err := s.PutCardSnapshot(ctx, "wf1", []byte(`{"config":{"wide_screen_mode":true,"update_multi":true}}`)); err != nil {
		t.Fatalf("PutCardSnapshot: %v", err)
	}
	got, err := s.GetCardSnapshot(ctx, "wf1")
	if err != nil || !bytes.Contains(got, []byte("update_multi")) {
		t.Fatalf("GetCardSnapshot = (%s, %v)", got, err)
	}
})
```

- [ ] **Step 2: 运行，确认失败**

Run: `cd runtime && go test ./internal/store/ -run 'Conformance/(Message|LostLease|Reconcile|Snapshot)' -v`
Expected: 编译失败。

- [ ] **Step 3: 实现**

`ClaimMessage` 的谓词必须同时含两个合取子句：

```sql
UPDATE card_action_messages
   SET owner=$1, lease_expires_at=now()+$2::interval, attempts=attempts+1
 WHERE (workflow_id, open_message_id) = (
     SELECT workflow_id, open_message_id FROM card_action_messages
      WHERE update_state='pending'
        AND (lease_expires_at IS NULL OR lease_expires_at < now())
        AND (reconcile_after IS NULL OR reconcile_after <= now())
      ORDER BY updated_at LIMIT 1 FOR UPDATE SKIP LOCKED)
RETURNING ...
```

三个 completion 共用同一段 WHERE：

```sql
 WHERE workflow_id=$1 AND open_message_id=$2
   AND owner=$3 AND lease_expires_at > now()
   AND desired_revision=$4
```

- [ ] **Step 4: 运行，确认通过**

Run: `cd runtime && go test ./internal/store/ -run 'Conformance/(Message|LostLease|Reconcile|Snapshot)' -count=1 -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add runtime/internal/store/card_actions.go runtime/internal/store/postgres_card_actions.go \
        runtime/internal/store/conformance_test.go
git commit -m "feat(store): converge card renders per message instance

NotifyCard is retryable, so one workflow can end up with several cards;
rendering is keyed by message instance rather than by workflow. The
messages table deliberately has no foreign key to card_actions, because
the rejection paths that never produce an action still have a conclusion
to show.

Completion matches owner, lease and the revision claimed at render time.
Without the revision the old writer could mark a row succeeded after the
action had already moved on, and the newer state would never be swept
again. A writer that lost the row writes nothing at all -- not even
reconcile_after, which belongs to whoever holds it now."
```

---

## Task 7: feishu.CardUpdater

**Files:**
- Modify: `runtime/internal/feishu/feishu.go`
- Test: `runtime/internal/feishu/feishu_test.go`

**Interfaces:**
- Produces: `type CardUpdater interface { PatchCard(ctx context.Context, messageID string, card any) error }`，
  仅 `appSender` 实现。

- [ ] **Step 1: 写测试**

```go
// TestWebhookSenderIsNotCardUpdater:webhook 自定义机器人没有消息更新能力。
// 更要紧的是它必须继续满足 CardSender——把 PatchCard 加到 CardSender 上会让它掉出接口,
// notify_card.go 的类型断言随即失败,webhook 模式的展示卡片静默退化成纯文本。
func TestWebhookSenderIsNotCardUpdater(t *testing.T) {
	s, mode := NewSender(Config{WebhookURL: "https://example/hook"})
	if mode != "webhook" {
		t.Fatalf("mode = %q", mode)
	}
	if _, ok := s.(CardSender); !ok {
		t.Fatal("webhook sender 必须仍然满足 CardSender")
	}
	if _, ok := s.(CardUpdater); ok {
		t.Fatal("webhook sender 不应满足 CardUpdater")
	}
}

// TestAppSenderPatchCardWireShape:PATCH 走 im/v1/messages/:message_id,
// body 是 {"content": <卡片 JSON 字符串>}。
func TestAppSenderPatchCardWireShape(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t","expire":7200}`))
			return
		}
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()

	s, _ := NewSender(Config{AppID: "a", AppSecret: "b", ReceiveID: "oc_x", BaseURL: srv.URL})
	cu, ok := s.(CardUpdater)
	if !ok {
		t.Fatal("app sender 必须满足 CardUpdater")
	}
	if err := cu.PatchCard(context.Background(), "om_1",
		map[string]any{"config": map[string]any{"update_multi": true}}); err != nil {
		t.Fatalf("PatchCard: %v", err)
	}
	if !strings.HasSuffix(gotPath, "/im/v1/messages/om_1") {
		t.Fatalf("path = %q", gotPath)
	}
	if !strings.Contains(gotBody, `"content"`) || !strings.Contains(gotBody, `update_multi`) {
		t.Fatalf("body = %q", gotBody)
	}
}
```

- [ ] **Step 2: 运行，确认失败**

Run: `cd runtime && go test ./internal/feishu/ -run 'CardUpdater|PatchCard' -v`
Expected: FAIL——`CardUpdater` 未定义。

- [ ] **Step 3: 实现**

```go
// CardUpdater 是能更新已发送卡片的 Sender。**单独成接口,绝不加到 CardSender 上**:
// webhookSender 已实现 SendCard,扩 CardSender 会让它掉出接口,
// notify_card.go 的类型断言失败后 webhook 模式会静默退化成纯文本。
type CardUpdater interface {
	PatchCard(ctx context.Context, messageID string, card any) error
}

// PatchCard 更新应用已发送的卡片消息。要求原卡带 config.update_multi=true。
func (s *appSender) PatchCard(ctx context.Context, messageID string, card any) error {
	content, err := json.Marshal(card)
	if err != nil {
		return fmt.Errorf("feishu: encode patch card content: %w", err)
	}
	return s.patchMessage(ctx, messageID, string(content))
}
```
`patchMessage` 复用 `appSender.send` 的 token 缓存与过期重试一次的模式，
方法为 `PATCH`，路径 `/open-apis/im/v1/messages/{messageID}`。

- [ ] **Step 4: 运行，确认通过**

Run: `cd runtime && go test ./internal/feishu/ -count=1 -v`
Expected: PASS（含既有用例）

- [ ] **Step 5: 提交**

```bash
git add runtime/internal/feishu/feishu.go runtime/internal/feishu/feishu_test.go
git commit -m "feat(feishu): add CardUpdater as a separate interface

Patching belongs to the app sender only; the webhook bot cannot update a
message it already sent. Putting PatchCard on CardSender would drop
webhookSender out of that interface and silently turn its display cards
back into plain text, so the capability gets its own interface and a test
asserts webhook still satisfies CardSender and does not satisfy this one."
```

---

## Task 8: cardaction —— 同步 handler

**Files:**
- Create: `runtime/internal/cardaction/handler.go`、`readiness.go`、`handler_test.go`

**Interfaces:**
- Consumes: `store.PutInbox`、`feishu` 无
- Produces:
  ```go
  type Handler struct {
      Store     Store
      Readiness *Readiness
      Whitelist map[string]bool
      AppID     string
      Log       *zerolog.Logger
      Consume   func(eventID string)  // 交异步段;nil 时不投递(测试)
  }
  func (h *Handler) OnCardAction(ctx context.Context, ev *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error)

  type Readiness struct{ ... }
  func (r *Readiness) Ready() bool
  func (r *Readiness) SetWS(up bool)
  ```

- [ ] **Step 1: 写测试**

```go
// TestSyncRejectsFailClosed:§6.2 表格逐行覆盖,任一不满足即不进 claim。
func TestSyncRejectsFailClosed(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*callback.CardActionTriggerRequest)
		want   string // 期望的审计 action 后缀
	}{
		{"AppID 不符", func(r *callback.CardActionTriggerRequest) {}, "payload"}, // header 在外层构造
		{"tenant 为空", func(r *callback.CardActionTriggerRequest) { r.Operator.TenantKey = nil }, "payload"},
		{"tag 非 button", func(r *callback.CardActionTriggerRequest) { r.Action.Tag = "select_static" }, "payload"},
		{"OpenMessageID 空", func(r *callback.CardActionTriggerRequest) { r.Context.OpenMessageID = "" }, "payload"},
		{"host 非消息卡片", func(r *callback.CardActionTriggerRequest) { r.Host = "im_top_notice" }, "payload"},
		{"open_id 空", func(r *callback.CardActionTriggerRequest) { r.Operator.OpenID = "" }, "payload"},
		{"value 多一个键", func(r *callback.CardActionTriggerRequest) { r.Action.Value["extra"] = "x" }, "payload"},
		{"未知 action", func(r *callback.CardActionTriggerRequest) { r.Action.Value["action"] = "reboot" }, "payload"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			h := newHandler(st, readyAll())
			ev := validEvent()
			tc.mutate(ev.Event)

			resp, err := h.OnCardAction(ctx, ev)
			if err != nil {
				t.Fatalf("handler 不应返回 error(那会让飞书重投一个必然失败的载荷): %v", err)
			}
			if resp.Toast == nil {
				t.Fatal("必须返回 toast")
			}
			if st.claims != 0 {
				t.Fatalf("claim 次数 = %d, want 0", st.claims)
			}
			assertAuditSuffix(t, st, tc.want)
		})
	}
}

// TestUnauthorizedNeverUsesSendText:非白名单必须走同步 toast。
// SendText 会发到固定的 FEISHU_RECEIVE_ID(可能是群),
// 等于把一次未授权点击广播给所有人。
func TestUnauthorizedNeverUsesSendText(t *testing.T) {
	st, sender := newFakeStore(), &countingSender{}
	h := newHandler(st, readyAll())
	h.Whitelist = map[string]bool{"ou_allowed": true}
	ev := validEvent()
	ev.Event.Operator.OpenID = "ou_stranger"

	resp, err := h.OnCardAction(ctx, ev)
	if err != nil || resp.Toast == nil {
		t.Fatalf("(%v, %v)", resp, err)
	}
	if sender.sendTextCalls != 0 {
		t.Fatalf("SendText 调用 %d 次,必须为 0", sender.sendTextCalls)
	}
	assertAuditSuffix(t, st, "unauthorized")
}

// TestInboxWriteFailureReturnsError:写不进 inbox 时必须返回 error。
// SDK 会应答 500,飞书据此重投——返回成功 toast 等于把点击静默丢弃。
func TestInboxWriteFailureReturnsError(t *testing.T) {
	st := newFakeStore()
	st.putErr = errors.New("db down")
	h := newHandler(st, readyAll())

	if _, err := h.OnCardAction(ctx, validEvent()); err == nil {
		t.Fatal("inbox 写失败必须返回 error 促使飞书重投")
	}
}

// TestReadinessOffRejectsWithoutClaim:五项 readiness 任一为假 → rejected.disabled。
func TestReadinessOffRejectsWithoutClaim(t *testing.T) {
	for _, off := range []string{"switch", "whitelist", "mode", "handler", "ws"} {
		t.Run(off, func(t *testing.T) {
			st := newFakeStore()
			h := newHandler(st, readyExcept(off))
			resp, err := h.OnCardAction(ctx, validEvent())
			if err != nil || resp.Toast == nil {
				t.Fatalf("(%v, %v)", resp, err)
			}
			if st.claims != 0 {
				t.Fatal("readiness 为假时不得进 claim")
			}
			assertAuditSuffix(t, st, "disabled")
		})
	}
}
```

- [ ] **Step 2: 运行，确认失败**

Run: `cd runtime && go test ./internal/cardaction/ -v`
Expected: 包不存在。

- [ ] **Step 3: 实现 handler 与 readiness**

`OnCardAction` 顺序固定：来源校验 → 载荷 → 身份 → readiness → 白名单 →
`PutInbox` → 返回 toast。整段带 2 秒 deadline：

```go
ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
defer cancel()
```

`Readiness` 用 `atomic.Bool` 承载 WS 状态，其余四项装配期固定：

```go
// Ready 是五项合取。只能承诺"发送瞬间 ready"——卡片发出后连接可能断开,
// 所以回调路径必须再查一次(设计 §7.2)。
func (r *Readiness) Ready() bool {
	return r.enabled && r.whitelistNonEmpty && r.senderIsApp && r.handlerWired && r.wsUp.Load()
}
```

- [ ] **Step 4: 运行，确认通过**

Run: `cd runtime && go test ./internal/cardaction/ -count=1 -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add runtime/internal/cardaction/handler.go runtime/internal/cardaction/readiness.go \
        runtime/internal/cardaction/handler_test.go
git commit -m "feat(cardaction): validate and persist clicks synchronously

Everything the handler can decide without I/O it decides before writing
anything: app and tenant identity, button tag, host, payload size, the
closed value shape, readiness and the whitelist. Only then does the click
reach the inbox.

A failed inbox write returns an error rather than a cheerful toast, so
the SDK answers 500 and Feishu redelivers. Answering successfully with
nothing on disk is how a click disappears.

Unauthorized clicks are answered by the synchronous toast and never by
SendText, which posts to the configured receive ID and would broadcast
one person's unauthorized click to the whole group."
```

---

## Task 9: cardaction —— 异步消费

**Files:**
- Create: `runtime/internal/cardaction/consume.go`、`consume_test.go`

**Interfaces:**
- Consumes: `rerun.Resolver`、`store.ClaimInbox` / `CompleteAccept` / `CompleteReject` / `FinalizeAction`、
  `trigger.WorkflowStarter`
- Produces: `func (c *Consumer) ConsumeOne(ctx context.Context, eventID string) error`

- [ ] **Step 1: 写测试**

```go
// TestTargetInputAssertedFieldByField:WorkflowID() 不含 Version/RuleVersion/
// Packages/SourceWorkflowID,只断言它相等的实现会放过缺字段输入。
func TestTargetInputAssertedFieldByField(t *testing.T) {
	for _, drop := range []string{"Version", "RuleVersion", "Packages", "SourceWorkflowID"} {
		t.Run(drop, func(t *testing.T) {
			c := newConsumer(t, withResolution(fullResolution()))
			c.mutateInput = dropField(drop) // 注入缺字段的构造
			err := c.ConsumeOne(ctx, "e1")
			if err == nil {
				t.Fatalf("缺 %s 的 target_input 必须被拒", drop)
			}
			if c.store.actionRows() != 0 {
				t.Fatal("断言失败时不得写入 action 行")
			}
		})
	}
}

// TestStartedFalseIsIdempotentSuccess:target 已钉死,
// AlreadyStarted 说明该 workflow 已在运行,是成功不是失败。
func TestStartedFalseIsIdempotentSuccess(t *testing.T) {
	c := newConsumer(t, withStarter(&fakeStarter{started: false}))
	if err := c.ConsumeOne(ctx, "e1"); err != nil {
		t.Fatalf("ConsumeOne: %v", err)
	}
	if got := c.store.actionState("wf1"); got != "succeeded" {
		t.Fatalf("state = %q, want succeeded", got)
	}
}

// TestCrashBetweenAckAndClaimIsRecovered:v1 阻断 2 的回归。
func TestCrashBetweenAckAndClaimIsRecovered(t *testing.T) {
	// 模拟:inbox 有 received 行,但从未被消费过(进程在 ACK 后退出)
	c := newConsumer(t, withInboxRow("e1", "received"))
	if err := c.ConsumeOne(ctx, "e1"); err != nil {
		t.Fatalf("sweep 接管后必须能完成: %v", err)
	}
	if c.store.actionState("wf1") == "" {
		t.Fatal("动作丢失")
	}
}

// TestRejectionRendersOnCard:legacy/仍在运行/无失败变体经 CompleteReject
// 在卡片上留下结论,且 attempt 与 StartDeviceTest 零调用。
func TestRejectionRendersOnCard(t *testing.T) {
	for _, code := range []string{"NotAuthoritative", "StillRunning", "NoFailedVariants"} {
		t.Run(code, func(t *testing.T) {
			c := newConsumer(t, withRejectReason(code))
			if err := c.ConsumeOne(ctx, "e1"); err != nil {
				t.Fatalf("ConsumeOne: %v", err)
			}
			if c.store.attemptCalls != 0 || c.starter.startCalls != 0 {
				t.Fatal("拒绝路径必须零调用 attempt 与 StartDeviceTest")
			}
			m := c.store.message("wf1", "om_1")
			if m.RenderKind != "rejection" || m.RejectionReason == "" {
				t.Fatalf("message = %#v", m)
			}
		})
	}
}

// TestButtonsModeByReason:§7.5 表格逐行。
func TestButtonsModeByReason(t *testing.T) {
	for code, want := range map[string]string{
		"StillRunning": "both", "ResultUnreadable": "both", "ArtifactMissing": "both",
		"NotAuthoritative": "none", "NoFailedVariants": "none",
	} {
		t.Run(code, func(t *testing.T) {
			c := newConsumer(t, withRejectReason(code))
			_ = c.ConsumeOne(ctx, "e1")
			if got := c.store.message("wf1", "om_1").ButtonsMode; got != want {
				t.Fatalf("buttons_mode = %q, want %q", got, want)
			}
		})
	}
}
```

- [ ] **Step 2: 运行，确认失败**

Run: `cd runtime && go test ./internal/cardaction/ -run 'TargetInput|StartedFalse|Crash|Rejection|ButtonsMode' -v`
Expected: 编译失败。

- [ ] **Step 3: 实现 ConsumeOne**

```go
func (c *Consumer) ConsumeOne(ctx context.Context, eventID string) error {
	token := newFencingToken()  // 128 bit 随机 hex,绝不复用
	row, err := c.Store.ClaimInbox(ctx, eventID, token, leaseTTL)
	if errors.Is(err, store.ErrInboxNotClaimable) {
		return nil // 已被他人持有或已处理
	}
	...
	// ignore 只需要 ResolveFailureRun;retry 才走 ResolveRetry
	if row.Action == "ignore" {
		if _, err := c.Resolver.ResolveFailureRun(ctx, row.WorkflowID); err != nil {
			return c.reject(ctx, row, token, err)
		}
		_, err := c.Store.CompleteAccept(ctx, ignoreReq(row, token))
		return err
	}
	res, err := c.Resolver.ResolveRetry(ctx, row.WorkflowID, "")
	if err != nil {
		return c.reject(ctx, row, token, err)
	}
	// target_input 含 Attempt,而 Attempt 只有在事务内推进水位后才知道,
	// 所以构造与逐字段断言都在回调里、事务内完成(§5.5)。
	var built wf.DeviceTestInput
	req := acceptReq(row, token)
	req.BuildTarget = func(attempt int) ([]byte, string, error) {
		built = buildTargetInput(res, attempt)
		if err := assertTargetInput(built, res, attempt); err != nil {
			return nil, "", fmt.Errorf("target input mismatch (实现缺陷): %w", err)
		}
		raw, err := canonicalJSON(built)
		return raw, built.WorkflowID(), err
	}
	out, err := c.Store.CompleteAccept(ctx, req)
	...
	// 执行:started=false 是幂等成功
	_, started, err := c.Starter.StartDeviceTest(ctx, in)
	state, lastErr := "succeeded", ""
	if err != nil {
		if isPermanent(err) {
			state, lastErr = "failed", err.Error()
		} else {
			return err  // 暂时错误:留给 sweep 重试
		}
	}
	_ = started // AlreadyStarted 亦为成功:target 已钉死,说明该 workflow 已在运行
	ok, err := c.Store.FinalizeAction(ctx, row.WorkflowID, out.ActionToken, state, lastErr)
	if !ok {
		c.log().Warn().Msg("finalize lost fencing; sweep will take over")  // 零写入
	}
	return err
}
```

**注意 attempt 的来源**：`CompleteAccept` 在事务内分配水位后才知道 N，而 `target_input`
要含 `Attempt`。实现方式是 `CompleteAccept` 接收一个 `func(attempt int) ([]byte, string, error)`
回调，在事务内拿到 N 后回调构造并逐字段断言，再一次性 INSERT。

- [ ] **Step 4: 运行，确认通过**

Run: `cd runtime && go test ./internal/cardaction/ -count=1 -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add runtime/internal/cardaction/consume.go runtime/internal/cardaction/consume_test.go
git commit -m "feat(cardaction): consume clicks and start the retry

The input handed to Temporal is checked field by field against the
resolution that produced it. WorkflowID covers project, commit, pipeline,
scope and attempt and nothing else, so an input missing Version,
RuleVersion, Packages or SourceWorkflowID would pass an ID comparison and
start a workflow configured differently from the one the user asked for.

AlreadyStarted counts as success. The target is pinned, so a duplicate
start means that exact workflow is already running, which is the outcome
the click wanted."
```

---

## Task 10: cardaction —— 三个 sweep 与卡片渲染

**Files:**
- Create: `runtime/internal/cardaction/sweep.go`、`render.go`、`sweep_test.go`、`render_test.go`

**Interfaces:**
- Produces:
  ```go
  func (s *Sweeper) RunOnce(ctx context.Context) error   // 三个 sweep 各扫一轮
  func (s *Sweeper) Run(ctx context.Context)             // 30s 轮询,阻塞直到 ctx 取消
  func RenderCard(snapshot []byte, c store.MessageClaim) ([]byte, error)
  ```

- [ ] **Step 1: 写渲染测试**

```go
// TestRenderPreservesSnapshotByteForByte:除 action 模块外所有元素必须与快照逐字节相同。
func TestRenderPreservesSnapshotByteForByte(t *testing.T) {
	snap := []byte(`{"config":{"wide_screen_mode":true,"update_multi":true},` +
		`"header":{"title":{"tag":"plain_text","content":"[hermes-devops] p g1 p1 (v1)"},"template":"red"},` +
		`"elements":[{"tag":"div","text":{"tag":"plain_text","content":"v1  TEST_FAILED(CODE)"}}]}`)
	out, err := RenderCard(snap, claimSucceededRetry("ou_x", "device-test-p-g1-p1-r2"))
	if err != nil {
		t.Fatalf("RenderCard: %v", err)
	}
	var got, want map[string]any
	_ = json.Unmarshal(out, &got)
	_ = json.Unmarshal(snap, &want)
	if !reflect.DeepEqual(got["config"], want["config"]) ||
		!reflect.DeepEqual(got["header"], want["header"]) {
		t.Fatal("config/header 必须逐字保留")
	}
	els := got["elements"].([]any)
	if !reflect.DeepEqual(els[0], want["elements"].([]any)[0]) {
		t.Fatal("原有元素必须逐字保留")
	}
	last := els[len(els)-1].(map[string]any)
	if last["text"].(map[string]any)["content"] != "已由 ou_x 重试 → device-test-p-g1-p1-r2" {
		t.Fatalf("状态文本不符: %v", last)
	}
}

// TestRenderStatusTexts:§7.3 四种状态逐字匹配(表格是契约)。
func TestRenderStatusTexts(t *testing.T) {
	cases := map[string]string{
		"retry-pending":   "已由 ou_x 重试，正在启动…",
		"retry-succeeded": "已由 ou_x 重试 → wf-target",
		"retry-failed":    "重试启动失败：temporal down",
		"ignore":          "已由 ou_x 忽略（仅记录，不改变判定）",
	}
	for name, want := range cases { ... }
}

// TestFailedRetryKeepsOnlyResumeButton:失败态只保留「重新重试」——
// 摆一个必然 conflict 的 ignore 按钮是误导。
func TestFailedRetryKeepsOnlyResumeButton(t *testing.T) {
	out, _ := RenderCard(minimalSnapshot(), claimFailedRetry("ou_x", "boom"))
	buttons := extractButtons(t, out)
	if len(buttons) != 1 || buttons[0].Value.Action != "retry" {
		t.Fatalf("buttons = %#v, want 只有一个 retry", buttons)
	}
}

// TestRejectionButtonsMode:both 保留两个按钮,none 只留状态文本。
func TestRejectionButtonsMode(t *testing.T) { ... }
```

- [ ] **Step 2: 运行，确认失败**

Run: `cd runtime && go test ./internal/cardaction/ -run Render -v`
Expected: 编译失败。

- [ ] **Step 3: 实现 RenderCard 与三个 sweep**

`RenderCard` 反序列化快照 → 追加/替换末尾的 action 元素 → 重新序列化。
超预算时（`len(out) > 30*1024`）返回快照原文（发原展示卡片，不降级纯文本）。

`Sweeper.RunOnce` 依次跑：inbox sweep（`ClaimStaleInbox` → `Consumer.ConsumeOne`）、
action sweep（`ClaimStaleAction` → 用持久化 `target_input` 重 `StartDeviceTest` → finalize）、
card sweep（`ClaimMessage` → `GetCardSnapshot` → `RenderCard` → `PatchCard` → 三种 completion）。

PATCH 的三种归宿：

```go
switch classifyPatchErr(err) {
case patchOK:
	ok, _ := s.Store.CompleteMessageRender(ctx, c, token)
	if !ok { s.log().Warn().Msg("render completion lost fencing; leaving row to sweep") } // 零写入
case patchAmbiguous: // 超时/不确定:不得标 succeeded 或 abandoned
	_, _ = s.Store.DeferMessageRender(ctx, c, token, 60*time.Second, err.Error())
case patchPermanent: // 超 14 天更新期限 / message 不存在 / 权限不足
	_, _ = s.Store.AbandonMessageRender(ctx, c, token, err.Error())
case patchTransient:
	// 什么都不做,租约到期后重试
}
```

- [ ] **Step 4: 运行，确认通过**

Run: `cd runtime && go test ./internal/cardaction/ -count=1 -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add runtime/internal/cardaction/sweep.go runtime/internal/cardaction/render.go \
        runtime/internal/cardaction/sweep_test.go runtime/internal/cardaction/render_test.go
git commit -m "feat(cardaction): sweep for recovery and render from the snapshot

Rendering copies the stored display card and replaces only the action
block, so everything the workflow wrote survives byte for byte. If the
result no longer fits the budget the original card goes out unchanged
rather than degrading to text -- losing the buttons is better than losing
the report.

An ambiguous PATCH is neither success nor failure. It defers for a minute
and renders again, because a timed-out request may still land, and
marking it succeeded would strand whatever state came next."
```

---

## Task 11: activity —— 快照、readiness 与按钮注入

**Files:**
- Modify: `runtime/internal/activity/notify_card.go`
- Create: `runtime/internal/activity/notify_card_inject_test.go`

**Interfaces:**
- Consumes: `store.PutCardSnapshot`、`feishu.CardSender`、`cardaction.Readiness`
- Produces: 无新导出符号（`Acts.NotifyCard` 行为扩展）

- [ ] **Step 1: 写测试（必须在 activity testsuite 中）**

```go
// 注入依赖 activity.GetInfo,必须跑在 Temporal 的 activity 环境里。
// 用 context.Background() 写的测试取不到真实 workflow ID,证明不了生产行为。
func TestInjectUsesActivityWorkflowID(t *testing.T) {
	var env testsuite.TestActivityEnvironment
	suite := &testsuite.WorkflowTestSuite{}
	env = *suite.NewTestActivityEnvironment()
	env.SetWorkflowInfo(...)  // workflow ID = "device-test-grp/p-gabcd1234-p42"
	acts := &Acts{...}
	env.RegisterActivity(acts.NotifyCard)

	_, err := env.ExecuteActivity(acts.NotifyCard, redCardRequest())
	...
	btn := sentButtons(t, sender)
	if btn[0].Value.SourceWorkflowID != "device-test-grp/p-gabcd1234-p42" {
		t.Fatalf("source_workflow_id 必须取自 activity.GetInfo, got %q", btn[0].Value.SourceWorkflowID)
	}
}

// TestInjectEligibilityFromHeaderTemplate:red/orange 注入,
// green 与未知 template 一律 fail-closed 不注入。
func TestInjectEligibilityFromHeaderTemplate(t *testing.T) {
	for tmpl, want := range map[string]bool{
		"red": true, "orange": true, "green": false, "": false, "blue": false, "RED": false,
	} { ... }
}

// TestInjectIgnoresBodyAndFallback:只改正文与 fallback、不改 header 时,
// 注入结果必须不变——证明没有解析它们。
func TestInjectIgnoresBodyAndFallback(t *testing.T) {
	a := renderWith(t, redCardRequest())
	req := redCardRequest()
	req.FallbackText = "完全不同的文本 device-test-other-g0-p0"
	req.Card.Elements[0].Text.Content = "完全不同的正文"
	b := renderWith(t, req)
	if !reflect.DeepEqual(sentButtons(t, a), sentButtons(t, b)) {
		t.Fatal("注入结果依赖了正文或 fallback")
	}
}

// TestSnapshotFailureShipsCardWithoutButtons:存不下原卡就不带按钮发出。
// 发出能点、却存不下原卡的按钮,会让重启后的 sweep 只能拿残缺卡片去覆盖用户的通知。
func TestSnapshotFailureShipsCardWithoutButtons(t *testing.T) {
	st := &fakeStore{snapshotErr: errors.New("db down")}
	... 执行 NotifyCard ...
	if hasButtons(t, sender.lastCard) {
		t.Fatal("快照写失败时不得带按钮")
	}
	if sender.sendCardCalls != 1 {
		t.Fatal("仍应发出展示卡片")
	}
}

// TestSnapshotWrittenBeforeSend:顺序不能反。
func TestSnapshotWrittenBeforeSend(t *testing.T) {
	var order []string
	... 断言 order == []string{"snapshot", "send"} ...
}
```

- [ ] **Step 2: 运行，确认失败**

Run: `cd runtime && go test ./internal/activity/ -run Inject -v`
Expected: FAIL

- [ ] **Step 3: 实现**

`NotifyCard` 在既有逻辑前插入：

```go
// 失败资格只看 header 底色:red = 存在非 INFRA 失败,orange = 失败全是 INFRA,
// green = 无失败。因此 red|orange 恰好等价于"存在 verdict ∉ {PASSED,SKIPPED}"。
// **禁止解析 FallbackText 或正文元素**——那些是展示内容,格式随时可变(设计 §7.1)。
eligible := req.Card.Header.Template == "red" || req.Card.Header.Template == "orange"
workflowID := activity.GetInfo(ctx).WorkflowExecution.ID

if eligible && workflowID != "" && a.CardActions != nil && a.CardActions.Ready() {
	raw, err := json.Marshal(req.Card)
	if err == nil {
		err = a.Store.PutCardSnapshot(ctx, workflowID, raw)
	}
	if err != nil {
		// 存不下原卡就不带按钮:发出能点、却无法更新的按钮是更坏的结果
		a.warnf("card snapshot failed: %v; sending display card without buttons", err)
	} else {
		injectButtons(&req.Card, workflowID)
	}
}
```

- [ ] **Step 4: 运行，确认通过**

Run: `cd runtime && go test ./internal/activity/ -count=1 -v`
Expected: PASS（含既有 `notify_card_test.go`）

- [ ] **Step 5: 提交**

```bash
git add runtime/internal/activity/notify_card.go runtime/internal/activity/notify_card_inject_test.go
git commit -m "feat(activity): inject card buttons at send time

Eligibility comes from the header template and the workflow ID comes from
the activity context. Neither is parsed out of the card body or the
fallback text: those are presentation, they change, and a button bound to
a workflow ID scraped from a string would eventually point somewhere
else.

The snapshot is written before a clickable card goes out, and if that
write fails the card ships without buttons. A button we cannot later
update leaves the sweep with nothing to patch."
```

---

## Task 12: 装配与端到端

**Files:**
- Modify: `runtime/internal/feishucmd/listener.go`、`runtime/cmd/worker/main.go`
- Test: `runtime/internal/feishucmd/listener_test.go`

**Interfaces:**
- Consumes: `cardaction.Handler.OnCardAction`、`cardaction.Readiness.SetWS`、
  `cardaction.Sweeper.Run`、`cardaction.Consumer.ConsumeOne`
- Produces: `Listener` 增字段 `Card *cardaction.Handler`、`Readiness *cardaction.Readiness`；
  新环境变量 `FEISHU_CARD_ACTIONS_ENABLED`（缺省 `false`）

- [ ] **Step 1: listener 注册回调与五个生命周期钩子**

```go
handler := dispatcher.NewEventDispatcher("", "").
	OnP2MessageReceiveV1(...).                       // 既有
	OnP2CardActionTrigger(l.Card.OnCardAction).      // 新增
	OnP2MessageReadV1(...)

cli := larkws.NewClient(l.AppID, l.AppSecret,
	larkws.WithEventHandler(handler),
	larkws.WithOnReady(func() { l.Readiness.SetWS(true) }),
	larkws.WithOnReconnected(func() { l.Readiness.SetWS(true) }),
	larkws.WithOnReconnecting(func() { l.Readiness.SetWS(false) }),
	larkws.WithOnDisconnected(func() { l.Readiness.SetWS(false) }),
	larkws.WithOnError(func(error) { l.Readiness.SetWS(false) }),
	larkws.WithLogLevel(larkcore.LogLevelError),
	larkws.WithAutoReconnect(true),
)
defer l.Readiness.SetWS(false)   // ctx 取消后关闭交互能力
```

- [ ] **Step 2: worker 装配**

`cmd/worker/main.go` 里构造 `cardaction.Readiness`（五项）、`Handler`、`Consumer`、`Sweeper`，
并在 listener 之外起 sweeper goroutine。新增环境变量 `FEISHU_CARD_ACTIONS_ENABLED`（缺省 `false`）。

- [ ] **Step 3: 全量回归**

Run: `cd runtime && go vet ./... && go test ./... -count=1`
Expected: 全绿

- [ ] **Step 4: 提交**

```bash
git add runtime/internal/feishucmd/listener.go runtime/cmd/worker/main.go
git commit -m "feat(worker): wire card actions into the listener

Readiness tracks the socket through the SDK lifecycle hooks, so a card
goes out with buttons only while the connection that would service them
is actually up. It can only ever promise readiness at send time, which is
why the callback re-checks."
```

---

## Task 13: 文档与部署

**Files:**
- Modify: `CLAUDE.md`（第 37 行 §3 规则 7、§11 数据模型）
- Modify: `docs/device-test-sequence.md`
- Modify: `deploy/README.md`、`deploy/.env.example`
- Modify: `deploy/tests/test_deploy_contracts.py`

**Interfaces:**
- Consumes: 无代码依赖（纯文档），但断言的字符串是契约：
  `FEISHU_CARD_ACTIONS_ENABLED`、`card.action.trigger`、两段部署顺序串
- Produces: 无导出符号

- [ ] **Step 1: CLAUDE.md §3 规则 7 开口子**

第 37 行改为（保留原句，追加限定）：

> 7. 所有跨组件动作携带幂等键；所有 Hermes/人工决策落 `decisions` 表（**例外：终态卡片确认是
>    run 级动作，记于 `card_actions` + `audit_log`，不属于 task-level decision**）；所有操作落 `audit_log`。

§11 数据模型补五张表。

- [ ] **Step 2: device-test-sequence.md**

写明终态卡片确认**不投 workflow signal、不走 outbox**——终态通知发出时 workflow 已结束。

- [ ] **Step 3: deploy/README.md 两段部署**

第一段（随 workflow-runs 首发）：只有 `update_multi`，卡片仍无按钮。
第二段（workflow-runs rollout 稳定后）：迁移五张表 + 打开 `FEISHU_CARD_ACTIONS_ENABLED`。
**明确写出不得混入 workflow_runs 停写窗口的理由。**
补充 `card.action.trigger` 需在开放平台「事件与回调 → 回调」中订阅，订阅方式为长连接。

- [ ] **Step 4: deploy 契约测试**

在 `test_deploy_contracts.py` 加断言：README 含两段顺序串、含
`FEISHU_CARD_ACTIONS_ENABLED`、含 `card.action.trigger`。

- [ ] **Step 5: 运行**

Run: `python3 -m pytest deploy/tests/test_deploy_contracts.py -q`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add CLAUDE.md docs/device-test-sequence.md deploy/README.md deploy/.env.example \
        deploy/tests/test_deploy_contracts.py
git commit -m "docs: record card actions in the context and deploy docs

Rule 7 said every human decision lands in decisions, which a run-level
card acknowledgment cannot satisfy: decisions is keyed by task and a
click covers a whole run. The exception is written down rather than left
for the next person to rediscover as an FK violation.

Rollout stays in two stages, and the reason the second one must not join
the workflow_runs stop-write window is stated: that window already
carries the artifact key change and the bridge cutover, and a third
moving part would make failures unattributable."
```

---

## Self-Review

**Spec 覆盖：** §1 实测结论 → Task 8/11 的判据；§2 语义 → Task 2/9/10；§3 五张表 → Task 3–6；
§4 Resolver → Task 2；§5 接受事务 → Task 5；§6 回调 → Task 8/9；§7 卡片 → Task 1/10/11；
§8 恢复 → Task 10；§9 代码边界 → 文件结构表；§10 测试 → 各 task 的 Step 1；§11 部署 → Task 13。

**已知取舍（实施时注意）：**
- Task 5 的 `CompleteAccept` 需要在事务内拿到水位 N 后再构造 `target_input`，
  因此接口带一个 `func(attempt int) ([]byte, string, error)` 回调。这是 §5.5 逐字段断言
  与"一次性 INSERT 完整行"两个约束共同逼出来的形状，不是可选设计。
- `RenderCard` 对快照做 JSON round-trip 会重排键顺序。测试用**结构比较**（`reflect.DeepEqual`
  于 `map[string]any`）而非字节比较，spec §10.4a 的"逐字节相同"应理解为**语义等价**——
  实施时若发现飞书对键顺序敏感，改为保序解析并在此处标 `CONTRACT-ISSUE:`。

---

## 执行方式

Plan complete and saved to `docs/superpowers/plans/2026-07-31-feishu-card-actions.md`. Two execution options:

**1. Subagent-Driven (recommended)** - 每个 task 派发全新 subagent，task 之间我做 review，迭代快

**2. Inline Execution** - 在本会话内按 executing-plans 批量执行，带 checkpoint

Which approach?
