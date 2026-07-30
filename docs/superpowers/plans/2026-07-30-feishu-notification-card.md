# 飞书终态通知卡片化 实施计划（展示部分）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把终态通知从纯文本换成结构化卡片，颜色一眼区分"查代码"与"查环境"，且不引入任何交互组件。

**Architecture:** workflow 侧生成封闭 DTO 卡片 + 降级文本，经新增的 `NotifyCard` 活动发出；`Notify` 与 `buildNotification` 原样保留供旧版本分支与降级路径使用。`feishu` 包新增 `CardSender` 接口而不扩展 `Sender`。

**Tech Stack:** Go 1.22+、Temporal Go SDK（`worker.WorkflowReplayer`）、飞书开放平台卡片。

**设计文档:** `docs/superpowers/specs/2026-07-30-feishu-notification-card-design.md`（已批准，v6）

## Global Constraints

- **任务顺序不可调换**：Task 1 必须在**任何生产代码改动之前**完成——它要录制改动前的
  workflow history 作为 fixture，顺序反了就永远录不到"改动前"的历史。
- `Notify(ctx context.Context, text string) error` 的**签名与行为一个字节不改**；
  `notify_test.go` 与 `feishucmd/executor_test.go` 的既有 fake **一行不改即编译通过**。
- **不扩展 `feishu.Sender`**，另加 `CardSender`（`Sender` 嵌入 + `SendCard`）。
- 卡片结构必须逐字采用设计 §4.4 的封闭 DTO。序列化后允许出现的 JSON key **仅九个**：
  `config`、`wide_screen_mode`、`header`、`title`、`template`、`elements`、`tag`、`text`、`content`。
- `CardElement.Tag ∈ {div, hr}`；`CardText.Tag` 恒为 `plain_text`；
  `CardHeader.Template ∈ {green, red, orange}`。
- header 颜色：无可判定失败 → 绿；存在非 `INFRA_ERROR` 的可判定失败 → **红**；
  可判定失败全部是 `INFRA_ERROR` → 橙。**可判定失败 = `verdict ∉ {PASSED, SKIPPED}`**。
- 卡片字段逐条对齐设计 §4.3 的对照表；**唯一有意偏离**是 `SKIPPED` 不显示 attempt。
- 单个 `Reason` / `Summary` 上限 **500 rune**，按 **rune 边界**截断（按字节切会产生半个汉字）。
- 卡片总大小判据 `len(json.Marshal(card)) <= 30*1024`；加省略标注后**重新测量**。
- `NotifyCard` 的执行顺序固定（设计 §5.2）：nil → 静默；非 `CardSender` → 发文本且
  **`SendCard` 零调用**；超预算 → 发文本且 **`SendCard` 零调用**；否则发卡片；卡片失败 → 发文本。
- Go 错误用 wrapped errors；注释中文；提交信息英文。

**Go 工具链：`/home/maxin/.local/go/bin/go`（go1.26.5）。**
不要下载临时工具链——本机已装。`export` 不跨 subagent 或独立 shell 保留，
所以**每条命令都要自带环境**，形如：

```bash
cd /home/maxin/Code/hermes_ai_devops/runtime && \
  PATH=/home/maxin/.local/go/bin:$PATH GOCACHE=/tmp/hermes-runtime-go-cache \
  go test ./...
```

`runtime/` 与 `agent/` 是**两个独立 module**，各自 `cd` 后再跑。

验证用 `go vet ./...` 而非只 `go build`（后者不编译测试文件，看不见测试包的编译错误）。
格式化用同一套工具链的 `gofmt -w`——它对中文注释与 ASCII 引号是安全的（已实测）。

---

### Task 1: 录制改动前的 history fixture（必须最先做）

**Files:**
- Create: `runtime/internal/workflow/testdata/history-pre-notify-card.json`
- Create: `runtime/internal/workflow/record_history_test.go`（**带 build tag**，只负责录制）
- Create: `runtime/internal/workflow/replay_test.go`（**无 tag**，永远进 CI）

**两个文件必须分开。** build tag 作用于**整个文件**——把录制器和重放测试放同一个文件、
再给文件加 tag，会让那条永久的 `TestReplayPreNotifyCardHistory` 也不进普通 CI，
本轮最重要的 DoD 就此空转。

**Interfaces:**
- Consumes: 无
- Produces: fixture 文件 + 一个当前即通过的 `WorkflowReplayer` 测试

**这个任务为什么必须最先做**：它录的是**改动前**的 workflow history。等 Task 5 改了
workflow 代码，就再也录不到这份历史了，而设计 §6 把"旧 history 重放不失败"定为 DoD。
先让它在**当前代码上通过**，Task 5 之后它仍须通过——这才构成回归判据。

- [ ] **Step 1: 写录制器**

在 `record_history_test.go`（文件首行 `//go:build record_history`）里写录制测试，
避免每次 CI 都起 dev server。用 `testtemporal.StartDevServer(t)` 起服务，注册 `DeviceTestWorkflow` 与
一组 fake activity（照 `devicetest_test.go` 既有的 `fakeActs` 写法），跑一个能走完
"选变体 → 一个变体 PASSED → Notify" 的最小输入，然后用 client 拉取 history 并写文件。

**录制器必须拒绝覆盖已存在的 fixture**：

`testdata/` 目录**当前不存在**，而 `os.OpenFile` 不会创建父目录。写盘必须按此固定顺序：

1. 拉取 history 并序列化到内存（`data []byte`）
2. **先校验**内容合格（含 `WorkflowExecutionStarted` 与 `WorkflowExecutionCompleted`）——
   不合格就根本不该落盘
3. `os.MkdirAll("testdata", 0o755)`
4. `os.OpenFile(..., O_CREATE|O_EXCL|O_WRONLY, 0o644)` **原子**取得首次写入权
5. 写入 → 检查 `Write` 与 `Close` 的返回
6. **任何一步失败都要删掉半成品**——否则下次录制会被 `O_EXCL` 永久挡住，
   而挡它的是一个残缺文件

