package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"

	"hermes-devops/runtime/internal/hermesclient"
	"hermes-devops/runtime/internal/rules"
)

var errBoom = errors.New("client unreachable")

// fakeActs 以真实方法签名注册,记录全部调用(线程安全:活动并发执行)。
type fakeActs struct {
	mu           sync.Mutex
	specs        []TestSpec
	skipped      []SkippedSpec // SelectTestSpecs 透传:fleet 无匹配设备/OS 未接入
	acquires     []*Lease      // 每次 AcquireDevice 依次弹出;耗尽后返回 defaultLease
	dispatchErrs []error       // 依次弹出;耗尽后 nil

	acquireCalls  int
	created       []TaskRow
	dispatched    []DispatchRequest
	canceled      []CancelRequest
	finished      []FinishRequest
	released      []ReleaseRequest
	notifications []string

	analysis      *hermesclient.Analysis // 非 nil 模拟 Analyzer 已启用
	evidenceCalls []ExtractEvidenceRequest
	analyzeCalls  []AnalyzeRequest
	decisions     []DecisionRow
	matchedSigs   []string // ExtractEvidence 返回的 runtime 提取签名命中
	evidenceErr   error    // 非 nil 模拟提取失败(降级路径)
	snapshotID    string   // ExtractEvidence 返回的快照 id(空 = 降级未持久化)

	results   map[string]ResultRecord // LoadResult 的权威数据源(模拟 results 表)
	loadCalls []string

	leaseExpiry     *time.Time // CheckLease 的返回(模拟 device_leases.lease_expires_at);nil = 未续期
	checkLeaseCalls []string
}

var defaultLease = &Lease{DeviceID: "dev1", Serial: "513cd3de", ClientID: "c1", ClientBaseURL: "https://client:8443"}

func (f *fakeActs) SelectTestSpecs(_ context.Context, _ DeviceTestInput) (*SpecSelection, error) {
	return &SpecSelection{Specs: f.specs, Skipped: f.skipped}, nil
}
func (f *fakeActs) AcquireDevice(_ context.Context, _ AcquireRequest) (*Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquireCalls++
	if len(f.acquires) > 0 {
		l := f.acquires[0]
		f.acquires = f.acquires[1:]
		return l, nil
	}
	return defaultLease, nil
}
func (f *fakeActs) CreateTask(_ context.Context, t TaskRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, t)
	return nil
}
func (f *fakeActs) Dispatch(_ context.Context, d DispatchRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dispatched = append(f.dispatched, d)
	if len(f.dispatchErrs) > 0 {
		err := f.dispatchErrs[0]
		f.dispatchErrs = f.dispatchErrs[1:]
		return err
	}
	return nil
}
func (f *fakeActs) CancelTask(_ context.Context, c CancelRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canceled = append(f.canceled, c)
	return nil
}
func (f *fakeActs) FinishTask(_ context.Context, fr FinishRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finished = append(f.finished, fr)
	return nil
}
func (f *fakeActs) ReleaseDevice(_ context.Context, r ReleaseRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, r)
	return nil
}
func (f *fakeActs) Notify(_ context.Context, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notifications = append(f.notifications, msg)
	return nil
}
func (f *fakeActs) ExtractEvidence(_ context.Context, r ExtractEvidenceRequest) (*ExtractEvidenceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evidenceCalls = append(f.evidenceCalls, r)
	if f.evidenceErr != nil {
		return nil, f.evidenceErr
	}
	return &ExtractEvidenceResponse{
		EvidenceJSON:      json.RawMessage(`{"evidence_version":1}`),
		Digest:            "deadbeef",
		MatchedSignatures: f.matchedSigs,
		SnapshotID:        f.snapshotID,
	}, nil
}
func (f *fakeActs) Analyze(_ context.Context, r AnalyzeRequest) (*hermesclient.Analysis, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.analyzeCalls = append(f.analyzeCalls, r)
	return f.analysis, nil // nil = Analyzer 未启用(§12 降级)
}
func (f *fakeActs) SaveDecision(_ context.Context, d DecisionRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decisions = append(f.decisions, d)
	return nil
}
func (f *fakeActs) LoadResult(_ context.Context, r LoadResultRequest) (*ResultRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadCalls = append(f.loadCalls, r.TaskID)
	rec, ok := f.results[r.TaskID]
	if !ok {
		return nil, nil
	}
	out := rec
	return &out, nil
}
func (f *fakeActs) CheckLease(_ context.Context, r CheckLeaseRequest) (*time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkLeaseCalls = append(f.checkLeaseCalls, r.TaskID)
	return f.leaseExpiry, nil
}

// seedResult 把 sig 登记为 results 表的权威行:事务性 Outbox 链路下
// workflow 只消费 signal 里的 task_id,结果本体由 LoadResult 回读(差距 #2)。
func seedResult(f *fakeActs, sig TaskResultSignal) {
	if f.results == nil {
		f.results = map[string]ResultRecord{}
	}
	f.results[sig.TaskID] = ResultRecord{TaskID: sig.TaskID, Result: sig}
}

const wfID = "device-test-grp/p-gabcd1234-p42"

func spec1() TestSpec {
	return TestSpec{
		TestID:            "t1",
		Variant:           "aarch64_Android_SNPE_2.21",
		Package:           PackageRef{URL: "https://reg/pkg.tar.gz", SHA256: "aa", ManifestDigest: "bb"},
		Selector:          DeviceSelector{SOC: []string{"QCM6125"}},
		SignatureCategory: map[string]rules.Category{"cpu_fallback": "MODEL", "dsp_unavailable": "DELEGATE"},
		MaxInfraRetries:   2,
		LeaseSeconds:      120,
		HardTimeoutSec:    2700,
		DeviceWaitRounds:  3,
		DeviceWaitSeconds: 30,
	}
}

func newEnv(t *testing.T, f *fakeActs) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: wfID})
	env.RegisterWorkflow(DeviceTestWorkflow)
	env.RegisterActivity(f)
	return env
}

func input() DeviceTestInput {
	return DeviceTestInput{Project: "grp/p", Commit: "abcd1234", PipelineID: 42, Version: "1.2.3"}
}

func taskID(attempt string) string { return wfID + ":t1:" + attempt }

