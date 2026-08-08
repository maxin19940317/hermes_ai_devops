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
	"go.temporal.io/sdk/temporal"
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
	recordErrs   []error
	selectErr    error

	acquireCalls  int
	selectCalls   int
	recordCalls   []RecordWorkflowRunRequest
	callOrder     []string
	created       []TaskRow
	dispatched    []DispatchRequest
	canceled      []CancelRequest
	finished      []FinishRequest
	released      []ReleaseRequest
	notifications []string
	notifyCards   []NotifyCardRequest

	analysis      *hermesclient.Analysis // 非 nil 模拟 Analyzer 已启用
	evidenceCalls []ExtractEvidenceRequest
	analyzeCalls  []AnalyzeRequest
	decisions     []DecisionRow
	matchedSigs   []string // ExtractEvidence 返回的 runtime 提取签名命中
	evidenceErr   error    // 非 nil 模拟提取失败(降级路径)
	snapshotID    string   // ExtractEvidence 返回的快照 id(空 = 降级未持久化)

	gate      EscalationGateResponse // EscalationGate 返回值(Enabled 缺省 false = 升级禁用)
	gateCalls []EscalationGateRequest
	escCalls  []EscalationRequest
	escErr    error // 非 nil 模拟 Escalate 活动失败

	savedMetrics []SaveMetricsRequest // SaveMetrics 活动调用记录(PASSED 基线沉淀)

	results   map[string]ResultRecord // LoadResult 的权威数据源(模拟 results 表)
	loadCalls []string

	leaseExpiry     *time.Time // CheckLease 的返回(模拟 device_leases.lease_expires_at);nil = 未续期
	checkLeaseCalls []string
}

var defaultLease = &Lease{DeviceID: "dev1", Serial: "513cd3de", ClientID: "c1", ClientBaseURL: "https://client:8443"}

func (f *fakeActs) RecordWorkflowRun(_ context.Context, req RecordWorkflowRunRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordCalls = append(f.recordCalls, req)
	f.callOrder = append(f.callOrder, "record")
	if len(f.recordErrs) == 0 {
		return nil
	}
	err := f.recordErrs[0]
	f.recordErrs = f.recordErrs[1:]
	return err
}
func (f *fakeActs) SelectTestSpecs(_ context.Context, _ DeviceTestInput) (*SpecSelection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.selectCalls++
	f.callOrder = append(f.callOrder, "select")
	return &SpecSelection{Specs: f.specs, Skipped: f.skipped}, f.selectErr
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
func (f *fakeActs) ExplainNoDevice(_ context.Context, _ ExplainNoDeviceRequest) (string, error) {
	return "无可用设备:测试需求;匹配设备:dev1 当前离线", nil
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
func (f *fakeActs) NotifyCard(_ context.Context, req NotifyCardRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notifyCards = append(f.notifyCards, req)
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
func (f *fakeActs) EscalationGate(_ context.Context, r EscalationGateRequest) (*EscalationGateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gateCalls = append(f.gateCalls, r)
	g := f.gate
	return &g, nil
}
func (f *fakeActs) Escalate(_ context.Context, r EscalationRequest) (*EscalationResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.escCalls = append(f.escCalls, r)
	if f.escErr != nil {
		return nil, f.escErr
	}
	return &EscalationResponse{KanbanTaskID: "t_1", IdempotencyKey: "k", Result: "created"}, nil
}
func (f *fakeActs) SaveMetrics(_ context.Context, r SaveMetricsRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.savedMetrics = append(f.savedMetrics, r)
	return nil
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
	return newEnvID(t, f, wfID)
}

// newEnvID 用指定 workflow ID 创建环境(变体级输入的 workflow ID 带 -scope 后缀,
// 与 wfID 常量不同;方案 A 通知门控测试用它)。
func newEnvID(t *testing.T, f *fakeActs, id string) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: id})
	env.RegisterWorkflow(DeviceTestWorkflow)
	env.RegisterActivity(f)
	return env
}

// input 是测试主路径输入(2026-08-06 方案 A):Scope 空 = bundle 形态,workflow ID
// 与 wfID 常量一致。依赖通知的测试用 inputVariant()(变体级 kick,Scope 非空)。
func input() DeviceTestInput {
	return DeviceTestInput{Project: "grp/p", Commit: "abcd1234", PipelineID: 42, Version: "1.2.3"}
}

// inputVariant 返回变体级 kick 输入(Scope 非空):workflow ID 带 -scope 后缀,
// 通知门控(2026-08-06 方案 A:只有变体级才通知)放行。
// 注意:其 WorkflowID() 与 wfID 常量不同,使用处必须 SetStartWorkflowOptions 或
// 用 WorkflowID() 派生 ID。
func inputVariant() DeviceTestInput {
	in := input()
	in.Scope = "aarch64_Android_SNPE_2.21"
	return in
}

func taskID(attempt string) string { return wfID + ":t1:" + attempt }

// taskIDFor 按指定 workflow ID 派生 task_id(变体级输入的 workflow ID 带 scope 后缀)。
func taskIDFor(wf, attempt string) string { return wf + ":t1:" + attempt }

func passResult(id string) TaskResultSignal {
	return TaskResultSignal{TaskID: id, Status: "COMPLETED", ExitCode: 0, DurationSec: 12, CasesTotal: 10,
		Attachments: []Attachment{{Name: "logcat.txt", ObjectKey: "runs/x/logcat.txt"}}}
}