用 `O_EXCL` 而不是 `os.Stat` 预检查：后者是"检查后写入"竞态，且没处理 `Stat` 返回
非 `NotExist` 错误的情形。

```go
	const fixture = "testdata/history-pre-notify-card.json"

	hist := fetchHistory(t, c, wfID, runID) // 步骤 1a:*historypb.History

	// 步骤 2:按 EventType **结构化**校验,不要字节搜索序列化结果。
	// protojson 把 enum 写成 proto 名(EVENT_TYPE_WORKFLOW_EXECUTION_STARTED,
	// 见 go.temporal.io/api enums/v1/event_type.pb.go),搜 "WorkflowExecutionStarted"
	// 会拒掉每一份**合法**的 history。
	var started, completed bool
	for _, ev := range hist.GetEvents() {
		switch ev.GetEventType() {
		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED:
			started = true
		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_COMPLETED:
			completed = true
		}
	}
	if !started || !completed {
		t.Fatalf("history 不完整(started=%v completed=%v, %d 事件),拒绝落盘",
			started, completed, len(hist.GetEvents()))
	}

	data := serializeHistory(t, hist) // 步骤 1b
	if err := os.MkdirAll(filepath.Dir(fixture), 0o755); err != nil {
		t.Fatalf("创建 testdata 目录失败: %v", err) // 步骤 3
	}
	f, err := os.OpenFile(fixture, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644) // 步骤 4
	if errors.Is(err, os.ErrExist) {
		t.Fatalf("%s 已存在,拒绝覆盖。这份 history 必须是**改动前**录的;"+
			"Task 5 之后再跑本录制器会把它替换成改动后的历史,"+
			"重放测试就再也发现不了版本分支的问题。要重录请先手工删除。", fixture)
	}
	if err != nil {
		t.Fatalf("创建 %s 失败: %v", fixture, err)
	}
	// 步骤 6:失败即删半成品,否则 O_EXCL 会被一个残缺文件永久占住
	ok := false
	defer func() {
		cerr := f.Close()
		if ok && cerr == nil {
			return
		}
		// 删不掉半成品是硬问题:它会被 O_EXCL 永久占住,从此录不了新 fixture。
		if rerr := os.Remove(fixture); rerr != nil && !os.IsNotExist(rerr) {
			t.Errorf("删除半成品 %s 失败,后续录制会被 O_EXCL 永久挡住,请手工删除: %v",
				fixture, rerr)
		}
		if cerr != nil {
			t.Errorf("关闭 %s 失败: %v", fixture, cerr)
		}
	}()
	if _, err := f.Write(data); err != nil { // 步骤 5
		t.Fatalf("写入 %s 失败(将删除): %v", fixture, err)
	}
	ok = true
```

这不是防呆，是防一种**会静默毁掉回归判据**的操作：文件被覆盖后重放测试照样绿，
但它验证的已经不是"在途 workflow 能否重放"了。

关键：**history 必须能被 `worker.WorkflowReplayer` 读回**。先读 vendored SDK 确认
replayer 提供哪些入口（`ReplayWorkflowHistory(logger, *historypb.History)` 与
`ReplayWorkflowHistoryFromJSONFile(logger, path)` 两者的可用性与期望格式），**按实际
API 决定序列化方式**，不要凭记忆写。若 `FromJSONFile` 期望的是 `temporal workflow show`
的特定格式而我们难以复现，就用 in-memory 路径：把 `*historypb.History` 用 protojson
序列化存盘，测试里再反序列化回来喂 `ReplayWorkflowHistory`。两条路都可接受，报告里
写明你选了哪条以及为什么。

- [ ] **Step 2: 跑录制，产出 fixture**

```bash
cd /home/maxin/Code/hermes_ai_devops/runtime && \
  PATH=/home/maxin/.local/go/bin:$PATH GOCACHE=/tmp/hermes-runtime-go-cache \
  go test -tags record_history ./internal/workflow \
    -run '^TestRecordPreNotifyCardHistory$' -count=1 -v
```

`-count=1` 不是可选的：录制器一旦被测试缓存命中就会被跳过，而它有写文件的副作用，
"跳过"看起来像成功但什么都没产出。

Expected: 生成 `runtime/internal/workflow/testdata/history-pre-notify-card.json`，
非空且包含 `WorkflowExecutionStarted` 与 `WorkflowExecutionCompleted` 事件

- [ ] **Step 3: 写重放测试（当前代码即须通过）**

```go
// 重放改动前录制的 history(设计 §6 的 DoD)。
// 本测试在 notify-card 改动**之前**就必须通过——它是回归判据:
// 改完 workflow 后它仍须通过,才说明在途 workflow 不会因版本分支而重放失败。
func TestReplayPreNotifyCardHistory(t *testing.T) {
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(DeviceTestWorkflow)
	if err := replayer.ReplayWorkflowHistoryFromJSONFile(nil,
		"testdata/history-pre-notify-card.json"); err != nil {
		t.Fatalf("重放改动前的 history 失败: %v", err)
	}
}
```

（若 Step 1 选了 in-memory 路径，这里改成读文件 → protojson 反序列化 →
`ReplayWorkflowHistory`。断言不变。）

- [ ] **Step 4: 跑测试确认通过**

Run: `cd runtime && PATH=/home/maxin/.local/go/bin:$PATH GOCACHE=/tmp/hermes-runtime-go-cache go test ./internal/workflow/ -run TestReplayPreNotifyCardHistory -v`
Expected: PASS —— **在生产代码尚未改动的情况下通过**，证明 fixture 与 harness 都可用

- [ ] **Step 5: 格式化并 Commit**

```bash
/home/maxin/.local/go/bin/gofmt -w \
  runtime/internal/workflow/replay_test.go runtime/internal/workflow/record_history_test.go
git add runtime/internal/workflow/testdata/ \
        runtime/internal/workflow/replay_test.go \
        runtime/internal/workflow/record_history_test.go
git commit -m "test(workflow): record pre-change history fixture for replay"
```