func passResult(id string) TaskResultSignal {
	return TaskResultSignal{TaskID: id, Status: "COMPLETED", ExitCode: 0, DurationSec: 12, CasesTotal: 10,
		Attachments: []Attachment{{Name: "logcat.txt", ObjectKey: "runs/x/logcat.txt"}}}
}

func TestHappyPathPassed(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}}
	env := newEnv(t, f)
	// 30s 时回传结果(期间无需心跳,未超 120s 租约)
	seedResult(f, passResult(taskID("a1")))
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalTaskResult, passResult(taskID("a1")))
	}, 30*time.Second)

	env.ExecuteWorkflow(DeviceTestWorkflow, input())
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow err: %v", env.GetWorkflowError())
	}
	var out DeviceTestOutput
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Tasks) != 1 || out.Tasks[0].Verdict != "PASSED" || out.Tasks[0].Attempt != 1 {
		t.Errorf("tasks = %+v", out.Tasks)
	}
	if len(f.dispatched) != 1 || f.dispatched[0].IdempotencyKey != taskID("a1") ||
		f.dispatched[0].DeviceSerial != "513cd3de" {
		t.Errorf("dispatched = %+v", f.dispatched)
	}
	if len(f.released) != 1 || f.released[0].InfraFail {
		t.Errorf("released = %+v(通过不得计入 fail_streak)", f.released)
	}
	if len(f.finished) != 1 || f.finished[0].Verdict != "PASSED" {
		t.Errorf("finished=%+v", f.finished)
	}
	if len(f.notifications) != 1 ||
		!strings.Contains(f.notifications[0], "PASSED") ||
		!strings.Contains(f.notifications[0], "12.0s cases=10/10") ||
		strings.Contains(f.notifications[0], "runs/x/") {
		t.Errorf("notification = %q(需含 verdict 与耗时/用例,不含附件对象键)", f.notifications)
	}
	// §11:PASSED 落规则裁决;不触发证据提取/分析(Phase 2 只对非 PASSED)
	if len(f.decisions) != 1 || f.decisions[0].Actor != "rule" {
		t.Errorf("decisions = %+v, want 单条 rule 裁决", f.decisions)
	}
	if len(f.evidenceCalls) != 0 || len(f.analyzeCalls) != 0 {
		t.Errorf("PASSED 不应触发证据提取/分析: evidence=%d analyze=%d",
			len(f.evidenceCalls), len(f.analyzeCalls))
	}
	// 新版本分支(设计文档 §5):workflow.GetVersion 对新 workflow 恒返回最大版本,
	// 所以本用例走的是 FailScope 载荷,而不是旧的 InfraFail 布尔。
	if len(f.released) != 1 {
		t.Fatalf("released = %d 次, want 1", len(f.released))
	}
	if got := f.released[0].FailScope; got != FailScopeOK {
		t.Errorf("PASSED 终态的 FailScope = %q, want %q", got, FailScopeOK)
	}
	if f.released[0].InfraFail {
		t.Error("新版本分支不该再填 InfraFail")
	}
}

func TestLeaseExpiryRetriesThenInfraError(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}}
	env := newEnv(t, f)
	// 不回传任何结果、CheckLease 始终返回未续期(nil)→ 每次 attempt 120s
	// 租约到期确认过期,机械重试 2 次后终态 INFRA_ERROR

	env.ExecuteWorkflow(DeviceTestWorkflow, input())
	var out DeviceTestOutput
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	if out.Tasks[0].Verdict != "INFRA_ERROR" || out.Tasks[0].Category != "INFRA" || out.Tasks[0].Attempt != 3 {
		t.Errorf("summary = %+v, want INFRA_ERROR attempt=3(1+2 次机械重试)", out.Tasks[0])
	}
	if len(f.dispatched) != 3 {
		t.Errorf("dispatched %d 次, want 3", len(f.dispatched))
	}
	if len(f.checkLeaseCalls) != 3 {
		t.Errorf("checkLeaseCalls = %v, want 3(每 attempt 到期确认一次)", f.checkLeaseCalls)
	}
	if len(f.canceled) != 3 {
		t.Errorf("canceled = %+v, 每次租约过期应尽力取消", f.canceled)
	}
	for _, r := range f.released {
		// 租约过期 = agent 失联(设计文档 §4):归 client,不是笼统的 "infra fail"。
		// InfraFail 是旧载荷字段,新版本分支不再填(由 FailScope 携带归因)。
		if r.FailScope != FailScopeClient {
			t.Errorf("release = %+v, 租约过期应归因 client(agent 失联)", r)
		}
	}
}

// TestCheckLeaseRenewsLease:原则 6——心跳只续 DB 租约(此处由 delayed callback
// 模拟把库内 expires_at 推后),workflow 不发/不收心跳 signal,以租约到期
// Durable Timer 触发 CheckLease:读到新 expires_at → 重设 Timer 继续等。
func TestCheckLeaseRenewsLease(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}}
	env := newEnv(t, f)
	// 心跳在 100s/200s 续库内租约(每次都在上次到期前);
	// workflow 在 120s/220s Timer 到期时 CheckLease 读到续期结果
	for _, d := range []time.Duration{100 * time.Second, 200 * time.Second} {
		env.RegisterDelayedCallback(func() {
			exp := env.Now().Add(120 * time.Second)
			f.mu.Lock()
			f.leaseExpiry = &exp
			f.mu.Unlock()
		}, d)
	}
	seedResult(f, passResult(taskID("a1")))
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalTaskResult, passResult(taskID("a1")))
	}, 290*time.Second)

	env.ExecuteWorkflow(DeviceTestWorkflow, input())
	var out DeviceTestOutput
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	if out.Tasks[0].Verdict != "PASSED" || out.Tasks[0].Attempt != 1 {
		t.Errorf("summary = %+v, CheckLease 续租后应 PASSED 且无重试", out.Tasks[0])
	}
	if len(f.checkLeaseCalls) != 2 {
		t.Errorf("checkLeaseCalls = %v, want 2 次(120s/220s Timer 到期)", f.checkLeaseCalls)
	}
}