func TestHappyPathPassed(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}}
	in := inputVariant() // 变体级 kick:通知门控放行(方案 A)
	env := newEnvID(t, f, in.WorkflowID())
	tid := taskIDFor(in.WorkflowID(), "a1")
	// 30s 时回传结果(期间无需心跳,未超 120s 租约)
	seedResult(f, passResult(tid))
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalTaskResult, passResult(tid))
	}, 30*time.Second)

	env.ExecuteWorkflow(DeviceTestWorkflow, in)
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
	if len(f.dispatched) != 1 || f.dispatched[0].IdempotencyKey != tid ||
		f.dispatched[0].DeviceSerial != "513cd3de" {
		t.Errorf("dispatched = %+v", f.dispatched)
	}
	if len(f.released) != 1 || f.released[0].InfraFail {
		t.Errorf("released = %+v(通过不得计入 fail_streak)", f.released)
	}
	if len(f.finished) != 1 || f.finished[0].Verdict != "PASSED" {
		t.Errorf("finished=%+v", f.finished)
	}
	// 新 workflow 走 notify-card 版本分支(设计文档 §5):Notify 零调用,
	// 断言改落在 NotifyCard 的 FallbackText 上。
	if len(f.notifications) != 0 {
		t.Errorf("notifications = %v, want 零调用(新 workflow 应走 NotifyCard)", f.notifications)
	}
	if len(f.notifyCards) != 1 {
		t.Fatalf("notifyCards = %+v, want 1 次", f.notifyCards)
	}
	fallback := f.notifyCards[0].FallbackText
	if !strings.Contains(fallback, "PASSED") ||
		!strings.Contains(fallback, "12.0s cases=10/10") ||
		strings.Contains(fallback, "runs/x/") {
		t.Errorf("fallback text = %q(需含 verdict 与耗时/用例,不含附件对象键)", fallback)
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

// TestHappyPathPassedSavesMetrics:PASSED 且结果带 metrics 时,workflow 调
// SaveMetrics 沉淀基线(2026-08-06 修复:此前指标保存挂在只在非 PASSED
// 运行的 ExtractEvidence 里,metrics 表恒空),且随通知透出。
func TestHappyPathPassedSavesMetrics(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}}
	in := inputVariant()
	env := newEnvID(t, f, in.WorkflowID())
	tid := taskIDFor(in.WorkflowID(), "a1")
	res := passResult(tid)
	res.Metrics = map[string]float64{"ocr_test.inference_ms_avg": 1451.39}
	seedResult(f, res)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalTaskResult, res)
	}, 30*time.Second)

	env.ExecuteWorkflow(DeviceTestWorkflow, in)
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow err: %v", env.GetWorkflowError())
	}
	var out DeviceTestOutput
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Tasks) != 1 || out.Tasks[0].Verdict != "PASSED" {
		t.Fatalf("tasks = %+v", out.Tasks)
	}
	if got := out.Tasks[0].Metrics["ocr_test.inference_ms_avg"]; got != 1451.39 {
		t.Errorf("summary metrics = %v, want 1451.39", out.Tasks[0].Metrics)
	}
	if len(f.savedMetrics) != 1 {
		t.Fatalf("SaveMetrics 调用 = %d 次, want 1", len(f.savedMetrics))
	}
	sm := f.savedMetrics[0]
	if sm.TaskID != tid || sm.Project != "grp/p" || sm.Variant != spec1().Variant ||
		sm.Metrics["ocr_test.inference_ms_avg"] != 1451.39 {
		t.Errorf("SaveMetrics req = %+v", sm)
	}
	fallback := f.notifyCards[0].FallbackText
	if !strings.Contains(fallback, "ocr_test=1451.4ms") {
		t.Errorf("fallback text = %q, want 含推理耗时", fallback)
	}
}

func TestWorkflowRecordsRunBeforeSelect(t *testing.T) {
	f := &fakeActs{}
	env := newEnv(t, f)
	in := input()
	in.RuleVersion = rules.DefaultVersion
	in.Scope = "v2"
	in.Attempt = 2
	in.SourceWorkflowID = "device-test-grp/p-gabcd1234-p42"
	in.Packages = []PackageRef{{Variant: "v2"}, {Variant: "v1"}, {Variant: "v2"}}

	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: in.WorkflowID()})
	env.ExecuteWorkflow(DeviceTestWorkflow, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if len(f.callOrder) < 2 ||
		!reflect.DeepEqual(f.callOrder[:2], []string{"record", "select"}) {
		t.Fatalf("call order = %v", f.callOrder)
	}
	if len(f.recordCalls) != 1 {
		t.Fatalf("record calls = %d", len(f.recordCalls))
	}
	want := RecordWorkflowRunRequest{
		WorkflowID: in.WorkflowID(), Project: in.Project, CommitSHA: in.Commit,
		PipelineID: in.PipelineID, Version: in.Version, RuleVersion: rules.DefaultVersion,
		Scope: in.Scope, Attempt: in.Attempt, Variants: []string{"v1", "v2"},
		SourceWorkflowID: in.SourceWorkflowID,
	}
	if !reflect.DeepEqual(f.recordCalls[0], want) {
		t.Fatalf("record request = %+v, want %+v", f.recordCalls[0], want)
	}
	if got := []string{in.Packages[0].Variant, in.Packages[1].Variant, in.Packages[2].Variant}; !reflect.DeepEqual(got, []string{"v2", "v1", "v2"}) {
		t.Fatalf("input package order mutated: %v", got)
	}
}

func TestWorkflowRecordRetriesPastDefaultMaximum(t *testing.T) {
	f := &fakeActs{recordErrs: []error{errBoom, errBoom, errBoom, errBoom}}
	env := newEnv(t, f)
	env.SetTestTimeout(2 * time.Second)

	env.ExecuteWorkflow(DeviceTestWorkflow, input())
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if len(f.recordCalls) != 5 {
		t.Fatalf("record calls = %d, want 5", len(f.recordCalls))
	}
	if f.selectCalls != 1 {
		t.Fatalf("select calls = %d, want 1", f.selectCalls)
	}
}