---

### Task 2: `CardSender` 与两种 `SendCard`

**Files:**
- Modify: `runtime/internal/feishu/feishu.go`
- Modify: `runtime/internal/feishu/feishu_test.go`

**Interfaces:**
- Consumes: 无
- Produces:
  - `type feishu.CardSender interface { Sender; SendCard(ctx context.Context, card any) error }`
  - `webhookSender.SendCard` / `appSender.SendCard`

- [ ] **Step 1: 写失败的测试**

沿用 `feishu_test.go` 既有的 httptest 写法，精确断言两种 wire 形态（它们**不对称**）：

```go
// webhook:content 是对象,卡片走顶层 card 字段。
func TestWebhookSendCardWireShape(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()
	s, _ := NewSender(Config{WebhookURL: srv.URL})
	cs, ok := s.(CardSender)
	if !ok {
		t.Fatal("webhookSender 应实现 CardSender")
	}
	if err := cs.SendCard(context.Background(), map[string]any{"header": "x"}); err != nil {
		t.Fatal(err)
	}
	if got["msg_type"] != "interactive" {
		t.Errorf("msg_type = %v, want interactive", got["msg_type"])
	}
	if _, isObj := got["card"].(map[string]any); !isObj {
		t.Errorf("webhook 的 card 应是对象, got %T", got["card"])
	}
}

// app:content 是**序列化后的字符串**(与 SendText 同形),不是对象。
func TestAppSendCardWireShape(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t","expire":7200}`))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()
	s := newAppSenderForTest(t, srv.URL) // 照既有测试构造 appSender 的写法
	card := map[string]any{"header": "x"}
	if err := s.SendCard(context.Background(), card); err != nil {
		t.Fatal(err)
	}
	if got["msg_type"] != "interactive" {
		t.Errorf("msg_type = %v, want interactive", got["msg_type"])
	}
	str, isStr := got["content"].(string)
	if !isStr {
		t.Fatalf("app 的 content 应是序列化字符串, got %T", got["content"])
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(str), &back); err != nil {
		t.Fatalf("content 不是合法 JSON: %v", err)
	}
	if !reflect.DeepEqual(back, card) {
		t.Errorf("content 解析回来应等于原卡片, got %v", back)
	}
}

// token 过期 → 强制刷新**并且只重试一次**(与 SendText 同款)。
func TestAppSendCardRefreshesExpiredToken(t *testing.T) {
	var tokenCalls, msgCalls int
	// 首个 messages 请求返回 token 失效码,第二个成功。
	// 两个计数都要断言:只断言 token=2 挡不住"消息被重试很多次"的实现。
	// 期望 tokenCalls == 2(首次 + 强制刷新)、msgCalls == 2(首次失败 + 重试一次)。
}
```

`newAppSenderForTest` / token 失效码常量按 `feishu_test.go` 既有用例的写法来——先读文件。

- [ ] **Step 2: 跑测试确认失败**

Run: `cd runtime && PATH=/home/maxin/.local/go/bin:$PATH GOCACHE=/tmp/hermes-runtime-go-cache go test ./internal/feishu/ -run SendCard`
Expected: 编译失败 —— `CardSender` / `SendCard` 未定义

- [ ] **Step 3: 加接口**

`feishu.go` 的 `Sender` 定义**下方**（`Sender` 本身一字不改）：

```go
// CardSender 是能发交互卡片的 Sender(终态通知卡片化)。
// 单独成接口而非往 Sender 上加方法:后者会让 activity/notify_test.go 与
// feishucmd/executor_test.go 里只实现 SendText 的既有 fake 直接编译失败。
type CardSender interface {
	Sender
	SendCard(ctx context.Context, card any) error
}
```

- [ ] **Step 4: 两种实现**

webhook（`content` 是对象，卡片走顶层 `card`）：

```go
// SendCard 发交互卡片。webhook 自定义机器人的卡片走顶层 card 字段。
func (s *webhookSender) SendCard(ctx context.Context, card any) error {
	return post(ctx, s.cfg, s.cfg.WebhookURL, nil, map[string]any{
		"msg_type": "interactive",
		"card":     card,
	})
}
```

app（`content` 必须是**序列化后的字符串**，与既有 `sendMessage` 同形）。把
`sendMessage` 抽成能带 `msgType` 与已序列化 `content` 的内部函数，`SendText` 与
`SendCard` 各自调用——**`SendText` 的行为与载荷必须逐字不变**（既有测试是判据）。
token 过期重试那段逻辑同样复用，不要复制粘贴出第二份。

- [ ] **Step 5: 跑测试**

Run: `cd runtime && PATH=/home/maxin/.local/go/bin:$PATH GOCACHE=/tmp/hermes-runtime-go-cache sh -c 'go vet ./... && go test ./internal/feishu/ -v' 2>&1 | tail -20`
Expected: 新用例全 PASS，**既有 `SendText` 用例一条未改即通过**

- [ ] **Step 6: Commit**

```bash
/home/maxin/.local/go/bin/gofmt -w \
  runtime/internal/feishu/feishu.go runtime/internal/feishu/feishu_test.go