func TestSignatureHitNoRetry(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}}
	env := newEnv(t, f)
	sig := TaskResultSignal{
		TaskID: taskID("a1"), Status: "COMPLETED", ExitCode: 0,
		SignaturesHit: []string{"cpu_fallback"},
	}
	seedResult(f, sig)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalTaskResult, sig)
	}, 10*time.Second)

	env.ExecuteWorkflow(DeviceTestWorkflow, input())
	var out DeviceTestOutput
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	if out.Tasks[0].Verdict != "TEST_FAILED" || out.Tasks[0].Category != "MODEL" || out.Tasks[0].Attempt != 1 {
		t.Errorf("summary = %+v, want TEST_FAILED(MODEL) 不重试", out.Tasks[0])
	}
	if len(f.dispatched) != 1 {
		t.Errorf("签名失败不得机械重试, dispatched=%d", len(f.dispatched))
	}
	if len(f.released) != 1 || f.released[0].InfraFail {
		t.Errorf("MODEL 失败不计设备 fail_streak: %+v", f.released)
	}
	// Phase 2:非 PASSED 触发证据提取;Analyzer 未启用(fake 返回 nil)时只落规则裁决
	if len(f.evidenceCalls) != 1 || len(f.analyzeCalls) != 1 {
		t.Errorf("非 PASSED 应提取证据并尝试分析: evidence=%d analyze=%d",
			len(f.evidenceCalls), len(f.analyzeCalls))
	}
	if len(f.decisions) != 1 || f.decisions[0].Actor != "rule" {
		t.Errorf("Analyzer 未启用应只落规则裁决: %+v", f.decisions)
	}
}

// TestUnknownRuleVersionRejected:未知 rule_version 拒绝启动并明确报错
// (差距 #7:绝不静默用最新版判定,重放安全)。
func TestUnknownRuleVersionRejected(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}}
	env := newEnv(t, f)
	in := input()
	in.RuleVersion = "verdict-rules-v99"
	env.ExecuteWorkflow(DeviceTestWorkflow, in)
	err := env.GetWorkflowError()
	if err == nil || !strings.Contains(err.Error(), "verdict-rules-v99") {
		t.Fatalf("err = %v, want 未知 rule_version 报错", err)
	}
	if len(f.dispatched) != 0 || len(f.finished) != 0 {
		t.Errorf("未知版本不得执行任何任务动作: dispatched=%d finished=%d",
			len(f.dispatched), len(f.finished))
	}
}

// TestRuntimeSignatureRefinesCategory:规则归类修复——SDK 测试程序不在
// result.json 自报签名,runtime 证据提取(variants.yaml 签名正则扫日志)
// 的命中作为规则引擎的额外输入(判定权仍在规则引擎,§9)。
// verdict 不因签名变"好";只有类别/reason 更精确(典型:CODE → DELEGATE)。
func TestRuntimeSignatureRefinesCategory(t *testing.T) {
	cases := []struct {
		name          string
		reported      []string // 设备自报(result.json signatures_hit)
		matched       []string // runtime 提取命中
		evidenceErr   error    // 非 nil 模拟提取失败
		wantVerdict   string
		wantCategory  string
		wantReasonSig string // 非空则 reason 必须含该签名 id
		wantAnalyze   int
		wantEvidence  int // 提取调用次数(失败时 activity 按 RetryPolicy 重试 3 次)
	}{
		{"提取命中dsp_unavailable→类别DELEGATE", nil, []string{"dsp_unavailable"}, nil,
			"TEST_FAILED", "DELEGATE", "dsp_unavailable", 1, 1},
		{"两者都空→现状CODE", nil, nil, nil,
			"TEST_FAILED", "CODE", "", 1, 1},
		{"自报与提取同名→去重不重复归类", []string{"dsp_unavailable"}, []string{"dsp_unavailable"}, nil,
			"TEST_FAILED", "DELEGATE", "dsp_unavailable", 1, 1},
		{"提取失败→降级现状CODE", nil, nil, errBoom,
			"TEST_FAILED", "CODE", "", 0, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeActs{specs: []TestSpec{spec1()}, matchedSigs: tc.matched, evidenceErr: tc.evidenceErr}
			env := newEnv(t, f)
			sig := TaskResultSignal{
				TaskID: taskID("a1"), Status: "COMPLETED", ExitCode: 0, CasesTotal: 10,
				CasesFailed: 2, SignaturesHit: tc.reported,
			}
			seedResult(f, sig)
			env.RegisterDelayedCallback(func() {
				env.SignalWorkflow(SignalTaskResult, sig)
			}, 10*time.Second)

			env.ExecuteWorkflow(DeviceTestWorkflow, input())
			var out DeviceTestOutput
			if err := env.GetWorkflowResult(&out); err != nil {
				t.Fatal(err)
			}
			sum := out.Tasks[0]
			if sum.Verdict != tc.wantVerdict || sum.Category != tc.wantCategory {
				t.Errorf("summary = %+v, want %s/%s", sum, tc.wantVerdict, tc.wantCategory)
			}
			if tc.wantReasonSig != "" && !strings.Contains(sum.Reason, tc.wantReasonSig) {
				t.Errorf("reason = %q, want 含签名 %q", sum.Reason, tc.wantReasonSig)
			}
			// 证据提取一次即复用(归类与分析共享);失败时按 RetryPolicy 重试 3 次后降级
			if len(f.evidenceCalls) != tc.wantEvidence {
				t.Errorf("evidenceCalls = %d, want %d", len(f.evidenceCalls), tc.wantEvidence)
			}
			if len(f.analyzeCalls) != tc.wantAnalyze {
				t.Errorf("analyzeCalls = %d, want %d", len(f.analyzeCalls), tc.wantAnalyze)
			}
			// decisions 落最终裁决(单条 rule)
			if len(f.decisions) != 1 || f.decisions[0].Actor != "rule" {
				t.Fatalf("decisions = %+v, want 单条 rule 最终裁决", f.decisions)
			}
			if !strings.Contains(string(f.decisions[0].Output), tc.wantCategory) {
				t.Errorf("最终裁决 output = %s, want 类别 %s", f.decisions[0].Output, tc.wantCategory)
			}
		})
	}
}

