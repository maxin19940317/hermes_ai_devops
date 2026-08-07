package feishucmd

import (
	"context"
	"errors"
	"fmt"
	"reflect"
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
		// plan 命令:取第一条空白后的全部文本为单参数
		{"plan", "plan", []string{""}},
		{"plan 测一下 SNPE 2.21", "plan", []string{"测一下 SNPE 2.21"}},
		{"plan     ", "plan", []string{""}},
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
	inputs    []wf.DeviceTestInput
	started   bool
	startErr  error
	closed    bool
	closedErr error
	// closedByID 非 nil 时按 workflow ID 返回关闭状态(缺省 false=未关闭),
	// 用于区分源 workflow 已关闭与上一次重试仍在运行两类场景。
	closedByID map[string]bool
	// startResults 非 nil 时按调用序号返回 started 值(消费式),
	// 用于模拟"已终态残留 ID → 自动推进 → 最终启动成功"。
	startResults []bool
	result       *wf.DeviceTestOutput
	resultErr    error
	terminateErr error
	calls        []string
	trace        *[]string
}

func (f *fakeStarter) StartDeviceTest(_ context.Context, in wf.DeviceTestInput) (string, bool, error) {
	f.calls = append(f.calls, "StartDeviceTest")
	if f.trace != nil {
		*f.trace = append(*f.trace, "StartDeviceTest")
	}
	f.inputs = append(f.inputs, in)
	if f.startResults != nil {
		started := f.startResults[0]
		f.startResults = f.startResults[1:]
		return in.WorkflowID(), started, f.startErr
	}
	return in.WorkflowID(), f.started, f.startErr
}

func (f *fakeStarter) WorkflowClosed(_ context.Context, workflowID string) (bool, error) {
	f.calls = append(f.calls, "WorkflowClosed:"+workflowID)
	if f.trace != nil {
		*f.trace = append(*f.trace, "WorkflowClosed")
	}
	if f.closedByID != nil {
		return f.closedByID[workflowID], f.closedErr
	}
	return f.closed, f.closedErr
}

func (f *fakeStarter) WorkflowResult(_ context.Context, workflowID string) (*wf.DeviceTestOutput, error) {
	f.calls = append(f.calls, "WorkflowResult:"+workflowID)
	if f.trace != nil {
		*f.trace = append(*f.trace, "WorkflowResult")
	}
	return f.result, f.resultErr
}

func (f *fakeStarter) TerminateWorkflow(_ context.Context, workflowID, reason string) error {
	f.calls = append(f.calls, "TerminateWorkflow:"+workflowID)
	if f.trace != nil {
		*f.trace = append(*f.trace, "TerminateWorkflow")
	}
	if f.closedByID != nil {
		// 测试里 cancel 对"已终态"返回 nil(幂等),运行中记录一次
		if closed := f.closedByID[workflowID]; !closed {
			f.closedByID[workflowID] = true
		}
	}
	return f.terminateErr
}

type fakeSender struct{ texts []string }

func (f *fakeSender) SendText(_ context.Context, text string) error {
	f.texts = append(f.texts, text)
	return nil
}

const wlOpenID = "ou_9530871ffdd8ce6997417413c22622d9"