git add runtime/internal/feishu/
git commit -m "feat(feishu): add CardSender with per-mode wire shapes"
```

---

### Task 3: 封闭 DTO 与 `buildNotificationCard`

**Files:**
- Modify: `runtime/internal/workflow/devicetest.go`
- Modify: `runtime/internal/workflow/devicetest_test.go`

**Interfaces:**
- Consumes: 无
- Produces:
  - `NotificationCard` / `CardConfig` / `CardHeader` / `CardElement` / `CardText`（设计 §4.4 逐字）
  - `func buildNotificationCard(in DeviceTestInput, out *DeviceTestOutput) NotificationCard`

- [ ] **Step 1: 写失败的测试**

三组，全部表驱动。**颜色**（设计 §4.1）：

```go
func TestBuildNotificationCardHeaderColor(t *testing.T) {
	cases := []struct {
		name     string
		verdicts []string
		want     string
	}{
		{"全 PASSED", []string{"PASSED", "PASSED"}, "green"},
		{"全 SKIPPED", []string{"SKIPPED"}, "green"},
		{"PASSED + SKIPPED", []string{"PASSED", "SKIPPED"}, "green"},
		{"只有 INFRA", []string{"INFRA_ERROR"}, "orange"},
		{"INFRA + SKIPPED", []string{"INFRA_ERROR", "SKIPPED"}, "orange"},
		{"只有 TEST_FAILED", []string{"TEST_FAILED"}, "red"},
		{"INFRA + TEST_FAILED", []string{"INFRA_ERROR", "TEST_FAILED"}, "red"}, // 业务失败优先
		{"无变体", nil, "green"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := &DeviceTestOutput{}
			for i, v := range tc.verdicts {
				out.Tasks = append(out.Tasks, TaskSummary{
					Variant: fmt.Sprintf("v%d", i), Verdict: v, Attempt: 1})
			}
			card := buildNotificationCard(DeviceTestInput{Project: "p"}, out)
			if card.Header.Template != tc.want {
				t.Errorf("template = %q, want %q", card.Header.Template, tc.want)
			}
		})
	}
}
```

**空任务正文**——颜色断言挡不住"绿 header + 空 Elements"这种实现：

```go
// out.Tasks 为空时,正文必须是与纯文本同款的提示,而不是什么都不放。
func TestBuildNotificationCardEmptyTasks(t *testing.T) {
	card := buildNotificationCard(DeviceTestInput{Project: "p"}, &DeviceTestOutput{})
	want := []CardElement{
		{Tag: "div", Text: &CardText{Tag: "plain_text",
			Content: "无可测变体(Android 包缺失或未配置)"}},
	}
	if !reflect.DeepEqual(card.Elements, want) {
		t.Errorf("空任务正文不匹配\ngot:\n%swant:\n%s",
			dumpElements(card.Elements), dumpElements(want))
	}
}
```

文案必须与 `buildNotification` 里那句**逐字相同**（设计 §4.3 对照表最后一行）。

**封闭结构**（设计 §4.4；含三份反例证明断言不空转）：

```go
var allowedCardKeys = map[string]bool{
	"config": true, "wide_screen_mode": true, "header": true, "title": true,
	"template": true, "elements": true, "tag": true, "text": true, "content": true,
}

// walkCard 递归检查 key / tag / text 类型;返回全部违规项。
func walkCard(t *testing.T, v any) []string { /* 递归 map/slice,收集违规 */ }

func TestCardIsClosedStructure(t *testing.T) {
	card := buildNotificationCard(sampleInput(), sampleOutput())
	raw, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	if bad := walkCard(t, generic); len(bad) != 0 {
		t.Errorf("卡片出现集合外结构: %v", bad)
	}
}