// TestSkippedVariantInOutputAndNotification:fleet 无匹配设备的变体不进
// acquire/dispatch,直接以 SKIPPED 出现在输出与通知中(§12 变体级触发)。
func TestSkippedVariantInOutputAndNotification(t *testing.T) {
	f := &fakeActs{
		specs:   []TestSpec{spec1()},
		skipped: []SkippedSpec{{Variant: "aarch64_Android_RKNN_2.3.2", Reason: "no capable device registered (soc=[RK3588 RK3566] capabilities=[rknpu])"}},
	}
	env := newEnv(t, f)
	seedResult(f, passResult(taskID("a1")))
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalTaskResult, passResult(taskID("a1")))
	}, 30*time.Second)

	env.ExecuteWorkflow(DeviceTestWorkflow, input())
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow err: %v", env.GetWorkflowError())
	}
	var out DeviceTestOutput
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Tasks) != 2 {
		t.Fatalf("tasks = %+v, want skipped + passed 各 1", out.Tasks)
	}
	sk, passed := out.Tasks[0], out.Tasks[1]
	if sk.Variant != "aarch64_Android_RKNN_2.3.2" || sk.Verdict != VerdictSkipped ||
		!strings.Contains(sk.Reason, "no capable device") {
		t.Errorf("skipped task = %+v", sk)
	}
	if passed.Verdict != "PASSED" {
		t.Errorf("passed task = %+v", passed)
	}
	// 被跳过的变体不得占用设备/派单
	if len(f.dispatched) != 1 {
		t.Errorf("dispatched = %d, want 1(跳过变体不派单)", len(f.dispatched))
	}
	if len(f.notifications) != 1 ||
		!strings.Contains(f.notifications[0], "SKIPPED") ||
		!strings.Contains(f.notifications[0], "no capable device") {
		t.Errorf("notification = %q(需含 SKIPPED 及原因)", f.notifications)
	}
}

// TestAnalysisSavedOnFailure 验证 Phase 2 接线:非 PASSED → 提取证据 →
// Analyzer 分析 → 规则与 hermes 两条裁决都落 decisions 表(§11 可回放)。
func TestAnalysisSavedOnFailure(t *testing.T) {
	f := &fakeActs{
		specs: []TestSpec{spec1()},
		analysis: &hermesclient.Analysis{
			AnalysisVersion: 1, Summary: "delegate fell back to CPU",
			SuggestedCategory: "MODEL", Confidence: 0.9,
		},
		snapshotID: "snap-1", // evidence 已持久化(差距 #6)
	}
	env := newEnv(t, f)
	sig := TaskResultSignal{
		TaskID: taskID("a1"), Status: "COMPLETED", ExitCode: 0,
		SignaturesHit: []string{"cpu_fallback"},
	}
	seedResult(f, sig)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalTaskResult, sig)
	}, 10*time.Second)

	env.ExecuteWorkflow(DeviceTestWorkflow, input())
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow err: %v", env.GetWorkflowError())
	}
	if len(f.analyzeCalls) != 1 || f.analyzeCalls[0].RuleCategory != "MODEL" {
		t.Fatalf("analyzeCalls = %+v, want 1 次且带规则类别 MODEL", f.analyzeCalls)
	}
	if len(f.decisions) != 2 {
		t.Fatalf("decisions = %+v, want rule + hermes 两条", f.decisions)
	}
	rule, herm := f.decisions[0], f.decisions[1]
	if rule.Actor != "rule" || !strings.Contains(string(rule.Output), "TEST_FAILED") {
		t.Errorf("rule 裁决 = %+v", rule)
	}
	if herm.Actor != "hermes" || herm.InputDigest != "deadbeef" ||
		herm.PromptVersion != hermesclient.PromptVersionAnalyze ||
		!strings.Contains(string(herm.Output), "delegate fell back to CPU") {
		t.Errorf("hermes 裁决 = %+v(需带 evidence 摘要/prompt 版本/分析本体)", herm)
	}
	// 差距 #6 决策可回放:hermes 裁决携带 evidence 快照引用;rule 裁决不带(基于 result)
	if herm.EvidenceSnapshotID != "snap-1" {
		t.Errorf("hermes 裁决 evidence_snapshot_id = %q, want snap-1", herm.EvidenceSnapshotID)
	}
	if rule.EvidenceSnapshotID != "" {
		t.Errorf("rule 裁决不应带快照引用: %+v", rule)
	}
	// 分析结论随 workflow 输出与飞书通知透出(§12.6 通知带 hermes 总结)
	var out DeviceTestOutput
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Tasks) != 1 || out.Tasks[0].Analysis == nil ||
		out.Tasks[0].Analysis.Summary != "delegate fell back to CPU" {
		t.Errorf("task summary 应携带 analysis: %+v", out.Tasks)
	}
	if len(f.notifications) != 1 ||
		!strings.Contains(f.notifications[0], "hermes: delegate fell back to CPU") {
		t.Errorf("notification = %q(需含 hermes summary 行)", f.notifications)
	}
}

func TestStaleResultSignalIgnored(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}}
	env := newEnv(t, f)
	env.RegisterDelayedCallback(func() { // 其他 task 的迟到结果:必须忽略
		env.SignalWorkflow(SignalTaskResult, passResult("some-other-task"))
	}, 5*time.Second)
	sig := TaskResultSignal{TaskID: taskID("a1"), Status: "COMPLETED", ExitCode: 7}
	seedResult(f, sig)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalTaskResult, sig)
	}, 10*time.Second)

	env.ExecuteWorkflow(DeviceTestWorkflow, input())
	var out DeviceTestOutput
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	if out.Tasks[0].Verdict != "TEST_FAILED" || out.Tasks[0].Category != "CODE" {
		t.Errorf("summary = %+v(应采用本 task 的结果:exit=7 → CODE)", out.Tasks[0])
	}
}

