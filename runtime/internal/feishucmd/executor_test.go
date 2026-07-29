package feishucmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"hermes-devops/runtime/internal/hermesclient"
	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

var ctx = context.Background()

// ---- parser(表驱动)----

func TestParse(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantArgs []string
	}{
		{"status", "status", nil},
		{"  STATUS  ", "status", nil}, // trim + 大小写不敏感
		{"devices", "devices", nil},
		{"rerun abcd1234 42", "rerun", []string{"abcd1234", "42"}},
		{"rerun abcd1234 42 aarch64_Android_SNPE_2.21", "rerun",
			[]string{"abcd1234", "42", "aarch64_Android_SNPE_2.21"}},
		{"unquarantine", "unquarantine", nil},
		{"unquarantine dev-1", "unquarantine", []string{"dev-1"}},
		{"", "help", nil},
		{"   ", "help", nil},
		{"drop database", "help", nil}, // 自由文本/未知 → help,不放大能力
		{"rerun", "rerun", nil},
	}
	for _, tc := range cases {
		got := Parse(tc.in)
		if got.Name != tc.wantName {
			t.Errorf("Parse(%q).Name = %q, want %q", tc.in, got.Name, tc.wantName)
		}
		if len(got.Args) != len(tc.wantArgs) {
			t.Errorf("Parse(%q).Args = %v, want %v", tc.in, got.Args, tc.wantArgs)
		}
	}
}

func TestParseWhitelist(t *testing.T) {
	wl := ParseWhitelist("ou_a, ou_b ,,")
	if !wl["ou_a"] || !wl["ou_b"] || len(wl) != 2 {
		t.Errorf("whitelist = %v", wl)
	}
	if len(ParseWhitelist("")) != 0 {
		t.Error("空白名单应为空集合(listener 不启动)")
	}
}

// ---- executor ----

type fakeStarter struct {
	inputs  []wf.DeviceTestInput
	started bool
	err     error
}

func (f *fakeStarter) StartDeviceTest(_ context.Context, in wf.DeviceTestInput) (string, bool, error) {
	f.inputs = append(f.inputs, in)
	return in.WorkflowID(), f.started, f.err
}

type fakeSender struct{ texts []string }

func (f *fakeSender) SendText(_ context.Context, text string) error {
	f.texts = append(f.texts, text)
	return nil
}

const wlOpenID = "ou_9530871ffdd8ce6997417413c22623d9"

func newExec(st Store, starter *fakeStarter, sender *fakeSender) *Executor {
	return &Executor{
		Store: st, Starter: starter, Sender: sender,
		Whitelist: map[string]bool{wlOpenID: true}, ExpectedVariants: 2,
	}
}

func seedFleet(t *testing.T, s *store.MemStore) {
	t.Helper()
	if err := s.UpsertClientDevices(ctx, store.Client{ClientID: "c1"},
		[]store.Device{{DeviceID: "dev1", Serial: "dev1", ClientID: "c1", SOC: "QCM6125"}}); err != nil {
		t.Fatal(err)
	}
}

func seedArtifacts(t *testing.T, s *store.MemStore, variants ...string) {
	t.Helper()
	arts := []store.Artifact{}
	for _, v := range variants {
		arts = append(arts, store.Artifact{
			Project: "grp/p", CommitSHA: "abcd1234", PipelineID: 42, Variant: v,
			BuildType: "Release", URL: "https://reg/" + v + ".tar.gz",
			SHA256: "sha-" + v, Size: 100, ManifestDigest: "md-" + v,
		})
	}
	if err := s.RegisterArtifacts(ctx, arts); err != nil {
		t.Fatal(err)
	}
}

// 白名单红线:非白名单 open_id 静默忽略(不回复、不执行)。
func TestNonWhitelistSilentlyIgnored(t *testing.T) {
	st := store.NewMemStore()
	starter := &fakeStarter{}
	sender := &fakeSender{}
	exec := newExec(st, starter, sender)
	exec.HandleMessage(ctx, "ou_intruder", "status")
	if len(sender.texts) != 0 {
		t.Errorf("非白名单不得回复: %v", sender.texts)
	}
	if len(starter.inputs) != 0 {
		t.Error("非白名单不得执行任何动作")
	}
}

