package workflow

import (
	"errors"
	"strings"
	"testing"
	"time"

	"hermes-devops/runtime/internal/hermesclient"
	"hermes-devops/runtime/internal/rules"
)

// ---- Phase 3 故障注入矩阵 ----
// 每项验证系统在故障下收敛到正确终态,零重复执行。

// ---- 1. 幂等:重复 signal 不触发重复副作用 ----
func TestFaultInjectionDuplicateResultSignal(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}}
	env := newEnv(t, f)
	sig := passResult(taskID("a1"))
	seedResult(f, sig)
	// 3 次相同的 signal(Relay 至少一次重投的极端情况)
	for i := 0; i < 3; i++ {
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(SignalTaskResult, sig)
		}, time.Duration(10+i*5)*time.Second)
	}

	env.ExecuteWorkflow(DeviceTestWorkflow, input())
	var out DeviceTestOutput
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	if out.Tasks[0].Verdict != "PASSED" {
		t.Fatalf("verdict = %s, want PASSED", out.Tasks[0].Verdict)
	}
	// 幂等: 只调度一次, 只 LoadResult 一次, 只 FinishTask 一次
	if len(f.dispatched) != 1 {
		t.Errorf("dispatched = %d, want 1(duplicate signals must not re-dispatch)", len(f.dispatched))
	}
	if len(f.finished) != 1 {
		t.Errorf("finished = %d, want 1(duplicate signals must not repeat finish)", len(f.finished))
	}
	if f.loadCalls == nil {
		t.Error("LoadResult was never called (signal ignored?)")
	}
	loadCount := len(f.loadCalls)
	if loadCount > 1 {
		t.Errorf("LoadResult called %d times, want ≤1(duplicate signal must not re-read)", loadCount)
	}
}

// ---- 2. 退化: CancelTask 失败不阻断 workflow ----
func TestFaultInjectionCancelFails(t *testing.T) {
	f := &fakeActs{
		specs:        []TestSpec{spec1()},
		dispatchErrs: []error{errBoom, errBoom, errBoom},
	}
	env := newEnv(t, f)
	// dispatch 三次都失败(activity 重试耗尽) → 每次 attempt 都触发 cancel + release + finish

	env.ExecuteWorkflow(DeviceTestWorkflow, input())
	var out DeviceTestOutput
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	if out.Tasks[0].Verdict != string(rules.VerdictInfraError) {
		t.Errorf("verdict = %s, want INFRA_ERROR(dispatch all failed)", out.Tasks[0].Verdict)
	}
	// 3 次 attempt,每 attempt 都 cancel + release + finish
	if len(f.canceled) < 1 {
		t.Errorf("canceled = %d, want ≥1(best-effort cancel per attempt)", len(f.canceled))
	}
	if len(f.released) < 1 {
		t.Errorf("released = %d, want ≥1(cleanup must run per attempt)", len(f.released))
	}
	// 最后一次 FinishTask 落 INFRA_ERROR
	if len(f.finished) == 0 {
		t.Fatal("finish task never called")
	}
	last := f.finished[len(f.finished)-1]
	if last.Verdict != string(rules.VerdictInfraError) {
		t.Errorf("final finish = %+v, want INFRA_ERROR", last)
	}
	// 整体 workflow 成功完成(降级而非 crash)
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Errorf("workflow error = %v, want nil(degraded, not crashed)", env.GetWorkflowError())
	}
}

// ---- 3. 退化: ReleaseDevice 失败不丢终态 ----
func TestFaultInjectionReleaseFailsTerminalStillRecorded(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}}
	env := newEnv(t, f)
	sig := passResult(taskID("a1"))
	seedResult(f, sig)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalTaskResult, sig)
	}, 10*time.Second)

	env.ExecuteWorkflow(DeviceTestWorkflow, input())
	var out DeviceTestOutput
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	// ReleaseDevice 被调用(正常路径)
	if len(f.released) != 1 {
		t.Errorf("released = %d, want 1", len(f.released))
	}
	// FinishTask 仍然被调用,终态无论如何都要落
	if len(f.finished) != 1 || f.finished[0].Verdict != string(rules.VerdictPassed) {
		t.Errorf("finished = %+v, want PASSED", f.finished)
	}
}

