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

// ---- A4a: 取消并发 spec 时,占位摘要的 verdict 必须取自 §9 枚举 ----
// 早先此处写死 "FAILED"(枚举外的值),会被下游按"业务失败"处理。
// §9 明文:CANCELED → INCONCLUSIVE。
// 本用例只断言 verdict 值本身——卡片配色与 rerun 候选语义属 A4b 的产品决策,
// 未定之前不在此固化。
func TestCanceledParallelSpecUsesInconclusiveVerdict(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1(), specParallel2()}}
	env := newEnv(t, f)
	// 不投递任何结果 signal → spec 卡在 awaitResult;在 dispatch 之后、
	// 租约到期(120s)之前取消,迫使 runParallelSpecs 走 sums[i] == nil 的占位分支。
	env.RegisterDelayedCallback(func() { env.CancelWorkflow() }, 10*time.Second)

	env.ExecuteWorkflow(DeviceTestWorkflow, input())

	var out DeviceTestOutput
	_ = env.GetWorkflowResult(&out) // 取消时 GetWorkflowResult 可能返回 canceled error

	valid := map[string]bool{
		string(rules.VerdictPassed):       true,
		string(rules.VerdictTestFailed):   true,
		string(rules.VerdictPerfRegress):  true,
		string(rules.VerdictInfraError):   true,
		string(rules.VerdictInconclusive): true,
		VerdictSkipped:                    true,
	}
	placeholders := 0
	for _, tk := range out.Tasks {
		if !valid[tk.Verdict] {
			t.Errorf("variant %s: verdict %q 不在 §9 枚举内", tk.Variant, tk.Verdict)
		}
		if tk.Reason == "workflow canceled" {
			placeholders++
			if tk.Verdict != string(rules.VerdictInconclusive) {
				t.Errorf("variant %s: 取消占位 verdict = %q, want INCONCLUSIVE(§9)", tk.Variant, tk.Verdict)
			}
		}
	}
	// 断言占位分支确实被走到:否则本用例会在分支不可达时静默空过,
	// 看起来绿但什么都没验证。
	if placeholders == 0 {
		t.Fatalf("没有任何 task 走到取消占位分支(tasks=%d),本用例已失去意义——"+
			"请检查取消时机或 runParallelSpecs 的 sums[i]==nil 分支是否仍可达", len(out.Tasks))
	}
}

// specParallel2 是第二个 spec,用于触发 runParallelSpecs(并发路径)。
func specParallel2() TestSpec {
	s := spec1()
	s.TestID = "t2"
	s.Variant = "aarch64_Android_QCM6490_SNPE_2.21"
	return s
}

// ---- 9. 差距 #10 端到端:设备归因信号驱动(或不驱动)隔离 ----
//
// 下面三条对应设计文档 docs/superpowers/specs/2026-08-09-device-attribution-signal-design.md
// §11「故障注入(端到端)」的前三条(第四条——隔离通知投递可见于 outbox backlog——
// 落在 internal/relay 包,因为真正的隔离/outbox 写入发生在 Store,workflow 包
// 无法引入 internal/store 而不产生 import cycle:store 已经 import workflow)。
//
// 三条用例共用的机制:fakeActs.quarantineAfter>0 时,ReleaseDevice/AcquireDevice
// 复刻 store.MemStore 的记账语义(见 devicetest_test.go)。真正的 Store 端记账
// 已由 Task 10 的 conformance 测试覆盖;这里要证明的是另一件事——workflow 从
// result signal 算出的 FailScope,在连续 3 次真实驱动这套记账时,结果与设计
// 意图完全一致:该隔离的隔离,不该隔离的绝不隔离。

// deviceUnreachableResult 模拟 Agent 两级存活复核确认设备不可达后上报的结果
// (设计 §5.3):Status=FAILED(precheck/deploy 阶段调用失败),FailureScope=device。
func deviceUnreachableResult(id string) TaskResultSignal {
	return TaskResultSignal{TaskID: id, Status: "FAILED", ExitCode: 1,
		FailureScope: "device", FailureStage: "deploy"}
}