// TestBundleWorkflowSilent:方案 A(2026-08-06)——bundle workflow(Scope 空)
// 只测不通知,补测 kick 漏掉的变体;通知由变体级 workflow(kick)承担,
// 避免 13 条/流水线的重复轰炸。bundle 仍正常执行测试并落库。
func TestBundleWorkflowSilent(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}, skipped: []SkippedSpec{}}
	env := newEnv(t, f)
	seedResult(f, passResult(taskID("a1")))
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalTaskResult, passResult(taskID("a1")))
	}, 30*time.Second)

	bundleIn := input()
	bundleIn.Scope = "" // bundle 全量(webhook):只测不通知
	env.ExecuteWorkflow(DeviceTestWorkflow, bundleIn)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var out DeviceTestOutput
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	// bundle 仍执行测试(变体跑完,结果落 out)
	if len(out.Tasks) != 1 || out.Tasks[0].Verdict != "PASSED" {
		t.Errorf("bundle 仍应执行测试: %+v", out.Tasks)
	}
	// 但不发任何通知(bundle 静默)
	if len(f.notifyCards) != 0 {
		t.Errorf("bundle workflow 不应发通知卡片: %+v", f.notifyCards)
	}
	if len(f.notifications) != 0 {
		t.Errorf("bundle workflow 不应发纯文本通知: %+v", f.notifications)
	}
	// 落库照常(decisions rule 裁决)
	if len(f.decisions) != 1 || f.decisions[0].Actor != "rule" {
		t.Errorf("bundle 结果应落 decisions: %+v", f.decisions)
	}
}

func TestWorkflowRecordPermanentFailureBlocksSelect(t *testing.T) {
	permanent := temporal.NewNonRetryableApplicationError(
		"immutable conflict", "WorkflowRunPermanent", errBoom)
	f := &fakeActs{recordErrs: []error{permanent}}
	env := newEnv(t, f)
	env.SetTestTimeout(2 * time.Second)

	env.ExecuteWorkflow(DeviceTestWorkflow, input())
	if env.GetWorkflowError() == nil {
		t.Fatal("workflow succeeded")
	}
	if len(f.recordCalls) != 1 || f.selectCalls != 0 {
		t.Fatalf("record=%d select=%d, want 1/0", len(f.recordCalls), f.selectCalls)
	}
}

func TestWorkflowRecordRetryPolicyDoesNotLeakToSelect(t *testing.T) {
	f := &fakeActs{selectErr: errBoom}
	env := newEnv(t, f)
	env.SetTestTimeout(2 * time.Second)

	env.ExecuteWorkflow(DeviceTestWorkflow, input())
	if env.GetWorkflowError() == nil {
		t.Fatal("workflow succeeded")
	}
	if len(f.recordCalls) != 1 || f.selectCalls != 3 {
		t.Fatalf("record=%d select=%d, want 1/3", len(f.recordCalls), f.selectCalls)
	}
}

func TestWorkflowRecordUsesActualExecutionID(t *testing.T) {
	f := &fakeActs{}
	env := newEnv(t, f)
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: "unexpected-execution-id"})

	env.ExecuteWorkflow(DeviceTestWorkflow, input())
	err := env.GetWorkflowError()
	if err == nil || !strings.Contains(err.Error(),
		`workflow execution id "unexpected-execution-id" does not match input id "`+wfID+`"`) {
		t.Fatalf("workflow error = %v", err)
	}
	if len(f.recordCalls) != 0 || f.selectCalls != 0 {
		t.Fatalf("record=%d select=%d, want 0/0", len(f.recordCalls), f.selectCalls)
	}
}