func newExec(st Store, starter *fakeStarter, sender *fakeSender) *Executor {
	return &Executor{
		Store: st, Starter: starter, Sender: sender,
		Whitelist: map[string]bool{wlOpenID: true},
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
		!strings.Contains(sender.texts[0], "🟢") {
		t.Errorf("status 回复 = %q", sender.texts[0])
	}

	sender.texts = nil
	exec.HandleMessage(ctx, wlOpenID, "devices")
	if !strings.Contains(sender.texts[0], "dev1") || !strings.Contains(sender.texts[0], "SoC=QCM6125") ||
		!strings.Contains(sender.texts[0], "失败=0") {
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

type rerunStore struct {
	*store.MemStore
	getErr            error
	listErr           error
	duplicateArtifact bool
	calls             []string
	trace             *[]string
}

func (s *rerunStore) GetWorkflowRun(ctx context.Context, workflowID string) (*store.WorkflowRun, error) {
	s.calls = append(s.calls, "GetWorkflowRun:"+workflowID)
	if s.trace != nil {
		*s.trace = append(*s.trace, "GetWorkflowRun")
	}
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.MemStore.GetWorkflowRun(ctx, workflowID)
}

func (s *rerunStore) ListArtifacts(
	ctx context.Context, project, commitSHA string, pipelineID int,
) ([]store.Artifact, error) {
	s.calls = append(s.calls, fmt.Sprintf("ListArtifacts:%s:%s:%d", project, commitSHA, pipelineID))
	if s.trace != nil {
		*s.trace = append(*s.trace, "ListArtifacts")
	}
	if s.listErr != nil {
		return nil, s.listErr
	}
	arts, err := s.MemStore.ListArtifacts(ctx, project, commitSHA, pipelineID)
	if err == nil && s.duplicateArtifact && len(arts) > 0 {
		arts = append(arts, arts[0])
	}
	return arts, err
}

func (s *rerunStore) NextWorkflowAttempt(
	ctx context.Context, project, commitSHA string, pipelineID int, variant string,
) (int, error) {
	s.calls = append(s.calls, "NextWorkflowAttempt:"+variant)
	if s.trace != nil {
		*s.trace = append(*s.trace, "NextWorkflowAttempt")
	}
	return s.MemStore.NextWorkflowAttempt(ctx, project, commitSHA, pipelineID, variant)
}

func (s *rerunStore) NextWorkflowAttemptAll(
	ctx context.Context, project, commitSHA string, pipelineID int,
) (int, error) {
	s.calls = append(s.calls, "NextWorkflowAttemptAll")
	if s.trace != nil {
		*s.trace = append(*s.trace, "NextWorkflowAttemptAll")
	}
	return s.MemStore.NextWorkflowAttemptAll(ctx, project, commitSHA, pipelineID)
}

const sourceWorkflowID = "device-test-grp/p-gabcd1234-p42-source"

func authoritativeOutput() *wf.DeviceTestOutput {
	return &wf.DeviceTestOutput{Tasks: []wf.TaskSummary{
		{Variant: "v1", TaskID: "task-v1", Verdict: "PASSED"},
		{Variant: "v3", TaskID: "task-v3", Verdict: "INFRA_ERROR"},
		{Variant: "v2", TaskID: "", Verdict: "TEST_FAILED"},
		{Variant: "v4", TaskID: "", Verdict: wf.VerdictSkipped},
	}}
}

func newRerunFixture(t *testing.T, variants ...string) (*rerunStore, *fakeStarter, *Executor) {
	t.Helper()
	mem := store.NewMemStore()
	run := store.WorkflowRun{
		WorkflowID: sourceWorkflowID, Project: "grp/p", CommitSHA: "abcd1234",
		PipelineID: 42, Version: "1.2.3", RuleVersion: "verdict-rules-v7",
		Scope: "source", Variants: []string{"v4", "v2", "v1", "v3"},
	}
	if err := mem.RecordWorkflowRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if variants == nil {
		variants = []string{"v1", "v2", "v3", "v4"}
	}
	seedArtifacts(t, mem, variants...)
	trace := []string{}
	st := &rerunStore{MemStore: mem, trace: &trace}
	starter := &fakeStarter{
		started: true, closed: true, result: authoritativeOutput(), trace: &trace,
	}
	return st, starter, newExec(st, starter, nil)
}

func runRerun(t *testing.T, e *Executor, args ...string) string {
	t.Helper()
	got, err := e.rerun(ctx, args)
	if err != nil {
		t.Fatalf("rerun(%v): %v", args, err)
	}
	return got
}

func artifactAttempt(t *testing.T, s *store.MemStore, project, variant string) int {
	t.Helper()
	for _, art := range s.Artifacts() {
		if art.Project == project && art.Variant == variant {
			return art.WorkflowAttempt
		}
	}
	return -1
}

func TestRerunExactAuthoritativeContract(t *testing.T) {
	t.Run("LegacyTwoArgsShowsMigration", func(t *testing.T) {
		st, starter, e := newRerunFixture(t)
		got := runRerun(t, e, "ABCD1234", "42")
		want := "旧 rerun 语法已停用，请使用 rerun <source_workflow_id> [variant]"
		if got != want {
			t.Fatalf("reply = %q, want %q", got, want)
		}
		if len(st.calls) != 0 || len(starter.calls) != 0 {
			t.Fatalf("legacy syntax touched dependencies: store=%v starter=%v", st.calls, starter.calls)
		}
	})

	t.Run("LegacyThreeArgsShowsMigration", func(t *testing.T) {
		st, starter, e := newRerunFixture(t)
		got := runRerun(t, e, strings.Repeat("x", 513), "not-an-iid", "v1")
		want := "旧 rerun 语法已停用，请使用 rerun <source_workflow_id> [variant]"
		if got != want {
			t.Fatalf("reply = %q, want %q", got, want)
		}
		if len(st.calls) != 0 || len(starter.calls) != 0 {
			t.Fatalf("legacy syntax touched dependencies: store=%v starter=%v", st.calls, starter.calls)
		}
	})

	t.Run("OtherArgCountsShowCurrentUsage", func(t *testing.T) {
		for _, args := range [][]string{nil, {"a", "b", "c", "d"}} {
			st, starter, e := newRerunFixture(t)
			got := runRerun(t, e, args...)
			want := "用法: rerun <source_workflow_id> [variant]"
			if got != want {
				t.Fatalf("rerun(%v) = %q, want %q", args, got, want)
			}
			if len(st.calls) != 0 || len(starter.calls) != 0 {
				t.Fatalf("bad arg count touched dependencies: store=%v starter=%v",
					st.calls, starter.calls)
			}
		}
	})

	t.Run("ArgumentLengthBoundary", func(t *testing.T) {
		const validationReply = "rerun 参数必须无空白且单项不超过 512 字符"

		t.Run("512CharacterWorkflowIDPassesGate", func(t *testing.T) {
			st, starter, e := newRerunFixture(t)
			got := runRerun(t, e, strings.Repeat("w", 512))
			if !strings.Contains(got, "查无权威") {
				t.Fatalf("reply = %q, 512-character ID should reach store lookup", got)
			}
			if len(st.calls) != 1 || len(starter.calls) != 0 {
				t.Fatalf("calls store=%v starter=%v, want GetWorkflowRun only",
					st.calls, starter.calls)
			}
		})

		t.Run("513CharacterWorkflowIDRejectedBeforeDependencies", func(t *testing.T) {
			st, starter, e := newRerunFixture(t)
			got := runRerun(t, e, strings.Repeat("w", 513))
			if got != validationReply {
				t.Fatalf("reply = %q, want %q", got, validationReply)
			}
			if len(st.calls) != 0 || len(starter.calls) != 0 {
				t.Fatalf("overlong ID touched dependencies: store=%v starter=%v",
					st.calls, starter.calls)
			}
		})

		t.Run("512CharacterVariantPassesGate", func(t *testing.T) {
			st, starter, e := newRerunFixture(t)
			variant := strings.Repeat("v", 512)
			got := runRerun(t, e, sourceWorkflowID, variant)
			if !strings.Contains(got, "不属于源 workflow") {
				t.Fatalf("reply = %q, 512-character variant should reach membership validation", got)
			}
			if len(st.calls) != 2 || len(starter.calls) != 1 {
				t.Fatalf("calls store=%v starter=%v, want Get/Closed/List path",
					st.calls, starter.calls)
			}
		})

		t.Run("513CharacterVariantRejectedBeforeDependencies", func(t *testing.T) {
			st, starter, e := newRerunFixture(t)
			got := runRerun(t, e, sourceWorkflowID, strings.Repeat("v", 513))
			if got != validationReply {
				t.Fatalf("reply = %q, want %q", got, validationReply)
			}
			if len(st.calls) != 0 || len(starter.calls) != 0 {
				t.Fatalf("overlong variant touched dependencies: store=%v starter=%v",
					st.calls, starter.calls)
			}
		})

		t.Run("WhitespaceArgRejectedBeforeDependencies", func(t *testing.T) {
			st, starter, e := newRerunFixture(t)
			got := runRerun(t, e, sourceWorkflowID, "bad variant")
			if got != validationReply {
				t.Fatalf("reply = %q, want %q", got, validationReply)
			}
			if len(st.calls) != 0 || len(starter.calls) != 0 {
				t.Fatalf("whitespace variant touched dependencies: store=%v starter=%v",
					st.calls, starter.calls)
			}
		})
	})

	t.Run("NewOneArgRerunsFailedOutputOnly", func(t *testing.T) {
		st, starter, e := newRerunFixture(t)
		got := runRerun(t, e, sourceWorkflowID)
		if !strings.Contains(got, "已启动") {
			t.Fatalf("reply = %q", got)
		}
		if len(starter.inputs) != 1 {
			t.Fatalf("inputs = %+v", starter.inputs)
		}
		in := starter.inputs[0]
		variants := []string{in.Packages[0].Variant, in.Packages[1].Variant}
		if !reflect.DeepEqual(variants, []string{"v2", "v3"}) {
			t.Fatalf("package variants = %v, want canonical failed output v2/v3", variants)
		}
		wantOrder := []string{
			"GetWorkflowRun:" + sourceWorkflowID,
			"ListArtifacts:grp/p:abcd1234:42",
			"NextWorkflowAttemptAll",
		}
		if !reflect.DeepEqual(st.calls, wantOrder) {
			t.Fatalf("store call order = %v, want %v", st.calls, wantOrder)
		}
		wantStarter := []string{
			"WorkflowClosed:" + sourceWorkflowID,
			"WorkflowResult:" + sourceWorkflowID,
			"StartDeviceTest",
		}
		if !reflect.DeepEqual(starter.calls, wantStarter) {
			t.Fatalf("starter call order = %v, want %v", starter.calls, wantStarter)
		}
		wantTrace := []string{
			"GetWorkflowRun", "WorkflowClosed", "WorkflowResult", "ListArtifacts",
			"NextWorkflowAttemptAll", "StartDeviceTest",
		}
		if !reflect.DeepEqual(*st.trace, wantTrace) {
			t.Fatalf("global call order = %v, want %v", *st.trace, wantTrace)
		}
	})

	t.Run("VariantScopedAllThenExplicitNeverReusesWorkflowID", func(t *testing.T) {
		mem := store.NewMemStore()
		if err := mem.RecordWorkflowRun(ctx, store.WorkflowRun{
			WorkflowID: sourceWorkflowID, Project: "grp/p", CommitSHA: "abcd1234",
			PipelineID: 42, Version: "1.2.3", RuleVersion: "verdict-rules-v7",
			Scope: "v1", Variants: []string{"v1", "v2"},
		}); err != nil {
			t.Fatal(err)
		}
		seedArtifacts(t, mem, "v1", "v2")
		for want := 1; want <= 3; want++ {
			if n, err := mem.NextWorkflowAttempt(ctx, "grp/p", "abcd1234", 42, "v2"); err != nil || n != want {
				t.Fatalf("skew v2 = %d err=%v, want %d", n, err, want)
			}
		}
		st := &rerunStore{MemStore: mem}
		starter := &fakeStarter{
			started: true, closed: true,
			result: &wf.DeviceTestOutput{Tasks: []wf.TaskSummary{
				{Variant: "v1", Verdict: "TEST_FAILED"},
			}},
		}
		e := newExec(st, starter, nil)

		runRerun(t, e, sourceWorkflowID)
		for range 3 {
			runRerun(t, e, sourceWorkflowID, "v1")
		}
		seen := make(map[string]struct{}, len(starter.inputs))
		for _, in := range starter.inputs {
			id := in.WorkflowID()
			if _, exists := seen[id]; exists {
				t.Fatalf("workflow ID reused after mixed reruns: %s; inputs=%+v", id, starter.inputs)
			}
			seen[id] = struct{}{}
		}
		if got := starter.inputs[1].Attempt; got != starter.inputs[0].Attempt+1 {
			t.Fatalf("first explicit attempt = %d, want all waterline %d + 1",
				got, starter.inputs[0].Attempt)
		}
	})

	t.Run("NewTwoArgsRerunsExplicitPassedVariant", func(t *testing.T) {
		_, starter, e := newRerunFixture(t)
		starter.resultErr = errors.New("must not read result")
		got := runRerun(t, e, sourceWorkflowID, "v1")
		if !strings.Contains(got, "已启动") || len(starter.inputs) != 1 {
			t.Fatalf("reply=%q inputs=%+v", got, starter.inputs)
		}
		if starter.inputs[0].Scope != "v1" || len(starter.inputs[0].Packages) != 1 ||
			starter.inputs[0].Packages[0].Variant != "v1" {
			t.Fatalf("input = %+v", starter.inputs[0])
		}
		if strings.Join(starter.calls, ",") !=
			"WorkflowClosed:"+sourceWorkflowID+",StartDeviceTest" {
			t.Fatalf("starter calls = %v, explicit variant must not read result", starter.calls)
		}
	})

	t.Run("ExplicitSkippedVariantAllowed", func(t *testing.T) {
		_, starter, e := newRerunFixture(t)
		got := runRerun(t, e, sourceWorkflowID, "v4")
		if !strings.Contains(got, "已启动") || starter.inputs[0].Packages[0].Variant != "v4" {
			t.Fatalf("reply=%q input=%+v", got, starter.inputs)
		}
	})

	t.Run("UnknownOrLegacyRunRejected", func(t *testing.T) {
		st, starter, e := newRerunFixture(t)
		st.getErr = store.ErrWorkflowRunNotFound
		got := runRerun(t, e, "unknown-workflow")
		if !strings.Contains(got, "权威") || len(starter.calls) != 0 {
			t.Fatalf("reply=%q starter calls=%v", got, starter.calls)
		}
	})

	t.Run("RunningOrDescribeErrorRejected", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			err  error
		}{
			{name: "running"},
			{name: "describe error", err: errors.New("describe unavailable")},
		} {
			t.Run(tc.name, func(t *testing.T) {
				st, starter, e := newRerunFixture(t)
				starter.closed = false
				starter.closedErr = tc.err
				got := runRerun(t, e, sourceWorkflowID)
				if tc.err == nil && !strings.Contains(got, "尚未结束") {
					t.Fatalf("running reply = %q", got)
				}
				if tc.err != nil && !strings.Contains(got, "检查 workflow 状态失败") {
					t.Fatalf("describe error reply = %q", got)
				}
				if len(st.calls) != 1 || len(starter.inputs) != 0 {
					t.Fatalf("calls store=%v starter=%v", st.calls, starter.calls)
				}
			})
		}
	})

	t.Run("WorkflowResultErrorRejectedWithoutVariant", func(t *testing.T) {
		st, starter, e := newRerunFixture(t)
		starter.resultErr = errors.New("result unavailable")
		got := runRerun(t, e, sourceWorkflowID)
		if !strings.Contains(got, "读取 workflow 结果失败") || len(st.calls) != 1 {
			t.Fatalf("reply=%q store calls=%v", got, st.calls)
		}
	})

	t.Run("NoFailuresDoesNotAllocateAttempt", func(t *testing.T) {
		st, starter, e := newRerunFixture(t)
		starter.result = &wf.DeviceTestOutput{Tasks: []wf.TaskSummary{
			{Variant: "v1", Verdict: "PASSED"},
			{Variant: "v4", Verdict: wf.VerdictSkipped},
		}}
		got := runRerun(t, e, sourceWorkflowID)
		if !strings.Contains(got, "没有失败变体") {
			t.Fatalf("reply = %q", got)
		}
		if artifactAttempt(t, st.MemStore, "grp/p", "v1") != 0 || len(starter.inputs) != 0 {
			t.Fatalf("attempt allocated or workflow started: arts=%+v inputs=%+v",
				st.Artifacts(), starter.inputs)
		}
	})

	t.Run("MissingArtifactDoesNotAllocateAttempt", func(t *testing.T) {
		st, starter, e := newRerunFixture(t, "v1", "v2", "v4")
		got := runRerun(t, e, sourceWorkflowID)
		if !strings.Contains(got, "v3") || !strings.Contains(got, "artifact") {
			t.Fatalf("reply = %q", got)
		}
		if artifactAttempt(t, st.MemStore, "grp/p", "v2") != 0 || len(starter.inputs) != 0 {
			t.Fatalf("attempt allocated or workflow started: arts=%+v inputs=%+v",
				st.Artifacts(), starter.inputs)
		}
	})

	t.Run("DuplicateArtifactDoesNotAllocateAttempt", func(t *testing.T) {
		st, starter, e := newRerunFixture(t)
		st.duplicateArtifact = true
		got := runRerun(t, e, sourceWorkflowID, "v1")
		if !strings.Contains(got, "artifact 数量为 2") {
			t.Fatalf("reply = %q", got)
		}
		if artifactAttempt(t, st.MemStore, "grp/p", "v1") != 0 || len(starter.inputs) != 0 {
			t.Fatalf("attempt allocated or workflow started: arts=%+v inputs=%+v",
				st.Artifacts(), starter.inputs)
		}
	})

	t.Run("ProjectVersionRuleAndSourceAreInherited", func(t *testing.T) {
		_, starter, e := newRerunFixture(t)
		runRerun(t, e, sourceWorkflowID)
		in := starter.inputs[0]
		if in.Project != "grp/p" || in.Commit != "abcd1234" || in.PipelineID != 42 ||
			in.Version != "1.2.3" || in.RuleVersion != "verdict-rules-v7" ||
			in.SourceWorkflowID != sourceWorkflowID || in.Scope != "source" {
			t.Fatalf("input identity = %+v", in)
		}
	})

	t.Run("PreCreateTaskFailureWithoutTaskIDIsRetried", func(t *testing.T) {
		_, starter, e := newRerunFixture(t)
		runRerun(t, e, sourceWorkflowID)
		if got := starter.inputs[0].Packages[0].Variant; got != "v2" {
			t.Fatalf("first retry variant = %q, want v2 with empty TaskID", got)
		}
	})

	t.Run("StaleTaskTableDoesNotOverrideWorkflowOutput", func(t *testing.T) {
		st, starter, e := newRerunFixture(t)
		if err := st.CreateTask(ctx, wf.TaskRow{
			TaskID: "stale-v1", WorkflowID: sourceWorkflowID, TestID: "v1",
			Attempt: 1, IdempotencyKey: "stale-v1", Status: "FAILED",
		}); err != nil {
			t.Fatal(err)
		}
		runRerun(t, e, sourceWorkflowID)
		got := []string{starter.inputs[0].Packages[0].Variant, starter.inputs[0].Packages[1].Variant}
		if !reflect.DeepEqual(got, []string{"v2", "v3"}) {
			t.Fatalf("variants = %v, stale task table overrode workflow output", got)
		}
	})

	t.Run("AlreadyStartedIsReportedButNotClaimDedup", func(t *testing.T) {
		st, starter, e := newRerunFixture(t)
		starter.started = false
		first := runRerun(t, e, sourceWorkflowID, "v2")
		second := runRerun(t, e, sourceWorkflowID, "v2")
		if !strings.Contains(first, "workflow 已存在") || !strings.Contains(second, "workflow 已存在") {
			t.Fatalf("replies = %q / %q", first, second)
		}
		if len(starter.inputs) != 2 ||
			starter.inputs[0].Attempt != 1 || starter.inputs[1].Attempt != 2 {
			t.Fatalf("inputs = %+v, each text command must allocate a fresh attempt", starter.inputs)
		}
		if n := artifactAttempt(t, st.MemStore, "grp/p", "v2"); n != 2 {
			t.Fatalf("v2 attempt = %d, want 2", n)
		}
	})
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
	for _, cmd := range []string{"status", "devices all"} {
		sender.texts = nil
		e.HandleMessage(context.Background(), wlOpenID, cmd)
		got := lastText(sender)
		if !strings.Contains(got, "client=") {
			t.Errorf("%s 输出应含 client=, got %q", cmd, got)
		}
		if !strings.Contains(got, "client_fail=") {
			t.Errorf("%s 输出应含 client_fail=, got %q", cmd, got)
		}
	}
}

// ---- 卡片按钮:ignore ----

// ignore 必须把人工裁决落在该变体最新任务上:decisions.task_id 有 FK 指向
// tasks,直接写 workflow_id 会违反 decisions_task_id_fkey(2026-08-03 实测)。
func TestHandleCardActionIgnoreSavesDecisionOnLatestTask(t *testing.T) {
	ms := store.NewMemStore()
	if err := ms.RecordWorkflowRun(ctx, store.WorkflowRun{
		WorkflowID: "w-1", Project: "grp/p", CommitSHA: "abcd1234", PipelineID: 42,
		Version: "1.0.2", RuleVersion: "v1", Variants: []string{"v1"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []wf.TaskRow{
		{TaskID: "w-1:v1:a1", WorkflowID: "w-1", TestID: "v1", Attempt: 1, IdempotencyKey: "w-1:v1:a1"},
		{TaskID: "w-1:v1:a2", WorkflowID: "w-1", TestID: "v1", Attempt: 2, IdempotencyKey: "w-1:v1:a2"},
	} {
		if err := ms.CreateTask(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	exec := newExec(ms, &fakeStarter{}, &fakeSender{})

	text, toast, err := exec.HandleCardAction(ctx,
		wf.ButtonValue{Action: "ignore", SourceWorkflowID: "w-1", Variant: "v1"}, wlOpenID)
	if err != nil || toast != "success" {
		t.Fatalf("ignore = %q/%q err=%v, want success toast", text, toast, err)
	}
	decs, err := ms.ListDecisions(ctx, "w-1:v1:a2")
	if err != nil || len(decs) != 1 || decs[0].Actor != "human" {
		t.Fatalf("decisions = %+v err=%v, want one human decision on latest task", decs, err)
	}
}

// 变体在该次运行中没有任务记录时,ignore 回 info toast 且不落裁决。
func TestHandleCardActionIgnoreWithoutTask(t *testing.T) {
	ms := store.NewMemStore()
	if err := ms.RecordWorkflowRun(ctx, store.WorkflowRun{
		WorkflowID: "w-1", Project: "grp/p", CommitSHA: "abcd1234", PipelineID: 42,
		Version: "1.0.2", RuleVersion: "v1", Variants: []string{"v1"},
	}); err != nil {
		t.Fatal(err)
	}
	exec := newExec(ms, &fakeStarter{}, &fakeSender{})

	text, toast, err := exec.HandleCardAction(ctx,
		wf.ButtonValue{Action: "ignore", SourceWorkflowID: "w-1", Variant: "v1"}, wlOpenID)
	if err != nil || toast != "info" || !strings.Contains(text, "没有任务记录") {
		t.Fatalf("ignore = %q/%q err=%v, want info toast", text, toast, err)
	}
}

// ---- 重试防连点认领 ----

// seedRetrySource 登记源运行 + 变体 artifact,并预消耗一次 attempt
// (模拟已存在 r1 重试)。
func seedRetrySource(t *testing.T, ms *store.MemStore) {
	t.Helper()
	if err := ms.RecordWorkflowRun(ctx, store.WorkflowRun{
		WorkflowID: "w-1", Project: "grp/p", CommitSHA: "abcd1234", PipelineID: 42,
		Version: "1.0.2", RuleVersion: "v1", Variants: []string{"v1"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ms.RegisterArtifacts(ctx, []store.Artifact{{
		Project: "grp/p", CommitSHA: "abcd1234", PipelineID: 42, Variant: "v1",
		BuildType: "Release", URL: "u", SHA256: "s", Size: 1, ManifestDigest: "m",
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.NextWorkflowAttempt(ctx, "grp/p", "abcd1234", 42, "v1"); err != nil {
		t.Fatal(err)
	}
}

func startedCount(starter *fakeStarter) int {
	n := 0
	for _, c := range starter.calls {
		if c == "StartDeviceTest" {
			n++
		}
	}
	return n
}

// 上一次重试仍在运行时,按钮重试被认领拦截:不分配新 attempt、不起新 workflow。
func TestHandleCardActionRetryBlockedWhileInFlight(t *testing.T) {
	ms := store.NewMemStore()
	seedRetrySource(t, ms)
	starter := &fakeStarter{closedByID: map[string]bool{"w-1": true}} // r1 未关闭
	exec := newExec(ms, starter, &fakeSender{})

	text, _, err := exec.HandleCardAction(ctx,
		wf.ButtonValue{Action: "retry", SourceWorkflowID: "w-1", Variant: "v1"}, wlOpenID)
	if err != nil || !strings.Contains(text, "重试正在进行中") {
		t.Fatalf("retry = %q err=%v, want 认领拦截", text, err)
	}
	if got := startedCount(starter); got != 0 {
		t.Errorf("StartDeviceTest 调用 %d 次, want 0", got)
	}
	if n, _ := ms.CurrentWorkflowAttempt(ctx, "grp/p", "abcd1234", 42, "v1"); n != 1 {
		t.Errorf("attempt = %d, want 1(拦截不得消耗 attempt)", n)
	}
}

// 上一次重试已关闭时,按钮重试正常推进到下一 attempt。
func TestHandleCardActionRetryProceedsAfterClosed(t *testing.T) {
	ms := store.NewMemStore()
	seedRetrySource(t, ms)
	r1ID := wf.DeviceTestInput{
		Project: "grp/p", Commit: "abcd1234", PipelineID: 42, Scope: "v1", Attempt: 1,
	}.WorkflowID()
	starter := &fakeStarter{started: true, closedByID: map[string]bool{"w-1": true, r1ID: true}}
	exec := newExec(ms, starter, &fakeSender{})

	text, toast, err := exec.HandleCardAction(ctx,
		wf.ButtonValue{Action: "retry", SourceWorkflowID: "w-1", Variant: "v1"}, wlOpenID)
	if err != nil || toast != "success" || !strings.Contains(text, "已启动重试") {
		t.Fatalf("retry = %q/%q err=%v, want 已启动", text, toast, err)
	}
	if got := startedCount(starter); got != 1 {
		t.Fatalf("StartDeviceTest 调用 %d 次, want 1", got)
	}
	if starter.inputs[0].Attempt != 2 {
		t.Errorf("新重试 attempt = %d, want 2", starter.inputs[0].Attempt)
	}
}

// 文本 rerun 显式变体与按钮共用同一条认领检查。
func TestRerunExplicitVariantBlockedWhileInFlight(t *testing.T) {
	ms := store.NewMemStore()
	seedRetrySource(t, ms)
	starter := &fakeStarter{closedByID: map[string]bool{"w-1": true}} // r1 未关闭
	exec := newExec(ms, starter, &fakeSender{})

	text, err := exec.rerun(ctx, []string{"w-1", "v1"})
	if err != nil || !strings.Contains(text, "重试正在进行中") {
		t.Fatalf("rerun = %q err=%v, want 认领拦截", text, err)
	}
	if got := startedCount(starter); got != 0 {
		t.Errorf("StartDeviceTest 调用 %d 次, want 0", got)
	}
}

// ---- test 命令 ----

func runTest(t *testing.T, e *Executor, args ...string) string {
	t.Helper()
	got, err := e.testCmd(ctx, args)
	if err != nil {
		t.Fatalf("test(%v): %v", args, err)
	}
	return got
}

// newTestFixture 返回带 variant 表 + 已登记产物(grp/p @ abcd1234 p42)的执行器。
func newTestFixture(t *testing.T, variants ...string) (*store.MemStore, *fakeStarter, *Executor) {
	t.Helper()
	mem := store.NewMemStore()
	if variants == nil {
		variants = []string{"v1", "v2"}
	}
	seedArtifacts(t, mem, variants...)
	starter := &fakeStarter{started: true}
	e := newExec(mem, starter, nil)
	e.Variants = variants
	return mem, starter, e
}

func TestTestCmdUsage(t *testing.T) {
	mem, starter, e := newTestFixture(t)
	for _, args := range [][]string{nil, {"a", "b", "c"}} {
		got := runTest(t, e, args...)
		if got != "用法: test <variant> [commit]" {
			t.Fatalf("test(%v) = %q", args, got)
		}
		if len(starter.inputs) != 0 {
			t.Fatalf("bad arg count started workflow: %+v", starter.inputs)
		}
	}
	_ = mem
}

func TestTestCmdUnknownVariant(t *testing.T) {
	mem, starter, e := newTestFixture(t, "v1", "v2")
	got := runTest(t, e, "ghost")
	if !strings.Contains(got, "未知变体") || !strings.Contains(got, "v1, v2") {
		t.Fatalf("reply = %q", got)
	}
	if len(starter.inputs) != 0 {
		t.Fatal("unknown variant started workflow")
	}
	_ = mem
}

func TestTestCmdBadCommitShape(t *testing.T) {
	_, starter, e := newTestFixture(t)
	got := runTest(t, e, "v1", "NOT-A-SHA!")
	if !strings.Contains(got, "commit 形态不合法") {
		t.Fatalf("reply = %q", got)
	}
	if len(starter.inputs) != 0 {
		t.Fatal("bad commit started workflow")
	}
}

func TestTestCmdNoArtifact(t *testing.T) {
	mem := store.NewMemStore() // 无产物
	starter := &fakeStarter{started: true}
	e := newExec(mem, starter, nil)
	e.Variants = []string{"v1"}
	got := runTest(t, e, "v1")
	if !strings.Contains(got, "暂无构建记录") {
		t.Fatalf("reply = %q", got)
	}
	if len(starter.inputs) != 0 {
		t.Fatal("no artifact started workflow")
	}
}

func TestTestCmdStartsVariantWorkflow(t *testing.T) {
	_, starter, e := newTestFixture(t)
	got := runTest(t, e, "v2")
	if !strings.Contains(got, "已启动") {
		t.Fatalf("reply = %q", got)
	}
	if len(starter.inputs) != 1 {
		t.Fatalf("inputs = %+v", starter.inputs)
	}
	in := starter.inputs[0]
	if in.Scope != "v2" {
		t.Errorf("Scope = %q, want v2", in.Scope)
	}
	if len(in.Packages) != 1 || in.Packages[0].Variant != "v2" {
		t.Errorf("Packages = %+v", in.Packages)
	}
	if in.Attempt < 1 {
		t.Errorf("Attempt = %d, want >= 1", in.Attempt)
	}
	// 缺省 commit:LatestArtifactForVariant → grp/p @ abcd1234 p42
	if in.Project != "grp/p" || in.Commit != "abcd1234" || in.PipelineID != 42 {
		t.Errorf("artifact = %s g%s p%d", in.Project, in.Commit, in.PipelineID)
	}
}

func TestTestCmdSpecifiedCommit(t *testing.T) {
	mem, starter, e := newTestFixture(t)
	// 先登记一条 workflow run,使 RecentRuns 返回 commit abcd1234 的权威记录。
	run := store.WorkflowRun{
		WorkflowID: "device-test-grp/p-gabcd1234-p42", Project: "grp/p",
		CommitSHA: "abcd1234", PipelineID: 42, Version: "1.2.3",
		RuleVersion: "verdict-rules-v7", Scope: "v2", Variants: []string{"v2"},
	}
	if err := mem.RecordWorkflowRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	got := runTest(t, e, "v2", "abcd1234")
	if !strings.Contains(got, "已启动") {
		t.Fatalf("reply = %q", got)
	}
	if len(starter.inputs) != 1 {
		t.Fatalf("inputs = %+v", starter.inputs)
	}
	if in := starter.inputs[0]; in.Commit != "abcd1234" || in.Scope != "v2" {
		t.Errorf("input = %+v", in)
	}
}

func TestTestCmdCommitWithoutVariant(t *testing.T) {
	_, starter, e := newTestFixture(t) // 只有 v1/v2,无其他 commit
	got := runTest(t, e, "v1", "deadbeef")
	if !strings.Contains(got, "无变体 v1 的构建记录") {
		t.Fatalf("reply = %q", got)
	}
	if len(starter.inputs) != 0 {
		t.Fatal("missing commit started workflow")
	}
}

// 已终态残留 ID(如手动启动的历史 workflow)应被自动跳过并推进到下一 attempt。
func TestTestCmdAdvancesPastTerminalLeftover(t *testing.T) {
	_, starter, e := newTestFixture(t)
	// r1 已终态(closed),r2 可启动:第一次 StartDeviceTest 返回未启动,
	// WorkflowClosed(r1)=true → 推进 attempt → 第二次启动成功。
	starter.closedByID = map[string]bool{"device-test-grp/p-gabcd1234-p42-v1-r1": true}
	starter.startResults = []bool{false, true}

	got := runTest(t, e, "v1")
	if !strings.Contains(got, "已启动") {
		t.Fatalf("reply = %q, want 推进后启动成功", got)
	}
	if len(starter.inputs) != 2 {
		t.Fatalf("inputs = %+v, want 两次启动尝试", starter.inputs)
	}
	if a1, a2 := starter.inputs[0].Attempt, starter.inputs[1].Attempt; a2 != a1+1 {
		t.Errorf("attempt %d → %d, want 单调递增", a1, a2)
	}
}

// 最新 attempt 的 workflow 仍在运行:拒绝重复派发。
func TestTestCmdRejectsInFlight(t *testing.T) {
	mem, starter, e := newTestFixture(t)
	// 先推进一次,使 attempt 水位=1(模拟已分配过一版)。
	if _, err := mem.NextWorkflowAttempt(ctx, "grp/p", "abcd1234", 42, "v1"); err != nil {
		t.Fatal(err)
	}
	// attempt 1 未关闭 → testInFlight 命中 → 拒绝。
	starter.closedByID = map[string]bool{"device-test-grp/p-gabcd1234-p42-v1-r1": false}

	got := runTest(t, e, "v1")
	if !strings.Contains(got, "测试正在进行中") {
		t.Fatalf("reply = %q, want 进行中拒绝", got)
	}
	if len(starter.inputs) != 0 {
		t.Fatalf("in-flight 拒绝后不应启动: %+v", starter.inputs)
	}
}

// 已存在 ID 且为运行中(并发竞态):提示进行中,不重复派发。
func TestTestCmdRaceAlreadyRunning(t *testing.T) {
	mem, starter, e := newTestFixture(t)
	if _, err := mem.NextWorkflowAttempt(ctx, "grp/p", "abcd1234", 42, "v1"); err != nil {
		t.Fatal(err)
	}
	// 第一次 StartDeviceTest 撞上已存在的运行中 workflow(attempt 2 被并发占用)。
	starter.closedByID = map[string]bool{
		"device-test-grp/p-gabcd1234-p42-v1-r1": true, // 水位 1 已终态 → 放行
		"device-test-grp/p-gabcd1234-p42-v1-r2": false, // 竞态占用的 r2 运行中
	}
	starter.startResults = []bool{false}

	got := runTest(t, e, "v1")
	if !strings.Contains(got, "测试正在进行中") {
		t.Fatalf("reply = %q, want 进行中提示", got)
	}
	if len(starter.inputs) != 1 {
		t.Fatalf("inputs = %+v, want 仅一次尝试", starter.inputs)
	}
}

// ---- 新指令(2026-08-07):runs/result/metrics/artifacts/quarantine/cancel ----

func TestRunsCommand(t *testing.T) {
	mem := store.NewMemStore()
	run := store.WorkflowRun{
		WorkflowID: "device-test-grp/p-gabcd1234-p42", Project: "grp/p",
		CommitSHA: "abcd1234", PipelineID: 42, Version: "1.2.3",
		RuleVersion: "verdict-rules-v7", Scope: "v1", Variants: []string{"v1"},
	}
	if err := mem.RecordWorkflowRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := mem.CreateTask(ctx, wf.TaskRow{
		TaskID: "t1", WorkflowID: run.WorkflowID, TestID: "v1", Attempt: 1,
		Status: "COMPLETED",
	}); err != nil {
		t.Fatal(err)
	}
	if err := mem.FinishTask(ctx, wf.FinishRequest{
		TaskID: "t1", Status: "COMPLETED", Verdict: "PASSED",
	}); err != nil {
		t.Fatal(err)
	}
	e := newExec(mem, &fakeStarter{}, &fakeSender{})

	got, err := e.runs(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "v1") || !strings.Contains(got, "PASSED") || !strings.Contains(got, "gabcd1234") {
		t.Errorf("runs = %q", got)
	}
	// 非法参数
	if got, _ := e.runs(ctx, []string{"abc"}); !strings.Contains(got, "用法") {
		t.Errorf("runs abc = %q", got)
	}
}

func TestRunsCommandBadLimit(t *testing.T) {
	e := newExec(store.NewMemStore(), &fakeStarter{}, &fakeSender{})
	for _, arg := range []string{"0", "-1", "21", "abc"} {
		got, _ := e.runs(ctx, []string{arg})
		if !strings.Contains(got, "用法") {
			t.Errorf("runs %s = %q, want usage", arg, got)
		}
	}
}

// bundle workflow 声明变体但从未 kick(task 不存在)→ 待调度而非运行中。
func TestRunsDistinguishesPendingFromRunning(t *testing.T) {
	mem := store.NewMemStore()
	pendingRun := store.WorkflowRun{
		WorkflowID: "device-test-grp/p-gabcdef01-p99", Project: "grp/p",
		CommitSHA: "abcdef01", PipelineID: 99, Version: "1.2.3",
		RuleVersion: "verdict-rules-v7", Scope: "", Variants: []string{"v1", "v2"},
	}
	if err := mem.RecordWorkflowRun(ctx, pendingRun); err != nil {
		t.Fatal(err)
	}
	// v1 有 task(运行中,无终态),v2 无 task(待调度)
	if err := mem.CreateTask(ctx, wf.TaskRow{
		TaskID: "t-running", WorkflowID: pendingRun.WorkflowID, TestID: "v1", Attempt: 1,
		Status: "RUNNING",
	}); err != nil {
		t.Fatal(err)
	}
	e := newExec(mem, &fakeStarter{}, &fakeSender{})
	got, err := e.runs(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "v1 运行中") {
		t.Errorf("v1 应有 task 显示运行中, got %q", got)
	}
	if !strings.Contains(got, "v2 待调度") {
		t.Errorf("v2 无 task 应显示待调度, got %q", got)
	}
}