// TestDuplicateResultSignalRedelivered:接收端幂等(原则 3/差距清单 #5)——
// Outbox Relay 至少一次投递,同 task_id 的结果 signal 可能重投(此处连发两次);
// 且 Relay 侧载荷可收缩为轻量(仅 task_id)。workflow 必须:首个匹配即采纳,
// 重投无副作用;verdict 数据全部来自 LoadResult 权威读,与 signal 载荷无关。
func TestDuplicateResultSignalRedelivered(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}}
	env := newEnv(t, f)
	seedResult(f, passResult(taskID("a1")))
	env.RegisterDelayedCallback(func() {
		// Relay 重投:轻量载荷(只带 task_id),连发两次
		env.SignalWorkflow(SignalTaskResult, TaskResultSignal{TaskID: taskID("a1")})
		env.SignalWorkflow(SignalTaskResult, TaskResultSignal{TaskID: taskID("a1")})
	}, 5*time.Second)

	env.ExecuteWorkflow(DeviceTestWorkflow, input())
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow err: %v", env.GetWorkflowError())
	}
	var out DeviceTestOutput
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	// verdict 与统计来自 LoadResult 回读(signal 载荷为空也不得影响判定)
	if out.Tasks[0].Verdict != "PASSED" || out.Tasks[0].CasesTotal != 10 ||
		out.Tasks[0].DurationSec != 12 {
		t.Errorf("summary = %+v(应来自权威 results 行)", out.Tasks[0])
	}
	// 幂等:LoadResult/FinishTask 各一次,重复 signal 无二次效果
	if len(f.loadCalls) != 1 || f.loadCalls[0] != taskID("a1") {
		t.Errorf("loadCalls = %v, want 单次权威读", f.loadCalls)
	}
	if len(f.finished) != 1 || f.finished[0].Verdict != "PASSED" {
		t.Errorf("finished = %+v", f.finished)
	}
}

// TestLoadResultMissingIsInfraError:signal 到了但 results 表无权威行
// (outbox 链路异常):按 INFRA 处理走机械重试,绝不拿 signal 载荷兜底判 verdict。
func TestLoadResultMissingIsInfraError(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}}
	env := newEnv(t, f)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalTaskResult, TaskResultSignal{TaskID: taskID("a1")})
	}, 5*time.Second)
	// 每个 attempt 都会收到 signal,但 results 始终为空(不 seed)
	for _, a := range []string{"a2", "a3"} {
		aid := taskID(a)
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(SignalTaskResult, TaskResultSignal{TaskID: aid})
		}, 5*time.Second)
	}

	env.ExecuteWorkflow(DeviceTestWorkflow, input())
	var out DeviceTestOutput
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	if out.Tasks[0].Verdict != "INFRA_ERROR" || out.Tasks[0].Category != "INFRA" ||
		out.Tasks[0].Attempt != 3 {
		t.Errorf("summary = %+v, want INFRA_ERROR attempt=3", out.Tasks[0])
	}
	if len(f.loadCalls) != 3 {
		t.Errorf("loadCalls = %v, want 每 attempt 一次权威读", f.loadCalls)
	}
}

func TestDeviceBusyThenAvailable(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}, acquires: []*Lease{nil, nil}} // 前两轮无设备
	env := newEnv(t, f)
	seedResult(f, passResult(taskID("a1")))
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalTaskResult, passResult(taskID("a1")))
	}, 90*time.Second) // 2×30s 等待后拿到设备

	env.ExecuteWorkflow(DeviceTestWorkflow, input())
	var out DeviceTestOutput
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	if out.Tasks[0].Verdict != "PASSED" {
		t.Errorf("summary = %+v", out.Tasks[0])
	}
	if f.acquireCalls != 3 {
		t.Errorf("acquire 调用 %d 次, want 3(两轮忙 + 一次成功)", f.acquireCalls)
	}
}

func TestDispatchFailureRetriesOnFreshTask(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}}
	// 第一次 dispatch 持续失败(activity 层重试 3 次后仍败,注入 3 个错误),第二 attempt 成功
	f.dispatchErrs = []error{errBoom, errBoom, errBoom}
	env := newEnv(t, f)
	seedResult(f, passResult(taskID("a2")))
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalTaskResult, passResult(taskID("a2")))
	}, 60*time.Second)

	env.ExecuteWorkflow(DeviceTestWorkflow, input())
	var out DeviceTestOutput
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	if out.Tasks[0].Verdict != "PASSED" || out.Tasks[0].Attempt != 2 {
		t.Errorf("summary = %+v, want 第 2 attempt PASSED", out.Tasks[0])
	}
	// 幂等键随 attempt 变化,禁止复用
	if f.dispatched[len(f.dispatched)-1].IdempotencyKey != taskID("a2") {
		t.Errorf("dispatched = %+v", f.dispatched)
	}
}

// 归因表(设计文档 §4)。每一行一个用例;特别钉住两条改动前会误伤设备的:
// check lease 失败 → none(不是 device),终态 INFRA+FAILED → client(不是 device)。
func TestFailScope(t *testing.T) {
	cases := []struct {
		name     string
		site     releaseSite
		category rules.Category
		status   string
		want     FailScope
	}{
		{"CreateTask 失败是 Runtime 侧", siteCreateTaskFailed, "", "", FailScopeNone},
		{"Dispatch 失败连不上 agent", siteDispatchFailed, "", "", FailScopeClient},
		{"租约过期即 agent 失联", siteLeaseExpired, "", "", FailScopeClient},
		{"CheckLease 自身失败是 Runtime 侧", siteCheckLeaseFailed, "", "", FailScopeNone},
		{"hard deadline 成因两可", siteHardDeadline, "", "", FailScopeNone},
		{"人为取消", siteCanceled, "", "", FailScopeNone},
		{"LoadResult 失败是 outbox/DB", siteLoadResultFailed, "", "", FailScopeNone},
		{"终态 CANCELED 与 siteCanceled 一致归 none", siteTerminal, "", "CANCELED", FailScopeNone},
		{"终态 DEVICE 类", siteTerminal, rules.CategoryDevice, "FAILED", FailScopeDevice},
		{"终态 INFRA+FAILED 是 client 流水线", siteTerminal, rules.CategoryInfra, "FAILED", FailScopeClient},
		{"终态 INFRA+TIMEOUT 是工作负载属性", siteTerminal, rules.CategoryInfra, "TIMEOUT", FailScopeNone},
		{"终态 PASSED", siteTerminal, "", "COMPLETED", FailScopeOK},
		{"终态 CODE 类测试失败", siteTerminal, rules.CategoryCode, "COMPLETED", FailScopeOK},
		{"未覆盖组合保守取 none", releaseSite("unknown"), "", "", FailScopeNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := failScope(tc.site, tc.category, tc.status); got != tc.want {
				t.Errorf("failScope(%q, %q, %q) = %q, want %q",
					tc.site, tc.category, tc.status, got, tc.want)
			}
		})
	}
}

// ---- 通知卡片(Task 3,设计文档 §4)----

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