// TestWorkflowSendsCardWithVerbatimFallback 验证 notify-card 版本分支(设计文档 §5):
// 新 workflow 一律走 NotifyCard,且载荷的 FallbackText 必须与 buildNotification
// 对同一输入的输出逐字节相同——降级文本只能有一个真源,不许 activity 侧另拼一份。
func TestWorkflowSendsCardWithVerbatimFallback(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}}
	in := inputVariant()
	env := newEnvID(t, f, in.WorkflowID())
	tid := taskIDFor(in.WorkflowID(), "a1")
	seedResult(f, passResult(tid))
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalTaskResult, passResult(tid))
	}, 30*time.Second)

	env.ExecuteWorkflow(DeviceTestWorkflow, in)
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow err: %v", env.GetWorkflowError())
	}
	var out DeviceTestOutput
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	want := buildNotification(in, &out)

	if len(f.notifications) != 0 {
		t.Errorf("Notify 调用次数 = %d, want 0(新 workflow 不应再走纯文本分支): %v", len(f.notifications), f.notifications)
	}
	if len(f.notifyCards) != 1 {
		t.Fatalf("notifyCards = %+v, want 1 次", f.notifyCards)
	}
	if got := f.notifyCards[0].FallbackText; got != want {
		t.Errorf("FallbackText = %q, want 与 buildNotification 逐字节相同 %q", got, want)
	}
	// 只查 FallbackText 查不出 workflow 是否真把卡片本体传下去了——
	// Card 字段单独断言,确保"接线"这条线真的被覆盖。
	if got, wantCard := f.notifyCards[0].Card, buildNotificationCard(in, &out, ""); !reflect.DeepEqual(got, wantCard) {
		t.Errorf("Card = %+v, want 与 buildNotificationCard 同源 %+v", got, wantCard)
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
	in := inputVariant()
	env := newEnvID(t, f, in.WorkflowID())
	tid := taskIDFor(in.WorkflowID(), "a1")
	seedResult(f, passResult(tid))
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalTaskResult, passResult(tid))
	}, 30*time.Second)

	env.ExecuteWorkflow(DeviceTestWorkflow, in)
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
	if len(f.notifications) != 0 {
		t.Errorf("notifications = %v, want 零调用(新 workflow 应走 NotifyCard)", f.notifications)
	}
	if len(f.notifyCards) != 1 {
		t.Fatalf("notifyCards = %+v, want 1 次", f.notifyCards)
	}
	if fallback := f.notifyCards[0].FallbackText; !strings.Contains(fallback, "SKIPPED") ||
		!strings.Contains(fallback, "no capable device") {
		t.Errorf("fallback text = %q(需含 SKIPPED 及原因)", fallback)
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
	in := inputVariant()
	env := newEnvID(t, f, in.WorkflowID())
	tid := taskIDFor(in.WorkflowID(), "a1")
	sig := TaskResultSignal{
		TaskID: tid, Status: "COMPLETED", ExitCode: 0,
		SignaturesHit: []string{"cpu_fallback"},
	}
	seedResult(f, sig)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalTaskResult, sig)
	}, 10*time.Second)

	env.ExecuteWorkflow(DeviceTestWorkflow, in)
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
	if len(f.notifications) != 0 {
		t.Errorf("notifications = %v, want 零调用(新 workflow 应走 NotifyCard)", f.notifications)
	}
	if len(f.notifyCards) != 1 {
		t.Fatalf("notifyCards = %+v, want 1 次", f.notifyCards)
	}
	if fallback := f.notifyCards[0].FallbackText; !strings.Contains(fallback, "hermes: delegate fell back to CPU") {
		t.Errorf("fallback text = %q(需含 hermes summary 行)", fallback)
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

// TestDispatchFailureRetriesOnFreshTask(t *testing.T) {
// 	f := &fakeActs{specs: []TestSpec{spec1()}}
// 	// 第一次 dispatch 持续失败(activity 层重试 3 次后仍败,注入 3 个错误),第二 attempt 成功
// 	f.dispatchErrs = []error{errBoom, errBoom, errBoom}
// 	env := newEnv(t, f)
// 	seedResult(f, passResult(taskID("a2")))
// 	env.RegisterDelayedCallback(func() {
// 		env.SignalWorkflow(SignalTaskResult, passResult(taskID("a2")))
// 	}, 60*time.Second)

// 	env.ExecuteWorkflow(DeviceTestWorkflow, input())
// 	var out DeviceTestOutput
// 	if err := env.GetWorkflowResult(&out); err != nil {
// 		t.Fatal(err)
// 	}
// 	if out.Tasks[0].Verdict != "PASSED" || out.Tasks[0].Attempt != 2 {
// 		t.Errorf("summary = %+v, want 第 2 attempt PASSED", out.Tasks[0])
// 	}
// 	// 幂等键随 attempt 变化,禁止复用
// 	if f.dispatched[len(f.dispatched)-1].IdempotencyKey != taskID("a2") {
// 		t.Errorf("dispatched = %+v", f.dispatched)
// 	}
// }

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

// TestEscalationGateMatrix:升级门槛矩阵(docs/superpowers/specs/2026-07-30 §5)——
// category ∈ {CODE,MODEL,DELEGATE,DEVICE} × 启用 × 判重 × 置信度 × 分析缺失。
// 幂等键尾段:有签名命中取首个签名 id(自报优先),否则 category。
func TestEscalationGateMatrix(t *testing.T) {
	cases := []struct {
		name         string
		reported     []string
		matched      []string
		extraSigCat  map[string]rules.Category
		gateEnabled  bool
		already      bool
		analysisNil  bool
		confidence   float64
		wantEscalate bool
		wantSigOrCat string
	}{
		{"CODE 升级(无签名取 category)", nil, nil, nil, true, false, false, 0.9, true, "CODE"},
		{"MODEL 升级(自报签名)", []string{"cpu_fallback"}, nil, nil, true, false, false, 0.9, true, "cpu_fallback"},
		{"DELEGATE 升级(runtime 提取命中)", nil, []string{"dsp_unavailable"}, nil, true, false, false, 0.9, true, "dsp_unavailable"},
		{"DEVICE 升级", []string{"sig_dev"}, nil, map[string]rules.Category{"sig_dev": "DEVICE"}, true, false, false, 0.9, true, "sig_dev"},
		{"INFRA 不升级(类别门槛)", []string{"sig_infra"}, nil, map[string]rules.Category{"sig_infra": "INFRA"}, true, false, false, 0.9, false, ""},
		{"endpoint 未配置不升级", nil, nil, nil, false, false, false, 0.9, false, ""},
		{"已升级判重不重复", nil, nil, nil, true, true, false, 0.9, false, ""},
		{"低置信不升级", nil, nil, nil, true, false, false, 0.5, false, ""},
		{"分析缺失不升级", nil, nil, nil, true, false, true, 0, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := spec1()
			for k, v := range tc.extraSigCat {
				spec.SignatureCategory[k] = v
			}
			f := &fakeActs{
				specs:       []TestSpec{spec},
				matchedSigs: tc.matched,
				gate:        EscalationGateResponse{Enabled: tc.gateEnabled, MinConfidence: 0.7, AlreadyEscalated: tc.already},
			}
			if !tc.analysisNil {
				f.analysis = &hermesclient.Analysis{AnalysisVersion: 1, Summary: "s", Confidence: tc.confidence}
			}
			env := newEnv(t, f)
			sig := TaskResultSignal{
				TaskID: taskID("a1"), Status: "COMPLETED", ExitCode: 1, CasesTotal: 10,
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
			if tc.wantEscalate {
				if len(f.escCalls) != 1 {
					t.Fatalf("escCalls = %d, want 1", len(f.escCalls))
				}
				req := f.escCalls[0]
				if req.SignatureOrCategory != tc.wantSigOrCat ||
					req.Project != "grp/p" || req.Commit != "abcd1234" || req.PipelineID != 42 ||
					req.TaskID != taskID("a1") || req.Verdict != "TEST_FAILED" ||
					req.Analysis == nil {
					t.Errorf("escalation req = %+v", req)
				}
			} else if len(f.escCalls) != 0 {
				t.Errorf("不应升级: escCalls = %+v", f.escCalls)
			}
			// 主链路不受影响:verdict 恒为规则判定
			if out.Tasks[0].Verdict != "TEST_FAILED" {
				t.Errorf("verdict = %s, want TEST_FAILED", out.Tasks[0].Verdict)
			}
		})
	}
}

// TestEscalateFailureKeepsMainFlow:bridge/活动失败(故障注入)只记日志——
// verdict、通知、hermes 分析全部不受影响(§7:升级失败不阻断、不重试)。
func TestEscalateFailureKeepsMainFlow(t *testing.T) {
	f := &fakeActs{
		specs:       []TestSpec{spec1()},
		matchedSigs: []string{"dsp_unavailable"},
		gate:        EscalationGateResponse{Enabled: true, MinConfidence: 0.7},
		analysis:    &hermesclient.Analysis{AnalysisVersion: 1, Summary: "s", Confidence: 0.9},
		escErr:      errBoom,
	}
	in := inputVariant()
	env := newEnvID(t, f, in.WorkflowID())
	tid := taskIDFor(in.WorkflowID(), "a1")
	sig := TaskResultSignal{
		TaskID: tid, Status: "COMPLETED", ExitCode: 0,
		SignaturesHit: []string{"dsp_unavailable"},
	}
	seedResult(f, sig)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalTaskResult, sig)
	}, 10*time.Second)

	env.ExecuteWorkflow(DeviceTestWorkflow, in)
	var out DeviceTestOutput
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	if out.Tasks[0].Verdict != "TEST_FAILED" || out.Tasks[0].Category != "DELEGATE" {
		t.Errorf("summary = %+v, want TEST_FAILED/DELEGATE(升级失败不改判定)", out.Tasks[0])
	}
	if out.Tasks[0].Analysis == nil {
		t.Error("升级失败不得影响 hermes 分析结论")
	}
	if len(f.notifyCards) != 1 {
		t.Errorf("通知卡片应发送 1 条, got %d", len(f.notifyCards))
	}
	if len(f.notifyCards) == 1 {
		card := f.notifyCards[0]
		if card.Card.Header.Template != "red" {
			t.Errorf("卡片 header = %s, want red(DELEGATE 失败)", card.Card.Header.Template)
		}
		// 降级文本应与旧逻辑一致(包含 TEST_FAILED)
		if !strings.Contains(card.FallbackText, "TEST_FAILED") {
			t.Errorf("降级文本不含 TEST_FAILED: %s", card.FallbackText)
		}
	}
}

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
			card := buildNotificationCard(DeviceTestInput{Project: "p"}, out, "")
			if card.Header.Template != tc.want {
				t.Errorf("template = %q, want %q", card.Header.Template, tc.want)
			}
		})
	}
}