// 反例:证明上面的遍历不是空转。
func TestCardClosedStructureCatchesViolations(t *testing.T) {
	cases := []struct{ name string; payload string }{
		{"带 actions", `{"header":{"title":{"tag":"plain_text","content":"x"}},"actions":[]}`},
		{"带 behaviors", `{"elements":[{"tag":"div","behaviors":[{"type":"open_url"}]}]}`},
		{"lark_md 文本", `{"elements":[{"tag":"div","text":{"tag":"lark_md","content":"x"}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var generic any
			if err := json.Unmarshal([]byte(tc.payload), &generic); err != nil {
				t.Fatal(err)
			}
			if bad := walkCard(t, generic); len(bad) == 0 {
				t.Error("这份卡片应被判违规,断言空转了")
			}
		})
	}
}
```

**字段精确值**（设计 §4.3）——**逐元素比对整个 `Elements` 切片**：

`strings.Contains` 挡不住子串误通过：`380.14` 会通过对 `"380.1"` 的检查，
`attempt 20` 会通过对 `"attempt 2"` 的检查。所以必须构造完整的 `want []CardElement`
并整体比对（`Tag`、`Text.Tag`、`Content`、**顺序**）。

**先把正文格式钉死**，否则黄金测试写不出来：

| 行 | 格式 | 出现条件 |
|---|---|---|
| 主行 | `{variant}  {verdict}`，非 PASSED 且 `Category != ""` 时追加 `({category})` | 每个变体总有 |
| 指标行 | 由存在的部分用 ` · ` 连接：`{duration:%.1f}s`、`cases {passed}/{total}`（两者同受 `CasesTotal > 0` 门控）、`attempt {n}`（`SKIPPED` 时省略） | 至少一部分存在时才有该行 |
| reason 行 | `{reason}` | `Reason != ""` |
| hermes 行 | `hermes: {summary}` | `Analysis != nil && Summary != ""` |

变体之间用 `{Tag: "hr"}` 分隔（首个变体前不加）。

```go
func TestBuildNotificationCardGolden(t *testing.T) {
	in := DeviceTestInput{Project: "algo-super-sdk", Commit: "9da3b9d9",
		PipelineID: 56, Version: "1.4.2"}
	out := &DeviceTestOutput{Tasks: []TaskSummary{
		{Variant: "aarch64_Android_SNPE_2.21", Verdict: "PASSED",
			DurationSec: 412.3, CasesTotal: 38, CasesFailed: 0, Attempt: 1},
		{Variant: "aarch64_Android_SNPE_1.68", Verdict: "TEST_FAILED", Category: "CODE",
			DurationSec: 380.14, CasesTotal: 38, CasesFailed: 3, Attempt: 2,
			Reason:   "three cases crashed",
			Analysis: &hermesclient.Analysis{Summary: "DSP 初始化崩溃"}},
		{Variant: "aarch64_Linux_RKNN_2.3.2", Verdict: "SKIPPED",
			Reason: "fleet 无匹配设备"},
	}}

	card := buildNotificationCard(in, out)

	if want := "[hermes-devops] algo-super-sdk g9da3b9d9 p56 (v1.4.2)"; card.Header.Title.Content != want {
		t.Errorf("header content = %q, want %q", card.Header.Title.Content, want)
	}
	if card.Header.Title.Tag != "plain_text" {
		t.Errorf("header tag = %q, want plain_text", card.Header.Title.Tag)
	}
	if card.Header.Template != "red" { // INFRA 不在场,存在 TEST_FAILED → red
		t.Errorf("template = %q, want red", card.Header.Template)
	}

	txt := func(c string) *CardText { return &CardText{Tag: "plain_text", Content: c} }
	want := []CardElement{
		{Tag: "div", Text: txt("aarch64_Android_SNPE_2.21  PASSED")},
		{Tag: "div", Text: txt("412.3s · cases 38/38 · attempt 1")},
		{Tag: "hr"},
		{Tag: "div", Text: txt("aarch64_Android_SNPE_1.68  TEST_FAILED(CODE)")},
		{Tag: "div", Text: txt("380.1s · cases 35/38 · attempt 2")}, // %.1f;passed=38-3
		{Tag: "div", Text: txt("three cases crashed")},
		{Tag: "div", Text: txt("hermes: DSP 初始化崩溃")},
		{Tag: "hr"},
		{Tag: "div", Text: txt("aarch64_Linux_RKNN_2.3.2  SKIPPED")}, // SKIPPED 无 category
		{Tag: "div", Text: txt("fleet 无匹配设备")},                     // 无指标行:CasesTotal=0 且 SKIPPED 省 attempt
	}
	if !reflect.DeepEqual(card.Elements, want) {
		t.Errorf("elements 不匹配\ngot  (%d):\n%s\nwant (%d):\n%s",
			len(card.Elements), dumpElements(card.Elements), len(want), dumpElements(want))
	}
}

// dumpElements 把切片逐行打出来,便于看出是哪一行/哪个字符不同。
func dumpElements(es []CardElement) string {
	var b strings.Builder
	for i, e := range es {
		if e.Text == nil {
			fmt.Fprintf(&b, "  [%d] tag=%s\n", i, e.Tag)
			continue
		}
		fmt.Fprintf(&b, "  [%d] tag=%s texttag=%s content=%q\n", i, e.Tag, e.Text.Tag, e.Text.Content)
	}
	return b.String()
}
```

用 `reflect.DeepEqual` 而非 `cmp.Diff`：`go-cmp` 只在 `go.sum`（间接依赖），
直接用它要动 `go.mod`；`dumpElements` 已经能给出可读的差异定位。

这条断言同时覆盖了顺序、`hr` 的位置、`%.1f` 舍入（`380.14` → `380.1`）、
`passed = CasesTotal - CasesFailed`（35 而非 3）、以及 SKIPPED 那两处省略。

**出现条件**（在黄金卡片之外，各一条最小用例）：

- `CasesTotal == 0` → 指标行**既无 duration 也无 cases**（同一门控）；`> 0` → 两者都有
- `Category == ""` 或 `Verdict == PASSED` → 无 category
- `Reason == ""` → 无 reason 行；`Analysis == nil || Summary == ""` → 无 hermes 行
- `Verdict == SKIPPED` → **无 attempt**；其余 verdict → 有 attempt

**渲染安全与截断**（设计 §4.5）：

- 超 500 rune 的 `Reason` 被截断并带省略标记
- **超 500 rune 的 `Analysis.Summary` 同样被截断并带省略标记**（与 `Reason` 对称，
  两者都要有；只测 `Reason` 会漏掉 Summary 这条路径）
- **纯中文超长 `Reason` 截断后 `utf8.ValidString` 为真**（按 rune 切的判据）
- **纯中文超长 `Summary` 同上**
- `Project = "a[x](http://evil)b"`、`Variant = "v<at user_id=\"all\">"`、
  `Reason`、`Summary` 各含 markdown/`<at>` → 卡片里是字面文本，节点 `tag == "plain_text"`
- 构造超预算输入 → `len(json.Marshal(card)) <= 30*1024`，且带"详情已省略"标注
- **裁剪必须从末尾删，且有会红的断言**。只检查"最终大小 + 有标注"是不够的：
  从头删、随机删、把所有详情都删光，三种错误实现都能通过。用两个各带**唯一可识别详情**
  的变体构造临界输入：

```go
// 裁剪必须保留前面变体的详情、只丢末尾的(设计 §4.5 第 1 步)。
func TestBuildNotificationCardTrimsFromTail(t *testing.T) {
	// 两个变体,reason 各带唯一标记,长度调到"两个都留会超预算、只留第一个不超"
	out := &DeviceTestOutput{Tasks: []TaskSummary{
		{Variant: "v-first", Verdict: "TEST_FAILED", Attempt: 1,
			Reason: "FIRST-MARKER" + strings.Repeat("甲", 400)},
		{Variant: "v-last", Verdict: "TEST_FAILED", Attempt: 1,
			Reason: "LAST-MARKER" + strings.Repeat("乙", 400)},
	}}
	// 用足够多的变体把总量顶过预算(见下方 padTo 辅助),但两个带标记的排在最前与最后
	card := buildNotificationCard(DeviceTestInput{Project: "p"}, padTo(out, 30*1024))
	body := allContent(card)

	if !strings.Contains(body, "FIRST-MARKER") {
		t.Error("裁剪从头删了:前面变体的详情必须保留")
	}
	if strings.Contains(body, "LAST-MARKER") {
		t.Error("末尾变体的详情应被丢弃")
	}
	// 变体本身(主行)不许被删——只删可选行
	for _, v := range []string{"v-first", "v-last"} {
		if !strings.Contains(body, v) {
			t.Errorf("变体 %s 的主行被删了,只应删可选行", v)
		}
	}
	// 省略数量必须与实际丢弃的变体数一致,不能写死
	if !strings.Contains(body, "个变体的详情已省略") {
		t.Error("缺省略标注")
	}
	if n := len(mustMarshal(t, card)); n > 30*1024 {
		t.Errorf("裁剪后仍超预算: %d", n)
	}
}
```

  `padTo` / `allContent` / `mustMarshal` 是本测试文件里的小辅助，由你实现；
  关键是**标记必须唯一且可搜索**，这样"删错了哪一端"能被区分出来。

- [ ] **Step 2: 跑测试确认失败**

Run: `cd runtime && PATH=/home/maxin/.local/go/bin:$PATH GOCACHE=/tmp/hermes-runtime-go-cache go test ./internal/workflow/ -run NotificationCard`
Expected: 编译失败 —— `buildNotificationCard` / `NotificationCard` 未定义

- [ ] **Step 3: 加 DTO**

把设计 §4.4 的五个类型**逐字**加进 `devicetest.go`（含注释——那段注释解释了为什么它是封闭的）。

- [ ] **Step 4: 实现 `buildNotificationCard`**

要点（对齐设计 §4.3/§4.5，不要自由发挥）：

- header 标题与 `buildNotification` 第一行同源：`[hermes-devops] {project} g{commit} p{pipeline} (v{version})`
- 颜色：先算可判定失败集合（`verdict ∉ {PASSED, SKIPPED}`），非空且**存在非 INFRA** → red；
  非空且全 INFRA → orange；空 → green
- **每个变体产出多个 `div`**（不是一个）：主行、指标行、reason 行、hermes 行各自独立，
  出现条件见上方格式表；变体之间插 `{Tag: "hr"}`（首个变体前不插）。
  这一点必须与黄金测试的 `want` 完全一致——把四行塞进一个 `div` 会让黄金比对红
- `Reason` / `Summary` 各自 `truncateRunes(s, 500)`
- 全部文本节点 `Tag: "plain_text"`
- 末尾按 §4.5 的两步裁剪：超预算 → 从末尾变体丢可选行 → 加标注 → **重新 Marshal 测量**

`truncateRunes` 用 `[]rune` 切片，不要用 `s[:n]`。

- [ ] **Step 5: 跑测试**

Run: `cd runtime && PATH=/home/maxin/.local/go/bin:$PATH GOCACHE=/tmp/hermes-runtime-go-cache go test ./internal/workflow/ -v -run 'NotificationCard|CardIsClosed|CardClosedStructure' 2>&1 | tail -30`
Expected: 全部 PASS，含三份反例

- [ ] **Step 6: 验证中文截断用例真的会红**

把 `truncateRunes` 临时改成按字节切（`s[:n]`），跑纯中文超长那条：

Run: `cd runtime && PATH=/home/maxin/.local/go/bin:$PATH GOCACHE=/tmp/hermes-runtime-go-cache go test ./internal/workflow/ -run 'Truncat'`
Expected: **FAIL**（`utf8.ValidString` 为假）。改回来确认恢复 PASS，两次输出记进报告。

- [ ] **Step 7: Commit**

```bash
/home/maxin/.local/go/bin/gofmt -w \
  runtime/internal/workflow/devicetest.go runtime/internal/workflow/devicetest_test.go
git add runtime/internal/workflow/
git commit -m "feat(workflow): build the notification card as a closed DTO"
```

---

### Task 4: `NotifyCard` 活动与固定降级顺序

**Files:**
- Create: `runtime/internal/activity/notify_card.go`
- Create: `runtime/internal/activity/notify_card_test.go`
- Modify: `runtime/internal/workflow/devicetest.go`（`NotifyCardRequest` 类型）

**Interfaces:**
- Consumes: Task 2 的 `feishu.CardSender`、Task 3 的 `NotificationCard`
- Produces:
  - `wf.NotifyCardRequest{Card NotificationCard; FallbackText string}`
  - `func (a *Acts) NotifyCard(ctx context.Context, req wf.NotifyCardRequest) error`

`Acts` 的方法由 `w.RegisterActivity(acts)`（`cmd/worker/main.go:203`）按方法名批量注册，
**不需要额外的注册代码**。

- [ ] **Step 1: 写失败的测试**

fake 要能分别计数 `SendText` 与 `SendCard`。准备两个 fake：一个只实现 `SendText`
（模拟旧 fake），一个实现 `CardSender`。

```go
func TestNotifyCardOrder(t *testing.T) {
	big := wf.NotificationCard{ /* 构造一个序列化后 > 30KB 的卡片 */ }
	small := wf.NotificationCard{ /* 正常大小 */ }
	cases := []struct {
		name       string
		sender     feishu.Sender
		card       wf.NotificationCard
		cardFails  bool
		wantCards  int
		wantTexts  int
	}{
		{"nil sender 静默", nil, small, false, 0, 0},
		{"非 CardSender → 只发文本", &textOnlyFake{}, small, false, 0, 1},
		{"超预算 → 只发文本,SendCard 零调用", &cardFake{}, big, false, 0, 1},
		{"正常 → 只发卡片", &cardFake{}, small, false, 1, 0},
		{"卡片失败 → 降级发文本", &cardFake{}, small, true, 1, 1},
		// 边界必须精确锁定,否则把 > 写成 >= 不会被发现:
		{"恰好 30*1024 → 发卡片", &cardFake{}, cardOfExactSize(t, 30*1024), false, 1, 0},
		{"30*1024+1 → 零调用,降级", &cardFake{}, cardOfExactSize(t, 30*1024+1), false, 0, 1},
	}
	// 断言 wantCards / wantTexts 的**精确调用次数**;
	// "超预算"那条的 wantCards=0 是设计 §5.2 第 3 步的机械判据。
}

// cardOfExactSize 造一个 json.Marshal 后**恰好** n 字节的卡片:
// 先建骨架,再逐字符调整某个 Content 的填充长度直到命中 n。
// 边界用例靠它——只有"正常小卡"和">30KB 大卡"两档时,把 `>` 写成 `>=`
// (或反之)不会有任何测试变红。
func cardOfExactSize(t *testing.T, n int) wf.NotificationCard {
	t.Helper()
	mk := func(pad int) wf.NotificationCard {
		return wf.NotificationCard{
			Header: wf.CardHeader{Title: wf.CardText{Tag: "plain_text", Content: "x"}, Template: "green"},
			Elements: []wf.CardElement{{Tag: "div", Text: &wf.CardText{
				Tag: "plain_text", Content: strings.Repeat("a", pad)}}},
		}
	}
	// 单调:pad 每 +1,序列化长度也 +1(全 ASCII 且无需转义),二分/线性都能命中。
	for pad := 0; pad <= n; pad++ {
		if len(mustMarshal(t, mk(pad))) == n {
			return mk(pad)
		}
	}
	t.Fatalf("造不出恰好 %d 字节的卡片", n)
	return wf.NotificationCard{}
}

// 降级发送本身失败时,错误必须保留 cause(便于排查是 token 还是网络)。
func TestNotifyCardFallbackFailureWrapsCause(t *testing.T) {
	sentinel := errors.New("boom")
	f := &cardFake{failCard: true, failText: sentinel}
	a := &Acts{Feishu: f}
	err := a.NotifyCard(ctx, wf.NotifyCardRequest{FallbackText: "x"})
	if err == nil {
		t.Fatal("降级也失败时必须返回错误")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("错误应保留 cause, got %v", err)
	}
}

// 降级文本必须原样来自载荷,activity 不得自己拼。
func TestNotifyCardFallbackTextIsVerbatim(t *testing.T) {
	f := &cardFake{failCard: true}
	a := &Acts{Feishu: f}
	const want = "任意文本 —— activity 不该改动它"
	if err := a.NotifyCard(ctx, wf.NotifyCardRequest{FallbackText: want}); err != nil {
		t.Fatal(err)
	}
	if len(f.texts) != 1 || f.texts[0] != want {
		t.Errorf("降级文本 = %v, want 原样 %q", f.texts, want)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd runtime && PATH=/home/maxin/.local/go/bin:$PATH GOCACHE=/tmp/hermes-runtime-go-cache go test ./internal/activity/ -run NotifyCard`
Expected: 编译失败 —— `NotifyCard` / `NotifyCardRequest` 未定义

- [ ] **Step 3: 加载荷类型**

`devicetest.go`，逐字采用设计 §5.1 的类型与注释。

- [ ] **Step 4: 实现活动**

严格按设计 §5.2 的五步顺序：

```go
// NotifyCard 发终态通知卡片,失败降级纯文本(设计 §5.2 的固定顺序)。
// 降级文本来自载荷(workflow 侧调 buildNotification 生成),activity **绝不自行拼文本**:
// 两处实现同一格式必然漂移,而"降级内容与改动前逐字节相同"是本轮的验收项。
func (a *Acts) NotifyCard(ctx context.Context, req wf.NotifyCardRequest) error {
	if a.Feishu == nil {
		return nil // 未配置:静默成功(开发模式),与 Notify 一致
	}
	cs, ok := a.Feishu.(feishu.CardSender)
	if !ok {
		// 注入的 Sender 不支持卡片(旧测试 fake):直接降级,不是错误
		return a.sendFallback(ctx, req.FallbackText)
	}
	raw, err := json.Marshal(req.Card)
	if err != nil || len(raw) > cardSizeBudget {
		// 超预算不调 SendCard(设计 §5.2 第 3 步):执行路径唯一,可机械断言
		a.warnf("notification card over budget (%d bytes) or unmarshalable; sending text", len(raw))
		return a.sendFallback(ctx, req.FallbackText)
	}
	if err := cs.SendCard(ctx, req.Card); err != nil {
		a.warnf("feishu send card failed: %v; falling back to text", err)
		return a.sendFallback(ctx, req.FallbackText)
	}
	return nil
}

// cardSizeBudget 见设计 §4.5。判据是 `len(raw) > cardSizeBudget` 才降级——
// **恰好等于**预算是允许发卡片的(边界由 cardOfExactSize 的两条用例锁定)。
const cardSizeBudget = 30 * 1024

func (a *Acts) sendFallback(ctx context.Context, text string) error {
	if err := a.Feishu.SendText(ctx, text); err != nil {
		return fmt.Errorf("feishu notify card fallback: %w", err)
	}
	return nil
}
```

- [ ] **Step 5: 跑测试**

Run: `cd runtime && PATH=/home/maxin/.local/go/bin:$PATH GOCACHE=/tmp/hermes-runtime-go-cache sh -c 'go vet ./... && go test ./internal/activity/ -v -run NotifyCard' 2>&1 | tail -20`
Expected: 全部 PASS；**既有 `notify_test.go` 一条未改即通过**

**为什么降级路径不做手工验收**：卡片与降级文本走**同一个** Sender、同一个 URL，
真实端点造不出"卡片失败但文本可达"。只有这里的 `cardFake`（`SendCard` 失败、
`SendText` 成功）能精确构造该条件，所以降级完全由本任务的自动化测试覆盖。

- [ ] **Step 6: 验证"超预算零调用"真的会红**

把超预算分支临时改成"照常调 `SendCard`，失败再降级"，跑：

Run: `cd runtime && PATH=/home/maxin/.local/go/bin:$PATH GOCACHE=/tmp/hermes-runtime-go-cache go test ./internal/activity/ -run NotifyCardOrder`
Expected: **FAIL** —— "超预算"那条的 `wantCards=0` 不满足。改回来确认恢复 PASS，
两次输出记进报告。

- [ ] **Step 7: Commit**

```bash
/home/maxin/.local/go/bin/gofmt -w \
  runtime/internal/activity/notify_card.go runtime/internal/activity/notify_card_test.go \
  runtime/internal/workflow/devicetest.go
git add runtime/internal/activity/ runtime/internal/workflow/devicetest.go
git commit -m "feat(activity): add NotifyCard with a single over-budget path"
```

---

### Task 5: workflow 接线与版本分支

**Files:**
- Modify: `runtime/internal/workflow/devicetest.go`
- Modify: `runtime/internal/workflow/devicetest_test.go`

**Interfaces:**
- Consumes: Task 3 的 `buildNotificationCard`、Task 4 的 `NotifyCardRequest`
- Produces: workflow 侧的版本分支

- [ ] **Step 1: 写失败的测试**

```go
// 新版本走 NotifyCard,载荷同时带卡片与降级文本;
// FallbackText 必须等于同一输入下 buildNotification 的输出(逐字节)。
func TestWorkflowSendsCardWithVerbatimFallback(t *testing.T) {
	// 用既有 fakeActs 记录 NotifyCard 收到的 req;
	// 断言 req.FallbackText == buildNotification(in, out)
	// 断言 fakeActs 的 Notify(string) **零调用**
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd runtime && PATH=/home/maxin/.local/go/bin:$PATH GOCACHE=/tmp/hermes-runtime-go-cache go test ./internal/workflow/ -run SendsCardWithVerbatim`
Expected: FAIL —— workflow 仍在调 `Notify`

- [ ] **Step 3: 加版本分支**

把 `devicetest.go:286-289` 那段替换为：

```go
	if workflow.GetVersion(ctx, "notify-card", workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		// 在途 workflow:原样发纯文本,载荷与改动前逐字节相同
		if err := workflow.ExecuteActivity(ctx, "Notify", buildNotification(in, out)).Get(ctx, nil); err != nil {
			workflow.GetLogger(ctx).Error("notify failed", "error", err)
		}
	} else {
		req := NotifyCardRequest{
			Card:         buildNotificationCard(in, out),
			FallbackText: buildNotification(in, out), // 原函数,原样调用
		}
		if err := workflow.ExecuteActivity(ctx, "NotifyCard", req).Get(ctx, nil); err != nil {
			workflow.GetLogger(ctx).Error("notify card failed", "error", err)
		}
	}
```

`buildNotification` 因此仍在主路径上被调用，不是死代码。

- [ ] **Step 4: 跑测试 + 重放回归**

Run: `cd runtime && PATH=/home/maxin/.local/go/bin:$PATH GOCACHE=/tmp/hermes-runtime-go-cache go test ./internal/workflow/ -v 2>&1 | tail -25`
Expected: 新用例 PASS，**且 Task 1 的 `TestReplayPreNotifyCardHistory` 仍然 PASS**——
这是版本分支没有破坏在途 workflow 的证据。若它红了，说明 version marker 的位置有问题，
**不要改 fixture 迁就**，改代码。

- [ ] **Step 5: 全量回归**

Run: `cd runtime && PATH=/home/maxin/.local/go/bin:$PATH GOCACHE=/tmp/hermes-runtime-go-cache sh -c 'go build ./... && go vet ./... && go test ./...'`
Expected: 全部 PASS

- [ ] **Step 6: Commit**

```bash
/home/maxin/.local/go/bin/gofmt -w \
  runtime/internal/workflow/devicetest.go runtime/internal/workflow/devicetest_test.go
git add runtime/internal/workflow/
git commit -m "feat(workflow): send notification cards behind a version gate"
```

---

### Task 6: 文档收尾

**Files:**
- Modify: `CLAUDE.md`
- Modify: `deploy/README.md`

**Interfaces:**
- Consumes: 前五个任务
- Produces: 无新导出符号

- [ ] **Step 1: 更正 CLAUDE.md 的两处 signal 描述**

§4 通知那一行：

```markdown
| 通知 | 飞书机器人 + 交互卡片(**按钮回调经 WS listener 执行,不是 workflow signal**——终态通知发出时 workflow 已结束) | 按钮尚未实现,见 §12 Phase 2 |
```

§12 Phase 2 里"重试/忽略/隔离按钮 → Runtime signal"改为：

```markdown
飞书交互卡片(2026-07-30:**展示卡片已实现**;重试/忽略按钮待 `workflow_runs` 落地后单独一轮,
按钮回调经 WS listener 执行而非 workflow signal;隔离按钮因无设备级信号源暂不做,见差距 #10)
```

- [ ] **Step 1b: 更正 CLAUDE.md §12 Phase 1 第 6 条**

`CLAUDE.md:281` 描述 DeviceTestWorkflow 主干时仍以"→ 飞书**纯文本**通知"结尾。
把"纯文本通知"改为"飞书通知(2026-07-30 起为展示卡片,失败降级纯文本)"。
这是本轮之后第三处过时描述，一并改掉——漏了它，读 Phase 1 主干的人拿到的仍是旧结论。

- [ ] **Step 2: 更新 deploy/README.md 的通知一节**

现有那句"Messages are plain text in this version; interactive cards are a later milestone"
已经过时，改为说明：终态通知是卡片，header 底色按 verdict 分三色（绿/红/橙，业务失败优先
于基础设施失败）；卡片发送失败自动降级纯文本；**本轮不含任何按钮**。

- [ ] **Step 3: 全量回归**

Run: `cd runtime && PATH=/home/maxin/.local/go/bin:$PATH GOCACHE=/tmp/hermes-runtime-go-cache sh -c 'go build ./... && go vet ./... && go test ./...'`
Run: `cd /home/maxin/Code/hermes_ai_devops && .venv/bin/python -m pytest contracts/tests deploy/tests -q`
Expected: 全部 PASS

- [ ] **Step 4: Commit**

```bash
git add CLAUDE.md deploy/README.md
git commit -m "docs: notification cards shipped; correct the signal description"
```

---

## 完成后的手工验收

1. 配好 app 模式（或 webhook），触发一次全 PASSED 的 workflow → 收到**绿色**卡片
2. 触发一次含 `TEST_FAILED` 的 → **红色**；含 `INFRA_ERROR` 且无其他失败的 → **橙色**
3. 一次同时含 `INFRA_ERROR` 与 `TEST_FAILED` 的 → **红色**（不是橙）
4. 卡片上**没有任何按钮**