func TestStatusAndDevicesReply(t *testing.T) {
	st := store.NewMemStore()
	seedFleet(t, st)
	sender := &fakeSender{}
	exec := newExec(st, &fakeStarter{}, sender)

	exec.HandleMessage(ctx, wlOpenID, "status")
	if len(sender.texts) != 1 {
		t.Fatalf("texts = %v", sender.texts)
	}
	if !strings.Contains(sender.texts[0], "运行中 workflow: 0") ||
		!strings.Contains(sender.texts[0], "dev1") ||
		!strings.Contains(sender.texts[0], "IDLE") {
		t.Errorf("status 回复 = %q", sender.texts[0])
	}

	sender.texts = nil
	exec.HandleMessage(ctx, wlOpenID, "devices")
	if !strings.Contains(sender.texts[0], "dev1") || !strings.Contains(sender.texts[0], "soc=QCM6125") ||
		!strings.Contains(sender.texts[0], "fail_streak=0") {
		t.Errorf("devices 回复 = %q", sender.texts[0])
	}
}

func TestUnknownCommandRepliesUsage(t *testing.T) {
	sender := &fakeSender{}
	exec := newExec(store.NewMemStore(), &fakeStarter{}, sender)
	exec.HandleMessage(ctx, wlOpenID, "随便说点什么")
	if len(sender.texts) != 1 || !strings.Contains(sender.texts[0], "可用指令") {
		t.Errorf("未知指令应回 usage: %v", sender.texts)
	}
}

// rerun 表驱动:无记录 / 包不齐 / 变体无记录 / 全量启动 / 单变体启动。
func TestRerun(t *testing.T) {
	cases := []struct {
		name        string
		seed        []string // 预登记变体
		cmd         string
		wantStarted bool
		wantScope   string
		wantAttempt int
		wantPkgs    int
		wantReply   string // 回复必须包含的片段
	}{
		{"查无记录", nil, "rerun abcd1234 42", false, "", 0, 0, "查无记录"},
		{"包不齐", []string{"v1"}, "rerun abcd1234 42", false, "", 0, 0, "包不齐"},
		{"变体无记录", []string{"v1", "v2"}, "rerun abcd1234 42 v3", false, "", 0, 0, "无记录"},
		{"全量启动", []string{"v1", "v2"}, "rerun abcd1234 42", true, "", 1, 2, "已启动"},
		{"单变体启动", []string{"v1", "v2"}, "rerun abcd1234 42 v1", true, "v1", 1, 1, "已启动"},
		{"非法sha", []string{"v1", "v2"}, "rerun zz 42", false, "", 0, 0, "非法 sha"},
		{"非法iid", []string{"v1", "v2"}, "rerun abcd1234 x", false, "", 0, 0, "非法 pipeline_iid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := store.NewMemStore()
			seedArtifacts(t, st, tc.seed...)
			starter := &fakeStarter{started: true}
			sender := &fakeSender{}
			exec := newExec(st, starter, sender)
			exec.HandleMessage(ctx, wlOpenID, tc.cmd)

			if len(sender.texts) != 1 || !strings.Contains(sender.texts[0], tc.wantReply) {
				t.Fatalf("reply = %v, want 含 %q", sender.texts, tc.wantReply)
			}
			if !tc.wantStarted {
				if len(starter.inputs) != 0 {
					t.Errorf("不应启动 workflow: %+v", starter.inputs)
				}
				return
			}
			if len(starter.inputs) != 1 {
				t.Fatalf("inputs = %+v", starter.inputs)
			}
			in := starter.inputs[0]
			if in.Scope != tc.wantScope || in.Attempt != tc.wantAttempt ||
				len(in.Packages) != tc.wantPkgs || in.Project != "grp/p" {
				t.Errorf("input = %+v", in)
			}
			if tc.wantAttempt > 0 && !strings.HasSuffix(in.WorkflowID(), "-r1") {
				t.Errorf("workflow id = %q, want -r1 后缀", in.WorkflowID())
			}
		})
	}
}

func TestUnquarantine(t *testing.T) {
	// 单台:不带 id 自动选定
	st := store.NewMemStore()
	seedFleet(t, st)
	sender := &fakeSender{}
	exec := newExec(st, &fakeStarter{}, sender)
	exec.HandleMessage(ctx, wlOpenID, "unquarantine")
	if !strings.Contains(sender.texts[0], "已解隔离: dev1") {
		t.Errorf("reply = %q", sender.texts[0])
	}
	// 多台:列出要求指定
	st2 := store.NewMemStore()
	if err := st2.UpsertClientDevices(ctx, store.Client{ClientID: "c1"}, []store.Device{
		{DeviceID: "dev1", Serial: "dev1", ClientID: "c1"},
		{DeviceID: "dev2", Serial: "dev2", ClientID: "c1"},
	}); err != nil {
		t.Fatal(err)
	}
	sender2 := &fakeSender{}
	exec2 := newExec(st2, &fakeStarter{}, sender2)
	exec2.HandleMessage(ctx, wlOpenID, "unquarantine")
	if !strings.Contains(sender2.texts[0], "多台设备") || !strings.Contains(sender2.texts[0], "dev2") {
		t.Errorf("reply = %q", sender2.texts[0])
	}
	// 指定 id / 未知 id
	sender2.texts = nil
	exec2.HandleMessage(ctx, wlOpenID, "unquarantine dev2")
	if !strings.Contains(sender2.texts[0], "已解隔离: dev2") {
		t.Errorf("reply = %q", sender2.texts[0])
	}
	sender2.texts = nil
	exec2.HandleMessage(ctx, wlOpenID, "unquarantine ghost")
	if !strings.Contains(sender2.texts[0], "无此设备") {
		t.Errorf("reply = %q", sender2.texts[0])
	}
}