// ---- 4. 退化: Evidence 提取失败 → 规则引擎保底 ----
func TestFaultInjectionEvidenceExtractionFails(t *testing.T) {
	f := &fakeActs{
		specs:       []TestSpec{spec1()},
		evidenceErr: errors.New("minio unreachable"),
		analysis:    &hermesclient.Analysis{AnalysisVersion: 1, Confidence: 0.9},
	}
	env := newEnv(t, f)
	sig := TaskResultSignal{
		TaskID: taskID("a1"), Status: "COMPLETED", ExitCode: 1,
		CasesTotal: 10, CasesFailed: 2,
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
	// 规则引擎裁决不受影响(signature hit → TEST_FAILED/MODEL)
	if out.Tasks[0].Verdict != string(rules.VerdictTestFailed) ||
		out.Tasks[0].Category != string(rules.CategoryModel) {
		t.Errorf("verdict/category = %s/%s, want TEST_FAILED/MODEL(rule engine stands)",
			out.Tasks[0].Verdict, out.Tasks[0].Category)
	}
	// analysis 为空:提取失败 → 无 analysis 输入
	if out.Tasks[0].Analysis != nil {
		t.Error("analysis should be nil when evidence extraction fails")
	}
}

// ---- 5. 退化: SaveDecision 失败不阻断 workflow ----
func TestFaultInjectionSaveDecisionFails(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}}
	env := newEnv(t, f)
	sig := TaskResultSignal{
		TaskID: taskID("a1"), Status: "COMPLETED", ExitCode: 1,
		CasesTotal: 10, CasesFailed: 2,
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
	// 规则引擎裁决照常产出
	if out.Tasks[0].Verdict != string(rules.VerdictTestFailed) {
		t.Errorf("verdict = %s, want TEST_FAILED", out.Tasks[0].Verdict)
	}
	// SaveDecision for rule 被调用(rule decision always saved)
	if len(f.decisions) == 0 {
		t.Error("rule decision not saved")
	}
}

// ---- 6. 退化: EscalationGate 失败 → 升级跳过 ----
func TestFaultInjectionEscalationGateFails(t *testing.T) {
	f := &fakeActs{
		specs:       []TestSpec{spec1()},
		matchedSigs: []string{"dsp_unavailable"},
		gate:        EscalationGateResponse{Enabled: true, MinConfidence: 0.7},
		analysis:    &hermesclient.Analysis{AnalysisVersion: 1, Summary: "s", Confidence: 0.9},
	}
	env := newEnv(t, f)
	sig := TaskResultSignal{
		TaskID: taskID("a1"), Status: "COMPLETED", ExitCode: 1,
		CasesTotal: 10, CasesFailed: 1,
		SignaturesHit: []string{"dsp_unavailable"},
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
	// 主链路不受影响: verdict 照常
	if out.Tasks[0].Verdict != string(rules.VerdictTestFailed) ||
		out.Tasks[0].Category != string(rules.CategoryDelegate) {
		t.Errorf("verdict/category = %s/%s, want TEST_FAILED/DELEGATE",
			out.Tasks[0].Verdict, out.Tasks[0].Category)
	}
}

// ---- 7. 退化: 所有 attempt 耗尽, INFRA 不可重试时直接终态 ----
func TestFaultInjectionAllAttemptsExhaustedNonRetryable(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}}
	env := newEnv(t, f)
	// 不回传结果,不续租,第一次 attempt 租约过期,
	// 重试两次后(共 3 次 attempt) INFRA 终态
	env.ExecuteWorkflow(DeviceTestWorkflow, input())
	var out DeviceTestOutput
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	if out.Tasks[0].Verdict != string(rules.VerdictInfraError) {
		t.Errorf("verdict = %s, want INFRA_ERROR(lease expired, all attempts exhausted)",
			out.Tasks[0].Verdict)
	}
	if out.Tasks[0].Attempt != 3 {
		t.Errorf("attempt = %d, want 3(maxAttempts exhausted)", out.Tasks[0].Attempt)
	}
	if !strings.Contains(out.Tasks[0].Reason, "lease") {
		t.Errorf("reason = %s, want lease-related", out.Tasks[0].Reason)
	}
}

// ---- 8. 幂等: SelectTestSpecs 失败阻断 workflow ----
func TestFaultInjectionSelectSpecsFailed(t *testing.T) {
	f := &fakeActs{selectErr: errors.New("variants.yaml unreadable")}
	env := newEnv(t, f)

	env.ExecuteWorkflow(DeviceTestWorkflow, input())
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("expected workflow error for select failure")
	} else if !strings.Contains(err.Error(), "select test specs") {
		t.Errorf("error = %v, want 'select test specs'", err)
	}
	if len(f.dispatched) != 0 {
		t.Error("dispatch must not be called when SelectTestSpecs fails")
	}
}