var allowedCardKeys = map[string]bool{
	"config": true, "wide_screen_mode": true, "header": true, "title": true,
	"template": true, "elements": true, "tag": true, "text": true, "content": true,
}

// walkCard 递归检查 key / tag / text 类型;返回全部违规项。
// textCtx 区分当前 map 是"文本节点"(tag 恒为 plain_text)还是"元素节点"
// (tag 只能是 div/hr):由父层的 key 决定——"text"/"title" 之下强制进入文本
// 语境,"elements" 数组里的每一项都是元素语境。
func walkCard(t *testing.T, v any) []string {
	t.Helper()
	var bad []string
	var walk func(node any, path string, textCtx bool)
	walk = func(node any, path string, textCtx bool) {
		switch n := node.(type) {
		case map[string]any:
			for k, val := range n {
				childPath := path + "." + k
				if !allowedCardKeys[k] {
					bad = append(bad, fmt.Sprintf("%s: 集合外 key %q", childPath, k))
					continue
				}
				switch k {
				case "tag":
					s, _ := val.(string)
					if textCtx {
						if s != "plain_text" {
							bad = append(bad, fmt.Sprintf("%s: text tag=%q,want plain_text", childPath, s))
						}
					} else if s != "div" && s != "hr" {
						bad = append(bad, fmt.Sprintf("%s: element tag=%q,want div|hr", childPath, s))
					}
				case "template":
					s, _ := val.(string)
					if s != "green" && s != "red" && s != "orange" {
						bad = append(bad, fmt.Sprintf("%s: template=%q,want green|red|orange", childPath, s))
					}
				case "text", "title":
					walk(val, childPath, true)
				default:
					walk(val, childPath, textCtx)
				}
			}
		case []any:
			for i, item := range n {
				// elements 数组里的每一项都是元素节点,不是文本节点。
				walk(item, fmt.Sprintf("%s[%d]", path, i), false)
			}
		}
	}
	walk(v, "$", false)
	return bad
}

func sampleInput() DeviceTestInput {
	return DeviceTestInput{Project: "algo-super-sdk", Commit: "9da3b9d9", PipelineID: 56, Version: "1.4.2"}
}

func sampleOutput() *DeviceTestOutput {
	return &DeviceTestOutput{Tasks: []TaskSummary{
		{Variant: "aarch64_Android_SNPE_2.21", Verdict: "PASSED",
			DurationSec: 412.3, CasesTotal: 38, CasesFailed: 0, Attempt: 1},
		{Variant: "aarch64_Android_SNPE_1.68", Verdict: "TEST_FAILED", Category: "CODE",
			DurationSec: 380.14, CasesTotal: 38, CasesFailed: 3, Attempt: 2,
			Reason:   "three cases crashed",
			Analysis: &hermesclient.Analysis{Summary: "DSP 初始化崩溃"}},
		{Variant: "aarch64_Linux_RKNN_2.3.2", Verdict: "SKIPPED",
			Reason: "fleet 无匹配设备"},
	}}
}

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
	cases := []struct{ name, payload string }{
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
		{Tag: "div", Text: txt("fleet 无匹配设备")},                       // 无指标行:CasesTotal=0 且 SKIPPED 省 attempt
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

// ---- 出现条件:各一条最小用例(设计 §4.3/§8) ----

func TestBuildNotificationCardMetricLineGating(t *testing.T) {
	// CasesTotal == 0 且非 SKIPPED → 只有 attempt,没有 duration/cases。
	out := &DeviceTestOutput{Tasks: []TaskSummary{
		{Variant: "v", Verdict: "TEST_FAILED", Attempt: 3},
	}}
	card := buildNotificationCard(DeviceTestInput{Project: "p"}, out)
	want := []CardElement{
		{Tag: "div", Text: &CardText{Tag: "plain_text", Content: "v  TEST_FAILED"}},
		{Tag: "div", Text: &CardText{Tag: "plain_text", Content: "attempt 3"}},
	}
	if !reflect.DeepEqual(card.Elements, want) {
		t.Errorf("CasesTotal==0 时指标行不对\ngot:\n%swant:\n%s",
			dumpElements(card.Elements), dumpElements(want))
	}
}

func TestBuildNotificationCardCategoryGating(t *testing.T) {
	cases := []struct {
		name    string
		verdict string
		cat     string
		wantMax string // 主行内容
	}{
		{"Category 为空", "TEST_FAILED", "", "v  TEST_FAILED"},
		{"PASSED 不显示 category", "PASSED", "CODE", "v  PASSED"},
		{"非 PASSED 且有 category", "TEST_FAILED", "CODE", "v  TEST_FAILED(CODE)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := &DeviceTestOutput{Tasks: []TaskSummary{
				{Variant: "v", Verdict: tc.verdict, Category: tc.cat, Attempt: 1},
			}}
			card := buildNotificationCard(DeviceTestInput{Project: "p"}, out)
			if got := card.Elements[0].Text.Content; got != tc.wantMax {
				t.Errorf("主行 = %q, want %q", got, tc.wantMax)
			}
		})
	}
}

func TestBuildNotificationCardReasonHermesGating(t *testing.T) {
	// SKIPPED + CasesTotal==0 → 没有指标行,只剩主行,便于单独考察 reason/hermes 门控。
	// Reason == "" → 无 reason 行;Analysis == nil || Summary == "" → 无 hermes 行。
	out := &DeviceTestOutput{Tasks: []TaskSummary{
		{Variant: "v", Verdict: "SKIPPED"},
	}}
	card := buildNotificationCard(DeviceTestInput{Project: "p"}, out)
	if len(card.Elements) != 1 {
		t.Fatalf("Reason/Summary 均空时应只有主行,got %d\n%s",
			len(card.Elements), dumpElements(card.Elements))
	}

	out2 := &DeviceTestOutput{Tasks: []TaskSummary{
		{Variant: "v", Verdict: "SKIPPED",
			Analysis: &hermesclient.Analysis{Summary: ""}},
	}}
	card2 := buildNotificationCard(DeviceTestInput{Project: "p"}, out2)
	if len(card2.Elements) != 1 {
		t.Fatalf("Summary 为空串时应无 hermes 行,got %d\n%s",
			len(card2.Elements), dumpElements(card2.Elements))
	}
}