// seedQuarantinedDevice 登记一台设备并驱动其连续 3 次 INFRA 失败进入 QUARANTINED
// (§10 缺省阈值)。设备登记复用 TestUnquarantine 里的同一套 UpsertClientDevices
// 调用;没有直接的"设为 QUARANTINED"store 方法,驱动到位是 store 包自身测试
// (conformance_test.go QuarantineAfterConsecutiveInfraFailures)已在用的既有路径。
func seedQuarantinedDevice(t *testing.T, s *store.MemStore, deviceID string) {
	t.Helper()
	if err := s.UpsertClientDevices(ctx, store.Client{ClientID: "c1"},
		[]store.Device{{DeviceID: deviceID, Serial: deviceID, ClientID: "c1"}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		l, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "seed-task", 120)
		if err != nil || l == nil {
			t.Fatalf("seedQuarantinedDevice: AcquireDevice #%d: lease=%+v err=%v", i+1, l, err)
		}
		if err := s.ReleaseDevice(ctx, l.DeviceID, "seed-task", wf.FailScopeDevice, 3); err != nil {
			t.Fatal(err)
		}
	}
}

// lastText 返回最后一条回复;无回复时返回空串。
func lastText(s *fakeSender) string {
	if len(s.texts) == 0 {
		return ""
	}
	return s.texts[len(s.texts)-1]
}

// countOutcome 统计审计行里某 outcome 的条数——只看回复文本会漏掉审计层的
// bug(比如 outcome 常量被写反,回复文案照样正确),Finding 2 就是这么漏过去的。
func countOutcome(rows []store.CommandTranslation, outcome string) int {
	n := 0
	for _, r := range rows {
		if r.Outcome == outcome {
			n++
		}
	}
	return n
}

func TestHandleMessageTranslatesUnknownInput(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "devices", Confidence: 0.95,
	}}
	st := store.NewMemStore()
	sender := &fakeSender{}
	e := &Executor{Store: st, Sender: sender, Whitelist: map[string]bool{"ou_1": true},
		Translator: newTranslator(f, st)}
	e.HandleMessage(context.Background(), "ou_1", "看下设备都什么状态")
	if f.calls != 1 {
		t.Fatalf("translator calls = %d, want 1", f.calls)
	}
	if !strings.Contains(lastText(sender), "已理解为: devices") {
		t.Errorf("回复应告知理解结果,便于用户下次直接打: %q", lastText(sender))
	}
}

func TestHandleMessageKnownCommandSkipsTranslator(t *testing.T) {
	f := &fakeTranslator{}
	st := store.NewMemStore()
	e := &Executor{Store: st, Sender: &fakeSender{}, Whitelist: map[string]bool{"ou_1": true},
		Translator: newTranslator(f, st)}
	e.HandleMessage(context.Background(), "ou_1", "devices")
	if f.calls != 0 {
		t.Errorf("已能解析的指令不得走翻译层, calls = %d", f.calls)
	}
}

func TestHandleMessageNonWhitelistNeverCallsTranslator(t *testing.T) {
	f := &fakeTranslator{}
	st := store.NewMemStore()
	e := &Executor{Store: st, Sender: &fakeSender{}, Whitelist: map[string]bool{"ou_1": true},
		Translator: newTranslator(f, st)}
	e.HandleMessage(context.Background(), "ou_evil", "帮我重跑一下")
	if f.calls != 0 {
		t.Errorf("非白名单必须零 LLM 调用, calls = %d", f.calls)
	}
}