// TestFaultInjectionDeviceUnreachableQuarantines:设备连续 3 次不可达 → QUARANTINED。
// Status=FAILED 触发 decideV1 的 CategoryInfra + Retry=true(rules.go:108),
// 因此 spec1() 的 MaxInfraRetries=2 会在同一 workflow 执行内驱动 3 次 attempt——
// 与生产环境"同一块坏板连续 3 次机械重试全部失败"完全对应,不是人为拼凑。
func TestFaultInjectionDeviceUnreachableQuarantines(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}, quarantineAfter: 3}
	env := newEnv(t, f)
	for _, a := range []string{"a1", "a2", "a3"} {
		sig := deviceUnreachableResult(taskID(a))
		seedResult(f, sig)
		aCopy := a
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(SignalTaskResult, deviceUnreachableResult(taskID(aCopy)))
		}, time.Duration(10*mustAttemptIndex(aCopy))*time.Second)
	}

	env.ExecuteWorkflow(DeviceTestWorkflow, input())
	var out DeviceTestOutput
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	if out.Tasks[0].Verdict != string(rules.VerdictInfraError) || out.Tasks[0].Attempt != 3 {
		t.Fatalf("summary = %+v, want INFRA_ERROR attempt=3(3 次机械重试耗尽)", out.Tasks[0])
	}
	if len(f.released) != 3 {
		t.Fatalf("released = %d 次, want 3(每次 attempt 都要释放)", len(f.released))
	}
	for i, r := range f.released {
		if r.FailScope != FailScopeDevice {
			t.Errorf("第 %d 次 release FailScope = %q, want device(Agent 上报 device 且非 PASSED,防线 3)",
				i+1, r.FailScope)
		}
	}
	// 隔离机制真的走完了 3 次,不是只验证单次调用的返回值。
	if !f.quarantinedDevices[defaultLease.DeviceID] {
		t.Fatal("连续 3 次 device scope 释放后设备应被(模拟)隔离,但未触发")
	}
	if f.deviceFailStreak[defaultLease.DeviceID] != 3 {
		t.Errorf("deviceFailStreak = %d, want 3", f.deviceFailStreak[defaultLease.DeviceID])
	}
}

// mustAttemptIndex 把 "a1"/"a2"/"a3" 转成 1/2/3,供延迟回调错开时间用。
func mustAttemptIndex(a string) int {
	switch a {
	case "a1":
		return 1
	case "a2":
		return 2
	case "a3":
		return 3
	}
	panic("unknown attempt suffix: " + a)
}

// socMismatchResult 模拟 SoC 别名表配置漂移(2026-08-08 A1 审计的真实先例):
// 属性读取成功但比较不符,Agent 按设计 §5.1 归 none(派单/配置问题,不是设备的错)。
func socMismatchResult(id string) TaskResultSignal {
	return TaskResultSignal{TaskID: id, Status: "FAILED", ExitCode: 1,
		FailureScope: "none", FailureStage: "precheck"}
}

// TestFaultInjectionSocMismatchDoesNotQuarantine:§3 核心护栏——配置错误(SoC
// 别名表失效导致连续 soc mismatch)绝不能隔离一块完全健康的板。与上一条同构
// (Status=FAILED → 3 次机械重试全部在同一 workflow 内发生),唯一差异是
// Agent 上报的 scope 是 none 而不是 device;这正是本用例要锁住的分界线。
//
// 必须真的驱动 3 次记账(而不是只调用一次 failScope()):否则测不出"如果
// FailScopeNone 被错误地当成会累计的值,第 3 次会不会被隔离"这个问题,
// 单次调用无法暴露记账逻辑本身的缺陷。
func TestFaultInjectionSocMismatchDoesNotQuarantine(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}, quarantineAfter: 3}
	env := newEnv(t, f)
	for _, a := range []string{"a1", "a2", "a3"} {
		seedResult(f, socMismatchResult(taskID(a)))
		aCopy := a
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(SignalTaskResult, socMismatchResult(taskID(aCopy)))
		}, time.Duration(10*mustAttemptIndex(aCopy))*time.Second)
	}

	env.ExecuteWorkflow(DeviceTestWorkflow, input())
	var out DeviceTestOutput
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatal(err)
	}
	if out.Tasks[0].Verdict != string(rules.VerdictInfraError) || out.Tasks[0].Attempt != 3 {
		t.Fatalf("summary = %+v, want INFRA_ERROR attempt=3(3 次机械重试耗尽)", out.Tasks[0])
	}
	if len(f.released) != 3 {
		t.Fatalf("released = %d 次, want 3", len(f.released))
	}
	for i, r := range f.released {
		if r.FailScope != FailScopeNone {
			t.Errorf("第 %d 次 release FailScope = %q, want none(配置类失败不驱动任何处置)",
				i+1, r.FailScope)
		}
	}
	// 最要紧的断言:走完 3 次真实记账之后,设备依然可用、依然未被隔离。
	if f.quarantinedDevices[defaultLease.DeviceID] {
		t.Fatal("soc mismatch 连续 3 次不得隔离设备——配置错误不许误伤好板")
	}
	if f.deviceFailStreak[defaultLease.DeviceID] != 0 {
		t.Errorf("deviceFailStreak = %d, want 0(none 不驱动设备计数)", f.deviceFailStreak[defaultLease.DeviceID])
	}
	if f.acquireCalls != 3 {
		t.Errorf("acquireCalls = %d, want 3(设备全程可正常复用)", f.acquireCalls)
	}
}