func TestBuildNotificationCardAttemptGating(t *testing.T) {
	// SKIPPED 不显示 attempt;其余 verdict 显示。
	out := &DeviceTestOutput{Tasks: []TaskSummary{
		{Variant: "vs", Verdict: "SKIPPED", CasesTotal: 5, CasesFailed: 0, DurationSec: 1.0, Attempt: 1},
		{Variant: "vf", Verdict: "TEST_FAILED", CasesTotal: 5, CasesFailed: 1, DurationSec: 1.0, Attempt: 2},
	}}
	card := buildNotificationCard(DeviceTestInput{Project: "p"}, out)
	// Elements: [0]=vs 主行 [1]=vs 指标行 [2]=hr [3]=vf 主行 [4]=vf 指标行
	skippedMetric := card.Elements[1].Text.Content
	if strings.Contains(skippedMetric, "attempt") {
		t.Errorf("SKIPPED 变体的指标行不应含 attempt: %q", skippedMetric)
	}
	failedMetric := card.Elements[4].Text.Content
	if !strings.Contains(failedMetric, "attempt 2") {
		t.Errorf("非 SKIPPED 变体的指标行应含 attempt: %q", failedMetric)
	}
}

// ---- 渲染安全与截断(设计 §4.5) ----

// allContent 把卡片所有 CardElement.Text.Content(以及 header 标题)拼成一个串,
// 供裁剪/安全类测试用 strings.Contains 定位片段,不关心具体在哪一行。
func allContent(card NotificationCard) string {
	var b strings.Builder
	b.WriteString(card.Header.Title.Content)
	b.WriteString("\n")
	for _, e := range card.Elements {
		if e.Text != nil {
			b.WriteString(e.Text.Content)
			b.WriteString("\n")
		}
	}
	return b.String()
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func TestBuildNotificationCardTruncatesReason(t *testing.T) {
	long := strings.Repeat("a", 600)
	out := &DeviceTestOutput{Tasks: []TaskSummary{
		{Variant: "v", Verdict: "TEST_FAILED", Attempt: 1, Reason: long},
	}}
	card := buildNotificationCard(DeviceTestInput{Project: "p"}, out)
	reason := card.Elements[len(card.Elements)-1].Text.Content
	if reason == long {
		t.Fatal("超长 Reason 未被截断")
	}
	if utf8.RuneCountInString(reason) > cardReasonSummaryLimit {
		t.Errorf("截断后仍超过 %d rune: %d", cardReasonSummaryLimit, utf8.RuneCountInString(reason))
	}
	if strings.Contains(reason, long) {
		t.Error("截断后仍包含完整原文")
	}
	if !strings.HasSuffix(reason, cardTruncationMarker) {
		t.Errorf("截断后应带省略标记 %q,got %q", cardTruncationMarker, reason)
	}

	// 边界:恰好等于上限不截断,上限+1 必须截断到恰好上限个 rune——
	// 挡住"实际上限比 500 松/紧"(比如误把 max 写成 400)的实现。
	if got := truncateRunes(strings.Repeat("甲", cardReasonSummaryLimit), cardReasonSummaryLimit); got != strings.Repeat("甲", cardReasonSummaryLimit) {
		t.Error("恰好 500 rune 不应被截断")
	}
	if n := utf8.RuneCountInString(truncateRunes(strings.Repeat("甲", cardReasonSummaryLimit+1), cardReasonSummaryLimit)); n != cardReasonSummaryLimit {
		t.Errorf("501 rune 截断后应恰为 %d,got %d", cardReasonSummaryLimit, n)
	}
}

func TestBuildNotificationCardTruncatesSummary(t *testing.T) {
	long := strings.Repeat("b", 600)
	out := &DeviceTestOutput{Tasks: []TaskSummary{
		{Variant: "v", Verdict: "TEST_FAILED", Attempt: 1,
			Analysis: &hermesclient.Analysis{Summary: long}},
	}}
	card := buildNotificationCard(DeviceTestInput{Project: "p"}, out)
	hermes := card.Elements[len(card.Elements)-1].Text.Content
	if strings.Contains(hermes, long) {
		t.Fatal("超长 Summary 未被截断")
	}
	// 截断后的内容(去掉 "hermes: " 前缀)不应超过上限。
	content := strings.TrimPrefix(hermes, "hermes: ")
	if utf8.RuneCountInString(content) > cardReasonSummaryLimit {
		t.Errorf("截断后仍超过 %d rune: %d", cardReasonSummaryLimit, utf8.RuneCountInString(content))
	}
	if !strings.HasSuffix(content, cardTruncationMarker) {
		t.Errorf("截断后应带省略标记 %q,got %q", cardTruncationMarker, content)
	}
}

func TestBuildNotificationCardTruncatesChineseReasonValidUTF8(t *testing.T) {
	long := strings.Repeat("甲", 600)
	out := &DeviceTestOutput{Tasks: []TaskSummary{
		{Variant: "v", Verdict: "TEST_FAILED", Attempt: 1, Reason: long},
	}}
	card := buildNotificationCard(DeviceTestInput{Project: "p"}, out)
	reason := card.Elements[len(card.Elements)-1].Text.Content
	if !utf8.ValidString(reason) {
		t.Error("纯中文 Reason 截断后不是合法 UTF-8")
	}
	if utf8.RuneCountInString(reason) > cardReasonSummaryLimit {
		t.Errorf("截断后仍超过 %d rune", cardReasonSummaryLimit)
	}
}

func TestBuildNotificationCardTruncatesChineseSummaryValidUTF8(t *testing.T) {
	long := strings.Repeat("乙", 600)
	out := &DeviceTestOutput{Tasks: []TaskSummary{
		{Variant: "v", Verdict: "TEST_FAILED", Attempt: 1,
			Analysis: &hermesclient.Analysis{Summary: long}},
	}}
	card := buildNotificationCard(DeviceTestInput{Project: "p"}, out)
	hermes := card.Elements[len(card.Elements)-1].Text.Content
	if !utf8.ValidString(hermes) {
		t.Error("纯中文 Summary 截断后不是合法 UTF-8")
	}
	// 与 TestBuildNotificationCardTruncatesChineseReasonValidUTF8 对称:去掉
	// "hermes: " 前缀后按上限校验 rune 数,不放过"截断没生效"这种回归。
	content := strings.TrimPrefix(hermes, "hermes: ")
	if utf8.RuneCountInString(content) > cardReasonSummaryLimit {
		t.Errorf("截断后仍超过 %d rune: %d", cardReasonSummaryLimit, utf8.RuneCountInString(content))
	}
}

// 恶意/不可信文本(markdown 链接、<at> 语法)必须原样以字面文本出现,
// 且节点 tag 恒为 plain_text——飞书不会把它们解释成链接或 @ 提及。
func TestBuildNotificationCardRendersUntrustedTextLiterally(t *testing.T) {
	in := DeviceTestInput{Project: "a[x](http://evil)b", Commit: "c", PipelineID: 1, Version: "1.0.0"}
	out := &DeviceTestOutput{Tasks: []TaskSummary{
		{Variant: "v<at user_id=\"all\">", Verdict: "TEST_FAILED", Attempt: 1,
			Reason:   "r<at user_id=\"all\">[click](http://evil)",
			Analysis: &hermesclient.Analysis{Summary: "s<at user_id=\"all\">[click](http://evil)"}},
	}}
	card := buildNotificationCard(in, out)

	if !strings.Contains(card.Header.Title.Content, "a[x](http://evil)b") {
		t.Errorf("Project 未原样出现在 header: %q", card.Header.Title.Content)
	}
	if card.Header.Title.Tag != "plain_text" {
		t.Errorf("header tag = %q, want plain_text", card.Header.Title.Tag)
	}
	body := allContent(card)
	for _, want := range []string{
		`v<at user_id="all">`,
		`r<at user_id="all">[click](http://evil)`,
		`s<at user_id="all">[click](http://evil)`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("卡片正文缺少字面文本 %q", want)
		}
	}
	for _, e := range card.Elements {
		if e.Text != nil && e.Text.Tag != "plain_text" {
			t.Errorf("节点 tag = %q, want plain_text", e.Text.Tag)
		}
	}
}