func TestConfirmFlowExecutesOnYes(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "unquarantine", Args: []string{"dev-1"}, Confidence: 0.95,
	}}
	st := store.NewMemStore()
	seedQuarantinedDevice(t, st, "dev-1") // 见既有测试里的设备准备辅助函数
	sender := &fakeSender{}
	e := &Executor{Store: st, Sender: sender, Whitelist: map[string]bool{"ou_1": true},
		Translator: newTranslator(f, st)}
	e.HandleMessage(context.Background(), "ou_1", "把那台板子放出来")
	if !strings.Contains(lastText(sender), "将执行") {
		t.Fatalf("应先回执待确认: %q", lastText(sender))
	}
	e.HandleMessage(context.Background(), "ou_1", "y")
	if !strings.Contains(lastText(sender), "已解隔离") {
		t.Errorf("确认后应执行: %q", lastText(sender))
	}

	rows, err := st.ListCommandTranslations(context.Background(), "ou_1", 10)
	if err != nil {
		t.Fatalf("ListCommandTranslations: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2 (pending_confirm + confirmed)", rows)
	}
	if n := countOutcome(rows, store.OutcomePendingConfirm); n != 1 {
		t.Errorf("pending_confirm 行数 = %d, want 1", n)
	}
	if n := countOutcome(rows, store.OutcomeConfirmed); n != 1 {
		t.Errorf("confirmed 行数 = %d, want 1", n)
	}
}

func TestConfirmFlowCancelsOnNoWithoutTranslating(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "unquarantine", Args: []string{"dev-1"}, Confidence: 0.95,
	}}
	st := store.NewMemStore()
	seedQuarantinedDevice(t, st, "dev-1")
	sender := &fakeSender{}
	e := &Executor{Store: st, Sender: sender, Whitelist: map[string]bool{"ou_1": true},
		Translator: newTranslator(f, st)}
	e.HandleMessage(context.Background(), "ou_1", "把那台板子放出来")
	before := f.calls
	e.HandleMessage(context.Background(), "ou_1", "n")
	if f.calls != before {
		t.Errorf("n 必须短路,不得再触发翻译: calls %d → %d", before, f.calls)
	}
	if !strings.Contains(lastText(sender), "已取消") {
		t.Errorf("reply = %q", lastText(sender))
	}

	rows, err := st.ListCommandTranslations(context.Background(), "ou_1", 10)
	if err != nil {
		t.Fatalf("ListCommandTranslations: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2 (pending_confirm + declined)", rows)
	}
	if n := countOutcome(rows, store.OutcomePendingConfirm); n != 1 {
		t.Errorf("pending_confirm 行数 = %d, want 1", n)
	}
	if n := countOutcome(rows, store.OutcomeDeclined); n != 1 {
		t.Errorf("declined 行数 = %d, want 1", n)
	}
}

func TestConfirmFlowFallsThroughOnOtherInput(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "unquarantine", Args: []string{"dev-1"}, Confidence: 0.95,
	}}
	st := store.NewMemStore()
	seedQuarantinedDevice(t, st, "dev-1")
	sender := &fakeSender{}
	e := &Executor{Store: st, Sender: sender, Whitelist: map[string]bool{"ou_1": true},
		Translator: newTranslator(f, st)}
	e.HandleMessage(context.Background(), "ou_1", "把那台板子放出来")
	e.HandleMessage(context.Background(), "ou_1", "devices")
	if !strings.Contains(lastText(sender), "已取消上一条待确认") {
		t.Errorf("改口时应提示待确认已取消: %q", lastText(sender))
	}
	if !strings.Contains(lastText(sender), "dev-1") && !strings.Contains(lastText(sender), "serial") {
		t.Errorf("devices 应被执行: %q", lastText(sender))
	}

	rows, err := st.ListCommandTranslations(context.Background(), "ou_1", 10)
	if err != nil {
		t.Fatalf("ListCommandTranslations: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2 (pending_confirm + declined)", rows)
	}
	if n := countOutcome(rows, store.OutcomePendingConfirm); n != 1 {
		t.Errorf("pending_confirm 行数 = %d, want 1", n)
	}
	if n := countOutcome(rows, store.OutcomeDeclined); n != 1 {
		t.Errorf("declined 行数 = %d, want 1", n)
	}
}