// out.Tasks 为空时,正文必须是与纯文本同款的提示,而不是什么都不放。
func TestBuildNotificationCardEmptyTasks(t *testing.T) {
	card := buildNotificationCard(DeviceTestInput{Project: "p"}, &DeviceTestOutput{}, "")
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
	"element_style": true, "display": true, "list_type": true,
}

// walkCard 按 NotificationCard/CardConfig/CardHeader/CardElement/CardText
// 五种 DTO 节点逐层校验允许键、必需键、值类型与 div/hr 的 Text 配对。
// 只做全局九键白名单不够:它会放过合法 key 出现在错误节点、标量 text,
// 以及缺 tag/content 的 CardText。
func walkCard(t *testing.T, v any) []string {
	t.Helper()
	var bad []string

	checkObject := func(node any, path, kind string, allowed map[string]bool, required ...string) (map[string]any, bool) {
		obj, ok := node.(map[string]any)
		if !ok {
			bad = append(bad, fmt.Sprintf("%s: %s 必须是 object, got %T", path, kind, node))
			return nil, false
		}
		for _, key := range required {
			if _, ok := obj[key]; !ok {
				bad = append(bad, fmt.Sprintf("%s: %s 缺必需 key %q", path, kind, key))
			}
		}
		for key := range obj {
			switch {
			case !allowedCardKeys[key]:
				bad = append(bad, fmt.Sprintf("%s.%s: 集合外 key %q", path, key, key))
			case !allowed[key]:
				bad = append(bad, fmt.Sprintf("%s.%s: key %q 不属于 %s", path, key, key, kind))
			}
		}
		return obj, true
	}

	textKeys := map[string]bool{"tag": true, "content": true}
	validateText := func(node any, path string) {
		text, ok := checkObject(node, path, "CardText", textKeys, "tag", "content")
		if !ok {
			return
		}
		tag, ok := text["tag"].(string)
		if !ok {
			bad = append(bad, fmt.Sprintf("%s.tag: 必须是 string, got %T", path, text["tag"]))
		} else if tag != "plain_text" && tag != "lark_md" {
			// 2026-08-06 修订:结构化字段与转义后的动态字段允许 lark_md
			bad = append(bad, fmt.Sprintf("%s.tag: got %q,want plain_text|lark_md", path, tag))
		}
		content, stringOK := text["content"].(string)
		if !stringOK {
			bad = append(bad, fmt.Sprintf("%s.content: 必须是 string, got %T", path, text["content"]))
			return
		}
		// 2026-08-06 修订:lark_md 文本若含未转义的 <at / <a / <font 之外的
		// 尖括号标签,视为注入风险(动态文本必须先 escapeCardText)。
		// 结构化字段允许的 <font color='green'> 等飞书标签是唯一例外。
		if tag == "lark_md" {
			if strings.Contains(content, "<at") {
				bad = append(bad, fmt.Sprintf("%s.content: lark_md 含未转义 <at>(注入风险)", path))
			}
		}
	}
	rootKeys := map[string]bool{"config": true, "header": true, "elements": true}
	root, ok := checkObject(v, "$", "NotificationCard", rootKeys, "config", "header", "elements")
	if !ok {
		return bad
	}

	configKeys := map[string]bool{"wide_screen_mode": true}
	if config, ok := checkObject(root["config"], "$.config", "CardConfig", configKeys, "wide_screen_mode"); ok {
		if _, ok := config["wide_screen_mode"].(bool); !ok {
			bad = append(bad, fmt.Sprintf("$.config.wide_screen_mode: 必须是 bool, got %T",
				config["wide_screen_mode"]))
		}
	}

	headerKeys := map[string]bool{"title": true, "template": true}
	if header, ok := checkObject(root["header"], "$.header", "CardHeader", headerKeys, "title", "template"); ok {
		validateText(header["title"], "$.header.title")
		template, stringOK := header["template"].(string)
		if !stringOK {
			bad = append(bad, fmt.Sprintf("$.header.template: 必须是 string, got %T", header["template"]))
		} else if template != "green" && template != "red" && template != "orange" {
			bad = append(bad, fmt.Sprintf("$.header.template: got %q,want green|red|orange", template))
		}
	}

	elements, ok := root["elements"].([]any)
	if !ok {
		bad = append(bad, fmt.Sprintf("$.elements: 必须是 array, got %T", root["elements"]))
		return bad
	}
	elementKeys := map[string]bool{"tag": true, "text": true, "content": true, "element_style": true}
	for i, node := range elements {
		path := fmt.Sprintf("$.elements[%d]", i)
		element, ok := checkObject(node, path, "CardElement", elementKeys, "tag")
		if !ok {
			continue
		}
		tag, stringOK := element["tag"].(string)
		if !stringOK {
			bad = append(bad, fmt.Sprintf("%s.tag: 必须是 string, got %T", path, element["tag"]))
			continue
		}
		switch tag {
		case "div":
			text, exists := element["text"]
			if !exists || text == nil {
				bad = append(bad, fmt.Sprintf("%s: div 的 text 必须非 nil", path))
				continue
			}
			validateText(text, path+".text")
		case "markdown":
			// 2026-08-06:飞书 markdown 元素——content 必填字符串(多行,\n 分隔);
			// element_style 可选,display=list 时渲染无序/有序列表。
			content, cok := element["content"].(string)
			if !cok {
				bad = append(bad, fmt.Sprintf("%s: markdown 的 content 必须是 string, got %T", path, element["content"]))
			}
			if strings.Contains(content, "<at") {
				bad = append(bad, fmt.Sprintf("%s.content: markdown 含未转义 <at>(注入风险)", path))
			}
			if es, exists := element["element_style"]; exists && es != nil {
				styleKeys := map[string]bool{"display": true, "list_type": true}
				style, sok := checkObject(es, path+".element_style", "CardElStyle", styleKeys, "display")
				if !sok {
					continue
				}
				display, dok := style["display"].(string)
				if !dok {
					bad = append(bad, fmt.Sprintf("%s.element_style.display: 必须是 string, got %T", path, style["display"]))
					continue
				}
				if display != "normal" && display != "list" {
					bad = append(bad, fmt.Sprintf("%s.element_style.display: got %q,want normal|list", path, display))
				}
				if lt, exists := style["list_type"]; exists && lt != nil {
					ltv, ltok := lt.(string)
					if !ltok || (ltv != "bullet" && ltv != "ordered") {
						bad = append(bad, fmt.Sprintf("%s.element_style.list_type: got %v,want bullet|ordered", path, lt))
					}
					if display != "list" {
						bad = append(bad, fmt.Sprintf("%s.element_style: display=normal 时不得带 list_type", path))
					}
				}
			}
		case "hr":
			if text, exists := element["text"]; exists && text != nil {
				bad = append(bad, fmt.Sprintf("%s: hr 的 text 必须为 nil", path))
			}
		default:
			bad = append(bad, fmt.Sprintf("%s.tag: got %q,want div|markdown|hr", path, tag))
		}
	}
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
	card := buildNotificationCard(sampleInput(), sampleOutput(), "")
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
		{"带 actions", `{"config":{"wide_screen_mode":true},"header":{"title":{"tag":"plain_text","content":"h"},"template":"green"},"elements":[{"tag":"div","text":{"tag":"plain_text","content":"x"}}],"actions":[]}`},
		{"带 behaviors", `{"config":{"wide_screen_mode":true},"header":{"title":{"tag":"plain_text","content":"h"},"template":"green"},"elements":[{"tag":"div","text":{"tag":"plain_text","content":"x"},"behaviors":[]}]}`},
		{"div 缺 text", `{"config":{"wide_screen_mode":true},"header":{"title":{"tag":"plain_text","content":"h"},"template":"green"},"elements":[{"tag":"div"}]}`},
		{"hr 带 text", `{"config":{"wide_screen_mode":true},"header":{"title":{"tag":"plain_text","content":"h"},"template":"green"},"elements":[{"tag":"hr","text":{"tag":"plain_text","content":"x"}}]}`},
		{"text 是标量", `{"config":{"wide_screen_mode":true},"header":{"title":{"tag":"plain_text","content":"h"},"template":"green"},"elements":[{"tag":"div","text":"x"}]}`},
		{"text 缺 tag", `{"config":{"wide_screen_mode":true},"header":{"title":{"tag":"plain_text","content":"h"},"template":"green"},"elements":[{"tag":"div","text":{"content":"x"}}]}`},
		{"text 缺 content", `{"config":{"wide_screen_mode":true},"header":{"title":{"tag":"plain_text","content":"h"},"template":"green"},"elements":[{"tag":"div","text":{"tag":"plain_text"}}]}`},
		{"合法 key 放错节点", `{"config":{"wide_screen_mode":true},"header":{"title":{"tag":"plain_text","content":"h"},"template":"green"},"elements":[{"tag":"div","text":{"tag":"plain_text","content":"x"},"template":"red"}]}`},
		{"动态文本未转义 lark_md", `{"config":{"wide_screen_mode":true},"header":{"title":{"tag":"plain_text","content":"h"},"template":"green"},"elements":[{"tag":"div","text":{"tag":"lark_md","content":"<at user_id=\"all\">注入</at>"}}]}`},
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

	card := buildNotificationCard(in, out, "")

	if want := "[hermes-devops] algo-super-sdk g9da3b9d9 p56 (v1.4.2)"; card.Header.Title.Content != want {
		t.Errorf("header content = %q, want %q", card.Header.Title.Content, want)
	}
	if card.Header.Title.Tag != "plain_text" {
		t.Errorf("header tag = %q, want plain_text", card.Header.Title.Tag)
	}
	if card.Header.Template != "red" { // INFRA 不在场,存在 TEST_FAILED → red
		t.Errorf("template = %q, want red", card.Header.Template)
	}

	txt := func(c string) *CardText { return &CardText{Tag: "lark_md", Content: c} }
	md := func(c string) *CardText { return &CardText{Tag: "lark_md", Content: c} }
	want := []CardElement{
		{Tag: "div", Text: md("**aarch64_Android_SNPE_2.21**  **PASSED** <font color='green'>✅</font>")},
		{Tag: "div", Text: md("412.3s · cases **38/38** · attempt 1")},
		{Tag: "hr"},
		{Tag: "div", Text: md("**aarch64_Android_SNPE_1.68**  **TEST_FAILED** **(CODE)** <font color='red'>❌</font>")},
		{Tag: "div", Text: md("380.1s · cases **35/38** · attempt 2")}, // %.1f;passed=38-3
		{Tag: "div", Text: md("three cases crashed")},
		{Tag: "div", Text: md("hermes: DSP 初始化崩溃")},
		{Tag: "hr"},
		{Tag: "div", Text: md("**aarch64_Linux_RKNN_2.3.2**  **SKIPPED**")}, // SKIPPED 无 category
		{Tag: "div", Text: md("fleet 无匹配设备")},                               // 无指标行:CasesTotal=0 且 SKIPPED 省 attempt
	}
	_ = txt
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
	card := buildNotificationCard(DeviceTestInput{Project: "p"}, out, "")
	want := []CardElement{
		{Tag: "div", Text: &CardText{Tag: "lark_md", Content: "**v**  **TEST_FAILED** <font color='red'>❌</font>"}},
		{Tag: "div", Text: &CardText{Tag: "lark_md", Content: "attempt 3"}},
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
		{"Category 为空", "TEST_FAILED", "", "**v**  **TEST_FAILED** <font color='red'>❌</font>"},
		{"PASSED 不显示 category", "PASSED", "CODE", "**v**  **PASSED** <font color='green'>✅</font>"},
		{"非 PASSED 且有 category", "TEST_FAILED", "CODE", "**v**  **TEST_FAILED** **(CODE)** <font color='red'>❌</font>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := &DeviceTestOutput{Tasks: []TaskSummary{
				{Variant: "v", Verdict: tc.verdict, Category: tc.cat, Attempt: 1},
			}}
			card := buildNotificationCard(DeviceTestInput{Project: "p"}, out, "")
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
	card := buildNotificationCard(DeviceTestInput{Project: "p"}, out, "")
	if len(card.Elements) != 1 {
		t.Fatalf("Reason/Summary 均空时应只有主行,got %d\n%s",
			len(card.Elements), dumpElements(card.Elements))
	}

	out2 := &DeviceTestOutput{Tasks: []TaskSummary{
		{Variant: "v", Verdict: "SKIPPED",
			Analysis: &hermesclient.Analysis{Summary: ""}},
	}}
	card2 := buildNotificationCard(DeviceTestInput{Project: "p"}, out2, "")
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
	card := buildNotificationCard(DeviceTestInput{Project: "p"}, out, "")
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

// ---- 渲染安全(2026-08-06 修订:lark_md + 转义) ----

// TestEscapeCardText:动态不可信文本(Reason/Hermes Summary)进 lark_md 前必须
// 转义,`<at>`、链接、标签语法退化为字面量;粗体/换行等安全语法保留。
func TestEscapeCardText(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"注入 at 提及", `<at user_id="all">所有人</at>`, "&lt;at user_id=\"all\"&gt;所有人&lt;/at&gt;"},
		// 2026-08-08 Review P2:lark_md 会渲染 [text](url) 为可点击链接,
		// 方括号转全角,链接语法被破坏 → 退化为纯文本。
		{"注入链接", "[点我](https://evil.example)", "［点我］(https://evil.example)"},
		{"普通方括号保留语义", "[INFO] 构建成功", "［INFO］ 构建成功"},
		{"尖括号标签", "a < b && c > d", "a &lt; b &amp;&amp; c &gt; d"},
		{"安全语法保留", "**粗体** 和 `code`", "**粗体** 和 `code`"},
		{"中文与换行", "行一\n行二", "行一\n行二"},
		{"纯文本原样", "normal text", "normal text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := escapeCardText(tc.in); got != tc.want {
				t.Errorf("escapeCardText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCardEscapesDynamicText:卡片中来自 Reason / Hermes Summary 的动态文本
// 必须被转义(lark_md 渲染安全);结构化字段(verdict/category)不受影响。
func TestCardEscapesDynamicText(t *testing.T) {
	out := &DeviceTestOutput{Tasks: []TaskSummary{
		{Variant: "v", Verdict: "TEST_FAILED", Category: "CODE", Attempt: 1,
			Reason:   `<at user_id="all">注入</at> 与 [链接](https://evil)`,
			Analysis: &hermesclient.Analysis{Summary: `<at user_id="all">模型注入</at>`}},
	}}
	card := buildNotificationCard(DeviceTestInput{Project: "p"}, out, "")
	for i, e := range card.Elements {
		if e.Text == nil || e.Text.Tag != "lark_md" {
			continue
		}
		if strings.Contains(e.Text.Content, "<at") {
			t.Errorf("elements[%d] 含未转义 <at>: %q", i, e.Text.Content)
		}
	}
	// 转义后的字面量必须出现(证明转义确实发生了)
	all := allContent(card)
	for _, want := range []string{"&lt;at", "&gt;", "https://evil"} {
		if !strings.Contains(all, want) {
			t.Errorf("卡片内容缺转义产物 %q:\n%s", want, all)
		}
	}
}

// allContent 把卡片所有 CardElement.Text.Content(以及 header 标题)拼成一个串,
// 供裁剪/安全类测试用 strings.Contains 定位片段,不关心具体在哪一行。
func allContent(card NotificationCard) string {
	var b strings.Builder
	b.WriteString(card.Header.Title.Content)
	for _, e := range card.Elements {
		if e.Text != nil {
			b.WriteString(e.Text.Content)
		}
	}
	return b.String()
}

// TestBuildNotificationCardTruncatesReason:超长 Reason 截断后带省略标记,
// 且不超过 cardReasonSummaryLimit rune;多行拆分后仍保持该上限(逐行累积)。
func TestBuildNotificationCardTruncatesReason(t *testing.T) {
	long := strings.Repeat("a", 600)
	els := cardReasonLines(long)
	var reason strings.Builder
	for _, e := range els {
		reason.WriteString(e.Text.Content)
	}
	got := reason.String()
	if got == long {
		t.Fatal("超长 Reason 未被截断")
	}
	if utf8.RuneCountInString(got) > cardReasonSummaryLimit {
		t.Errorf("截断后仍超过 %d rune: %d", cardReasonSummaryLimit, utf8.RuneCountInString(got))
	}
	if strings.Contains(got, long) {
		t.Error("截断后仍包含完整原文")
	}
	if !strings.HasSuffix(got, cardTruncationMarker) {
		t.Errorf("截断后应带省略标记 %q,got %q", cardTruncationMarker, got)
	}
	// 边界:恰好等于上限不截断,上限+1 必须截断到恰好上限个 rune
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
	card := buildNotificationCard(DeviceTestInput{Project: "p"}, out, "")
	// 定位 hermes 行(最后一个非空文本,reason 为空时即它)
	hermes := ""
	for _, e := range card.Elements {
		if e.Text != nil {
			hermes = e.Text.Content
		}
	}
	if strings.Contains(hermes, long) {
		t.Fatal("超长 Summary 未被截断")
	}
	content := strings.TrimPrefix(hermes, "hermes: ")
	if utf8.RuneCountInString(content) > cardReasonSummaryLimit {
		t.Errorf("截断后仍超过 %d rune: %d", cardReasonSummaryLimit, utf8.RuneCountInString(content))
	}
	if !strings.HasSuffix(content, cardTruncationMarker) {
		t.Errorf("截断后应带省略标记 %q,got %q", cardTruncationMarker, content)
	}
}

// TestBuildNotificationCardTruncatesChineseValidUTF8:纯中文超长文本截断后
// 必须是合法 UTF-8(不能切出半个字符)。
func TestBuildNotificationCardTruncatesChineseValidUTF8(t *testing.T) {
	els := cardReasonLines(strings.Repeat("甲", 600))
	var reason strings.Builder
	for _, e := range els {
		reason.WriteString(e.Text.Content)
	}
	got := reason.String()
	if !utf8.ValidString(got) {
		t.Error("纯中文 Reason 截断后不是合法 UTF-8")
	}
	if utf8.RuneCountInString(got) > cardReasonSummaryLimit {
		t.Errorf("截断后仍超过 %d rune", cardReasonSummaryLimit)
	}
}

// TestFormatMetricsCard:卡片指标每行一个,指标名加粗 + 数值等宽;
// 键排序确定性,`_inference_ms_avg` 后缀剥掉显示 ms。
func TestFormatMetricsCard(t *testing.T) {
	got := formatMetricsCard(map[string]float64{
		"gesture_test.inference_ms_avg":          16.7,
		"detect_face_attr_test.inference_ms_avg": 46.4,
		"seg_crowd_test.inference_ms_avg":        18.0,
	})
	want := "detect_face_attr_test  **46.4ms**\n" +
		"gesture_test  **16.7ms**\n" +
		"seg_crowd_test  **18.0ms**"
	if got != want {
		t.Errorf("formatMetricsCard = %q, want %q", got, want)
	}
	// 非推理指标:保留原键,数值 3 位有效数字
	got2 := formatMetricsCard(map[string]float64{"peak_rss_mb": 214.5})
	if got2 != "peak_rss_mb  **214**" {
		t.Errorf("formatMetricsCard 非推理指标 = %q", got2)
	}
}

// TestCardReasonLinesListAndParagraph:原因按行拆分——`- ` 列表项与普通段落
// 各自独立 div;空行跳过。
func TestCardReasonLinesListAndParagraph(t *testing.T) {
	reason := "无可用设备:测试需求;\n匹配设备:\n- 513cd3de 当前离线\n在线设备:\n- a 是 MTK\n\n接入即可"
	els := cardReasonLines(reason)
	// 期望:非列表行 → lark_md div;连续列表行 → 单个 markdown bullet 元素,
	// content **保留 "- " 前缀**(display=list 时飞书渲染圆点,实测去掉前缀无圆点)
	want := []struct{ tag, content string }{
		{"div", "无可用设备:测试需求;"},
		{"div", "匹配设备:"},
		{"markdown", "- 513cd3de 当前离线"},
		{"div", "在线设备:"},
		{"markdown", "- a 是 MTK"},
		{"div", "接入即可"},
	}
	if len(els) != len(want) {
		t.Fatalf("cardReasonLines 元素数 = %d, want %d: %+v", len(els), len(want), els)
	}
	for i, w := range want {
		e := els[i]
		if e.Tag != w.tag {
			t.Errorf("[%d] tag = %q, want %q", i, e.Tag, w.tag)
		}
		if w.tag == "markdown" {
			if e.Content != w.content {
				t.Errorf("[%d] content = %q, want %q", i, e.Content, w.content)
			}
			if e.ElementStyle == nil || e.ElementStyle.Display != "list" || e.ElementStyle.ListType != "bullet" {
				t.Errorf("[%d] element_style = %+v, want list/bullet", i, e.ElementStyle)
			}
		} else {
			if e.Text == nil || e.Text.Tag != "lark_md" || e.Text.Content != w.content {
				t.Errorf("[%d] text = %+v, want lark_md %q", i, e.Text, w.content)
			}
		}
	}
	// 连续列表行归并进同一个 markdown 元素,保留各行的 "- " 前缀
	els2 := cardReasonLines("列表:\n- 甲\n- 乙\n- 丙\n结尾")
	if len(els2) != 3 || els2[1].Tag != "markdown" ||
		els2[1].Content != "- 甲\n- 乙\n- 丙" {
		t.Errorf("连续列表行未归并/前缀丢失: %+v", els2)
	}
}