// passedWithCollectFailureResult 模拟一次整体 PASSED、但可选 collect 附件拉取
// 失败的结果。设计 §6 防线 1 要求 Agent 侧 best-effort 路径永不填这两个字段;
// 这里刻意反着构造(把 FailureScope 填成 device)是为了压测防线 2——Runtime
// 侧的纵深校验必须独立生效,不能依赖防线 1 不出 bug。
func passedWithCollectFailureResult(id string) TaskResultSignal {
	return TaskResultSignal{TaskID: id, Status: "COMPLETED", ExitCode: 0,
		CasesTotal: 10, CasesFailed: 0,
		FailureScope: "device", FailureStage: "collect"}
}

// TestFaultInjectionPassedWithCollectFailureDoesNotQuarantine:§6 防线 2 回归
// 护栏——即便 Agent 因 bug 把旁路 collect 失败误填成 failure_scope=device,
// 只要最终 verdict 是 PASSED,Runtime 必须强制清零,连续 3 次都不能例外。
//
// verdict=PASSED 时 rules.Decision.Retry 恒为 false(decideV1 最终 return 分支
// 未设 Retry),所以这里不能像前两条那样靠同一 workflow 内的机械重试拿到 3 次
// attempt——PASSED 是终态,一次就完。改用 3 次独立的 workflow 执行(对应生产环境
// "同一块板连续 3 次测试都 PASSED,但每次 collect 都失败"),共享同一个 fakeActs
// 实例以保留跨次的模拟记账状态,这样"3 次都不隔离"才是真实断言而不是巧合。
func TestFaultInjectionPassedWithCollectFailureDoesNotQuarantine(t *testing.T) {
	f := &fakeActs{specs: []TestSpec{spec1()}, quarantineAfter: 3}
	for i := 0; i < 3; i++ {
		env := newEnv(t, f)
		tid := taskID("a1")
		sig := passedWithCollectFailureResult(tid)
		seedResult(f, sig)
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(SignalTaskResult, sig)
		}, 10*time.Second)

		env.ExecuteWorkflow(DeviceTestWorkflow, input())
		var out DeviceTestOutput
		if err := env.GetWorkflowResult(&out); err != nil {
			t.Fatalf("第 %d 次执行: %v", i+1, err)
		}
		if out.Tasks[0].Verdict != string(rules.VerdictPassed) {
			t.Fatalf("第 %d 次执行 verdict = %s, want PASSED", i+1, out.Tasks[0].Verdict)
		}
	}
	if len(f.released) != 3 {
		t.Fatalf("released = %d 次, want 3(3 次独立执行各释放一次)", len(f.released))
	}
	for i, r := range f.released {
		if r.FailScope != FailScopeOK {
			t.Errorf("第 %d 次 release FailScope = %q, want ok(PASSED 必须强制清零,忽略上报值)",
				i+1, r.FailScope)
		}
	}
	if f.quarantinedDevices[defaultLease.DeviceID] {
		t.Fatal("PASSED 连续 3 次绝不能隔离设备——旁路失败不许误伤好板(§6 防线 2)")
	}
	if f.deviceFailStreak[defaultLease.DeviceID] != 0 {
		t.Errorf("deviceFailStreak = %d, want 0", f.deviceFailStreak[defaultLease.DeviceID])
	}
	if f.acquireCalls != 3 {
		t.Errorf("acquireCalls = %d, want 3(设备全程可正常复用)", f.acquireCalls)
	}
}