func TestConfirmExpires(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "unquarantine", Args: []string{"dev-1"}, Confidence: 0.95,
	}}
	st := store.NewMemStore()
	seedQuarantinedDevice(t, st, "dev-1")
	sender := &fakeSender{}
	now := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	e := &Executor{Store: st, Sender: sender, Whitelist: map[string]bool{"ou_1": true},
		Translator: newTranslator(f, st), Now: func() time.Time { return now }}
	e.HandleMessage(context.Background(), "ou_1", "把那台板子放出来")
	now = now.Add(121 * time.Second)
	e.HandleMessage(context.Background(), "ou_1", "y")
	if strings.Contains(lastText(sender), "已解隔离") {
		t.Error("TTL 过期后 y 不得执行")
	}

	rows, err := st.ListCommandTranslations(context.Background(), "ou_1", 10)
	if err != nil {
		t.Fatalf("ListCommandTranslations: %v", err)
	}
	if n := countOutcome(rows, store.OutcomeExpired); n != 1 {
		t.Errorf("TTL 到期应落一行 expired(设计文档 §4.3 的两个独立触发之一,"+
			"另一个是 TestNewTranslationSupersedesPending 覆盖的被覆盖场景), got %d, rows=%+v", n, rows)
	}
	if n := countOutcome(rows, store.OutcomeConfirmed); n != 0 {
		t.Errorf("TTL 过期后不得落 confirmed, got %d", n)
	}
}

func TestNewTranslationSupersedesPending(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "unquarantine", Args: []string{"dev-1"}, Confidence: 0.95,
	}}
	st := store.NewMemStore()
	seedQuarantinedDevice(t, st, "dev-1")
	e := &Executor{Store: st, Sender: &fakeSender{}, Whitelist: map[string]bool{"ou_1": true},
		Translator: newTranslator(f, st)}
	e.HandleMessage(context.Background(), "ou_1", "把那台板子放出来")
	e.HandleMessage(context.Background(), "ou_1", "再放一次")
	rows, err := st.ListCommandTranslations(context.Background(), "ou_1", 10)
	if err != nil {
		t.Fatalf("ListCommandTranslations: %v", err)
	}
	var expired int
	for _, r := range rows {
		if r.Outcome == store.OutcomeExpired {
			expired++
		}
	}
	if expired != 1 {
		t.Errorf("被覆盖的待确认应落一行 expired, got %d", expired)
	}
}

// TestBlankMessageSkipsTranslatorEvenWhenEnabled 挡住"贴图/图片等非文本消息
// 触发一次 LLM 调用"的回归:listener.extractMessage 对非文本消息返回空文本,
// Parse("") 命中 help,若不特判空串,help 分支的翻译旁路会把每一条贴图都送去
// hermes -z(13s 热/76s 冷),还写一行 raw_text 为空串的垃圾审计并取消待确认。
// 空输入必须原样落到改动前就有的 usage 回复,且翻译层零调用。
func TestBlankMessageSkipsTranslatorEvenWhenEnabled(t *testing.T) {
	f := &fakeTranslator{out: &hermesclient.Translation{
		TranslationVersion: 1, Command: "devices", Confidence: 0.95,
	}}
	st := store.NewMemStore()
	sender := &fakeSender{}
	e := &Executor{Store: st, Sender: sender, Whitelist: map[string]bool{"ou_1": true},
		Translator: newTranslator(f, st)}
	e.HandleMessage(context.Background(), "ou_1", "")
	if f.calls != 0 {
		t.Errorf("空文本(贴图/图片等非文本消息)不得触发翻译, calls = %d", f.calls)
	}
	if lastText(sender) != usage {
		t.Errorf("空文本应回今天的 usage,与翻译层禁用时逐字节一致:\n got %q\nwant %q", lastText(sender), usage)
	}
	rows, err := st.ListCommandTranslations(context.Background(), "ou_1", 10)
	if err != nil {
		t.Fatalf("ListCommandTranslations: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("空文本不应留下任何 raw_text='' 的翻译审计行, rows=%+v", rows)
	}
}

func TestTranslatorDisabledFallsBackToUsage(t *testing.T) {
	st := store.NewMemStore()
	sender := &fakeSender{}
	e := &Executor{Store: st, Sender: sender, Whitelist: map[string]bool{"ou_1": true}}
	e.HandleMessage(context.Background(), "ou_1", "随便说点什么")
	if lastText(sender) != usage {
		t.Errorf("翻译层禁用时必须与改动前逐字节一致:\n got %q\nwant %q", lastText(sender), usage)
	}
}

// 归因拆分后,client 计数必须在飞书输出里可见——否则"这个 client 是不是在
// 持续出问题"无处可查(设计文档决策 2:只计数与展示)。
func TestStatusAndDevicesShowClientFailStreak(t *testing.T) {
	st := store.NewMemStore()
	seedQuarantinedDevice(t, st, "dev-1")
	sender := &fakeSender{}
	e := newExec(st, &fakeStarter{}, sender)
	for _, cmd := range []string{"status", "devices"} {
		sender.texts = nil
		e.HandleMessage(context.Background(), wlOpenID, cmd)
		got := lastText(sender)
		if !strings.Contains(got, "client_fail=") {
			t.Errorf("%s 输出应含 client_fail=, got %q", cmd, got)
		}
	}
}
