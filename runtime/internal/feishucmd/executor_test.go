package feishucmd

import (
	"context"
	"strings"
	"testing"

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
