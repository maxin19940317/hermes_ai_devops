package workflow

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"

	"hermes-devops/runtime/internal/rules"
)

var errBoom = errors.New("client unreachable")

// fakeActs 以真实方法签名注册,记录全部调用(线程安全:活动并发执行)。
type fakeActs struct {
	mu           sync.Mutex
	specs        []TestSpec
	acquires     []*Lease // 每次 AcquireDevice 依次弹出;耗尽后返回 defaultLease
	dispatchErrs []error  // 依次弹出;耗尽后 nil

	acquireCalls  int
	created       []TaskRow
	dispatched    []DispatchRequest
	canceled      []CancelRequest
	recorded      []ResultRecord
	finished      []FinishRequest
	released      []ReleaseRequest
	notifications []string
}

var defaultLease = &Lease{DeviceID: "dev1", Serial: "513cd3de", ClientID: "c1", ClientBaseURL: "https://client:8443"}

func (f *fakeActs) SelectTestSpecs(_ context.Context, _ DeviceTestInput) ([]TestSpec, error) {
	return f.specs, nil
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
func (f *fakeActs) RecordResult(_ context.Context, r ResultRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded = append(f.recorded, r)
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

const wfID = "device-test-grp/p-gabcd1234-p42"

func spec1() TestSpec {
	return TestSpec{
		TestID:            "t1",
		Variant:           "aarch64_Android_SNPE_2.21",
		Package:           PackageRef{URL: "https://reg/pkg.tar.gz", SHA256: "aa", ManifestDigest: "bb"},
		Selector:          DeviceSelector{SOC: []string{"QCM6125"}},
		SignatureCategory: map[string]rules.Category{"cpu_fallback": "MODEL"},
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
	return TaskResultSignal{TaskID: id, Status: "COMPLETED", ExitCode: 0, CasesTotal: 10,
		Attachments: []Attachment{{Name: "logcat.txt", ObjectKey: "runs/x/logcat.txt"}}}
}

func TestHappyPathPassed(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}}
	env := newEnv(t, f)
	// 30s 时回传结果(期间无需心跳,未超 120s 租约)
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
	if len(f.recorded) != 1 || len(f.finished) != 1 || f.finished[0].Verdict != "PASSED" {
		t.Errorf("recorded=%d finished=%+v", len(f.recorded), f.finished)
	}
	if len(f.notifications) != 1 ||
		!strings.Contains(f.notifications[0], "PASSED") ||
		!strings.Contains(f.notifications[0], "runs/x/logcat.txt") {
		t.Errorf("notification = %q(需含 verdict 与日志对象键)", f.notifications)
	}
}

func TestLeaseExpiryRetriesThenInfraError(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}}
	env := newEnv(t, f)
	// 不回传任何结果、无心跳 → 每次 attempt 120s 租约过期,机械重试 2 次后终态 INFRA_ERROR

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
	if len(f.canceled) != 3 {
		t.Errorf("canceled = %+v, 每次租约过期应尽力取消", f.canceled)
	}
	for _, r := range f.released {
		if !r.InfraFail {
			t.Errorf("release = %+v, 租约过期必须计入 fail_streak", r)
		}
	}
}

func TestHeartbeatRenewsLease(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}}
	env := newEnv(t, f)
	// 心跳在 100s/200s(每次都在上次续约后 120s 内),结果在 290s:
	// 无续租机制的话 120s 就会过期
	for _, d := range []time.Duration{100 * time.Second, 200 * time.Second} {
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(SignalTaskHeartbeat, TaskHeartbeat{TaskID: taskID("a1")})
		}, d)
	}
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalTaskResult, passResult(taskID("a1")))
	}, 290*time.Second)

	env.ExecuteWorkflow(DeviceTestWorkflow, input())
	var out DeviceTestOutput
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	if out.Tasks[0].Verdict != "PASSED" || out.Tasks[0].Attempt != 1 {
		t.Errorf("summary = %+v, 心跳续租后应 PASSED 且无重试", out.Tasks[0])
	}
}

func TestSignatureHitNoRetry(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}}
	env := newEnv(t, f)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalTaskResult, TaskResultSignal{
			TaskID: taskID("a1"), Status: "COMPLETED", ExitCode: 0,
			SignaturesHit: []string{"cpu_fallback"},
		})
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
}

func TestStaleResultSignalIgnored(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}}
	env := newEnv(t, f)
	env.RegisterDelayedCallback(func() { // 其他 task 的迟到结果:必须忽略
		env.SignalWorkflow(SignalTaskResult, passResult("some-other-task"))
	}, 5*time.Second)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalTaskResult, TaskResultSignal{
			TaskID: taskID("a1"), Status: "COMPLETED", ExitCode: 7,
		})
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

func TestDeviceBusyThenAvailable(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}, acquires: []*Lease{nil, nil}} // 前两轮无设备
	env := newEnv(t, f)
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