// padTo 造一批变体:第 i 个的 Reason 带唯一标记 MARK-%03d,填充到 detailRunes 长
// (< 500 rune,避免撞上截断逻辑,让本测试只考察裁剪)。总量顶到预算的数倍,
// 保证正确实现**必须**丢掉一批详情。
//
// 注意:不要在这里预测"该丢几个"——JSON 骨架、主行、hr、header 都算进预算,
// 测试里重算一遍就是把实现的账抄第二遍,抄错了就会稳定误杀正确实现。
// 分界由下面从卡片内容**观测**得出,断言只压形状(连续后缀、精确数量、不过度裁剪)。
func padTo(out *DeviceTestOutput) (*DeviceTestOutput, []string) {
	const detailRunes, variants = 400, 60
	var markers []string
	add := func(variant string) {
		m := fmt.Sprintf("MARK-%03d", len(markers))
		markers = append(markers, m)
		out.Tasks = append(out.Tasks, TaskSummary{
			Variant: variant, Verdict: "TEST_FAILED", Attempt: 1,
			Reason: m + strings.Repeat("甲", detailRunes-len([]rune(m))),
		})
	}
	add("v-first")
	// 填充变体一律排在 v-first 与 v-last 之间,保证首尾两端的标记序号确定
	for i := 0; i < variants-2; i++ {
		add(fmt.Sprintf("v-fill-%03d", len(markers)))
	}
	add("v-last")
	return out, markers
}

// 裁剪必须保留前面变体的详情、只丢末尾的(设计 §4.5 第 1 步)。
func TestBuildNotificationCardTrimsFromTail(t *testing.T) {
	out, markers := padTo(&DeviceTestOutput{})
	card := buildNotificationCard(DeviceTestInput{Project: "p"}, out)
	body := allContent(card)

	// 1) 保留的标记必须是**完整前缀**、丢弃的必须是**完整后缀**。
	//    只搜"第一个在、最后一个不在"挡不住"中间也删了几个"。
	kept := 0
	for kept < len(markers) && strings.Contains(body, markers[kept]) {
		kept++
	}
	for i := kept; i < len(markers); i++ {
		if strings.Contains(body, markers[i]) {
			t.Fatalf("标记 %s 在断点 %d 之后仍存在:丢弃的不是连续后缀", markers[i], kept)
		}
	}
	if kept == 0 {
		t.Fatal("详情被删光了:至少要保住最前面几个变体的详情")
	}
	omitted := len(markers) - kept
	if omitted == 0 {
		t.Fatal("这批输入远超预算,不该一个详情都没丢")
	}

	// 2) 变体主行一个都不许删——只删可选行
	for i := range out.Tasks {
		if !strings.Contains(body, out.Tasks[i].Variant) {
			t.Errorf("变体 %s 的主行被删了,只应删可选行", out.Tasks[i].Variant)
		}
	}

	// 2b) 指标行也不许删:设计 §4.5 第 1 步只丢 reason/hermes,metric 保留。
	// padTo 里每个变体的 Attempt 都是 1、CasesTotal 都是 0,所以指标行内容
	// 逐变体相同(都是 "attempt 1"),没法按变体区分——改用出现次数:
	// 只要有一个变体的指标行被连带丢了,计数就会少于变体总数。
	if got := strings.Count(body, "attempt 1"); got != len(out.Tasks) {
		t.Errorf("指标行出现次数 = %d,want %d(应逐变体保留,只丢 reason/hermes)",
			got, len(out.Tasks))
	}

	// 3) 标注里的数字必须与实际丢弃数**逐字相符**,写死 999 或差一都要红
	want := fmt.Sprintf("（%d 个变体的详情已省略）", omitted) // 全角括号,与实现逐字一致
	if !strings.Contains(body, want) {
		t.Errorf("缺少或写错省略标注,期望包含 %q", want)
	}

	// 4) 不许过度裁剪。单个详情约占预算 4%,逐个丢到不超预算为止会停在 90% 以上;
	//    "一超预算就把所有可选行删光"这类实现会掉到 10% 上下,被这条挡住。
	n := len(mustMarshal(t, card))
	if n > 30*1024 {
		t.Errorf("裁剪后仍超预算: %d", n)
	}
	if n < 30*1024*3/4 {
		t.Errorf("只剩 %d 字节(预算 %d):裁剪过度,应逐个丢到刚好装下为止", n, 30*1024)
	}
}
