package workflow

import (
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"hermes-devops/runtime/internal/rules"
)

// ---- signal 契约(callbacks API → workflow) ----

const (
	SignalTaskResult    = "task-result"
	SignalTaskHeartbeat = "task-heartbeat"
)

// TaskResultSignal 是 /callbacks/v1/results 经 API 转投的终态(§8.2)。
type TaskResultSignal struct {
	TaskID        string             `json:"task_id"`
	Status        string             `json:"status"` // COMPLETED|FAILED|TIMEOUT|CANCELED
	ExitCode      int                `json:"exit_code"`
	DurationSec   float64            `json:"duration_sec"`
	CasesTotal    int                `json:"cases_total"`
	CasesFailed   int                `json:"cases_failed"`
	SignaturesHit []string           `json:"signatures_hit"`
	Metrics       map[string]float64 `json:"metrics"`
	Attachments   []Attachment       `json:"attachments"`
}

type Attachment struct {
	Name      string `json:"name"`
	ObjectKey string `json:"object_key"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
}

// TaskHeartbeat 续租(§8.2 heartbeat 即租约续期)。
type TaskHeartbeat struct {
	TaskID string `json:"task_id"`
}

// ---- 活动契约(实现在 internal/activity) ----

type DeviceSelector struct {
	SOC          []string `json:"soc"`
	Capabilities []string `json:"capabilities"`
}

// TestSpec 由 SelectTestSpecs 活动从配置(variants.yaml)派生。
type TestSpec struct {
	TestID            string                    `json:"test_id"`
	Variant           string                    `json:"variant"`
	Package           PackageRef                `json:"package"`
	Selector          DeviceSelector            `json:"selector"`
	SignatureCategory map[string]rules.Category `json:"signature_category"`
	MaxInfraRetries   int                       `json:"max_infra_retries"` // §10 缺省 2
	LeaseSeconds      int                       `json:"lease_seconds"`     // §10 缺省 120
	HardTimeoutSec    int                       `json:"hard_timeout_sec"`  // 单次 attempt 硬上限
	DeviceWaitRounds  int                       `json:"device_wait_rounds"`
	DeviceWaitSeconds int                       `json:"device_wait_seconds"`
}

type AcquireRequest struct {
	TaskID   string         `json:"task_id"`
	Selector DeviceSelector `json:"selector"`
}

// Lease 是 AcquireDevice 的结果;nil 表示当前无可用设备。
type Lease struct {
	DeviceID      string `json:"device_id"`
	Serial        string `json:"serial"`
	ClientID      string `json:"client_id"`
	ClientBaseURL string `json:"client_base_url"`
}

type TaskRow struct {
	TaskID         string `json:"task_id"`
	WorkflowID     string `json:"workflow_id"`
	TestID         string `json:"test_id"`
	Attempt        int    `json:"attempt"`
	IdempotencyKey string `json:"idempotency_key"`
	ClientID       string `json:"client_id"`
	DeviceID       string `json:"device_id"`
	Status         string `json:"status"`
}

// DispatchRequest 对应 §8.1 POST /api/v1/tasks 的派单载荷(凭据由活动实现补充)。
type DispatchRequest struct {
	TaskID         string `json:"task_id"`
	IdempotencyKey string `json:"idempotency_key"`
	Attempt        int    `json:"attempt"`
	PackageURL     string `json:"package_url"`
	PackageSHA256  string `json:"package_sha256"`
	ManifestDigest string `json:"manifest_digest"`
	DeviceSerial   string `json:"device_serial"`
	ClientBaseURL  string `json:"client_base_url"`
}

type CancelRequest struct {
	TaskID        string `json:"task_id"`
	ClientBaseURL string `json:"client_base_url"`
}

type ResultRecord struct {
	TaskID string           `json:"task_id"`
	Result TaskResultSignal `json:"result"`
}

type FinishRequest struct {
	TaskID   string `json:"task_id"`
	Status   string `json:"status"`
	Verdict  string `json:"verdict"`
	Category string `json:"category"`
	Reason   string `json:"reason"`
}

type ReleaseRequest struct {
	DeviceID  string `json:"device_id"`
	TaskID    string `json:"task_id"`
	InfraFail bool   `json:"infra_fail"` // true → fail_streak+1,连续 3 次隔离(§10)
}

// ---- 输出 ----

type TaskSummary struct {
	TestID      string       `json:"test_id"`
	Variant     string       `json:"variant"`
	TaskID      string       `json:"task_id"`
	Attempt     int          `json:"attempt"` // 最终 attempt 序号
	Verdict     string       `json:"verdict"`
	Category    string       `json:"category"`
	Reason      string       `json:"reason"`
	Attachments []Attachment `json:"attachments,omitempty"`
	retryable   bool
}

type DeviceTestOutput struct {
	Tasks []TaskSummary `json:"tasks"`
}

// ---- workflow 本体 ----

// DeviceTestWorkflow 主干(§12.6):
// SelectTestSpecs → 逐测试 [acquire_device → dispatch → await_result(signal,
// 心跳续租,过期按 on_infra_error 机械重试 ≤2)→ 规则引擎判 verdict → release_device]
// → 飞书纯文本通知。规则引擎为纯函数,直接在 workflow 内调用(确定性)。
func DeviceTestWorkflow(ctx workflow.Context, in DeviceTestInput) (*DeviceTestOutput, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    2 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumAttempts:    3,
		},
	})

	var specs []TestSpec
	if err := workflow.ExecuteActivity(ctx, "SelectTestSpecs", in).Get(ctx, &specs); err != nil {
		return nil, fmt.Errorf("select test specs: %w", err)
	}

	out := &DeviceTestOutput{}
	resultCh := workflow.GetSignalChannel(ctx, SignalTaskResult)
	hbCh := workflow.GetSignalChannel(ctx, SignalTaskHeartbeat)

	for _, spec := range specs {
		out.Tasks = append(out.Tasks, runTest(ctx, spec, resultCh, hbCh))
	}

	text := buildNotification(in, out)
	if err := workflow.ExecuteActivity(ctx, "Notify", text).Get(ctx, nil); err != nil {
		workflow.GetLogger(ctx).Error("notify failed", "error", err)
	}
	return out, nil
}

// runTest 执行一个测试(含 INFRA 机械重试,§10 缺省 ≤2 次)。
func runTest(ctx workflow.Context, spec TestSpec, resultCh, hbCh workflow.ReceiveChannel) TaskSummary {
	maxAttempts := spec.MaxInfraRetries + 1
	var sum TaskSummary
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		sum = runAttempt(ctx, spec, attempt, resultCh, hbCh)
		if !sum.retryable || attempt == maxAttempts {
			break
		}
		workflow.GetLogger(ctx).Info("infra failure, mechanical retry",
			"test", spec.TestID, "attempt", attempt, "reason", sum.Reason)
	}
	return sum
}

func runAttempt(ctx workflow.Context, spec TestSpec, attempt int, resultCh, hbCh workflow.ReceiveChannel) TaskSummary {
	wfID := workflow.GetInfo(ctx).WorkflowExecution.ID
	// 幂等键 = {workflow_id}:{test_id}:{attempt}(§12.6),task_id 同值
	taskID := fmt.Sprintf("%s:%s:a%d", wfID, spec.TestID, attempt)
	sum := TaskSummary{TestID: spec.TestID, Variant: spec.Variant, TaskID: taskID, Attempt: attempt}
	infra := func(reason string, retryable bool) TaskSummary {
		d := rules.Decide(rules.Input{Status: "FAILED", InfraReason: reason})
		sum.Verdict, sum.Category, sum.Reason = string(d.Verdict), string(d.Category), d.Reason
		sum.retryable = retryable && d.Retry
		return sum
	}
	// 清理活动用 disconnected ctx:workflow 被取消也要释放设备/落终态
	dctx, _ := workflow.NewDisconnectedContext(ctx)

	// ---- acquire_device(设备忙则有限等待) ----
	var lease *Lease
	for round := 0; ; round++ {
		if err := workflow.ExecuteActivity(ctx, "AcquireDevice",
			AcquireRequest{TaskID: taskID, Selector: spec.Selector}).Get(ctx, &lease); err != nil {
			return infra("acquire device: "+err.Error(), true)
		}
		if lease != nil {
			break
		}
		if round >= spec.DeviceWaitRounds {
			return infra("no device available", false)
		}
		if err := workflow.Sleep(ctx, time.Duration(spec.DeviceWaitSeconds)*time.Second); err != nil {
			return infra("canceled while waiting for device", false)
		}
	}
	released := false
	release := func(infraFail bool) {
		if released {
			return
		}
		released = true
		_ = workflow.ExecuteActivity(dctx, "ReleaseDevice",
			ReleaseRequest{DeviceID: lease.DeviceID, TaskID: taskID, InfraFail: infraFail}).Get(dctx, nil)
	}

	// ---- 登记任务 + dispatch ----
	if err := workflow.ExecuteActivity(ctx, "CreateTask", TaskRow{
		TaskID: taskID, WorkflowID: wfID, TestID: spec.TestID, Attempt: attempt,
		IdempotencyKey: taskID, ClientID: lease.ClientID, DeviceID: lease.DeviceID,
		Status: "DISPATCHING",
	}).Get(ctx, nil); err != nil {
		release(false)
		return infra("create task: "+err.Error(), true)
	}
	finish := func(status, verdict, category, reason string) {
		_ = workflow.ExecuteActivity(dctx, "FinishTask", FinishRequest{
			TaskID: taskID, Status: status, Verdict: verdict, Category: category, Reason: reason,
		}).Get(dctx, nil)
	}
	if err := workflow.ExecuteActivity(ctx, "Dispatch", DispatchRequest{
		TaskID: taskID, IdempotencyKey: taskID, Attempt: attempt,
		PackageURL: spec.Package.URL, PackageSHA256: spec.Package.SHA256,
		ManifestDigest: spec.Package.ManifestDigest,
		DeviceSerial:   lease.Serial, ClientBaseURL: lease.ClientBaseURL,
	}).Get(ctx, nil); err != nil {
		finish("FAILED", string(rules.VerdictInfraError), string(rules.CategoryInfra), "dispatch failed")
		release(true)
		return infra("dispatch: "+err.Error(), true)
	}

	// ---- await_result:signal 驱动,心跳续租,禁止轮询(§14) ----
	res, infraReason := awaitResult(ctx, taskID, spec, resultCh, hbCh)
	if infraReason != "" {
		_ = workflow.ExecuteActivity(dctx, "CancelTask",
			CancelRequest{TaskID: taskID, ClientBaseURL: lease.ClientBaseURL}).Get(dctx, nil)
		finish("FAILED", string(rules.VerdictInfraError), string(rules.CategoryInfra), infraReason)
		release(true)
		return infra(infraReason, true)
	}

	// ---- 落结果 + 规则引擎判 verdict ----
	if err := workflow.ExecuteActivity(ctx, "RecordResult",
		ResultRecord{TaskID: taskID, Result: *res}).Get(ctx, nil); err != nil {
		workflow.GetLogger(ctx).Error("record result failed", "error", err)
	}
	d := rules.Decide(rules.Input{
		Status: res.Status, ExitCode: res.ExitCode, CasesFailed: res.CasesFailed,
		SignaturesHit: res.SignaturesHit, SignatureCategory: spec.SignatureCategory,
	})
	sum.Verdict, sum.Category, sum.Reason = string(d.Verdict), string(d.Category), d.Reason
	sum.Attachments = res.Attachments
	sum.retryable = d.Retry
	finish(res.Status, sum.Verdict, sum.Category, sum.Reason)
	release(d.Category == rules.CategoryInfra)
	return sum
}

// awaitResult 阻塞等待本 task 的结果 signal;心跳 signal 续租;
// 租约过期或硬超时返回 infraReason。
func awaitResult(ctx workflow.Context, taskID string, spec TestSpec, resultCh, hbCh workflow.ReceiveChannel) (*TaskResultSignal, string) {
	lease := time.Duration(spec.LeaseSeconds) * time.Second
	hardDeadline := workflow.Now(ctx).Add(time.Duration(spec.HardTimeoutSec) * time.Second)
	leaseExpiry := workflow.Now(ctx).Add(lease)

	var res *TaskResultSignal
	for {
		now := workflow.Now(ctx)
		if now.After(hardDeadline) || now.Equal(hardDeadline) {
			return nil, "hard deadline exceeded"
		}
		if now.After(leaseExpiry) || now.Equal(leaseExpiry) {
			return nil, "lease expired (no heartbeat)"
		}
		next := leaseExpiry
		if hardDeadline.Before(next) {
			next = hardDeadline
		}
		timerCtx, cancelTimer := workflow.WithCancel(ctx)
		timer := workflow.NewTimer(timerCtx, next.Sub(now))

		sel := workflow.NewSelector(ctx)
		sel.AddReceive(resultCh, func(c workflow.ReceiveChannel, _ bool) {
			var cand TaskResultSignal
			c.Receive(ctx, &cand)
			if cand.TaskID == taskID {
				res = &cand
			} // 其他 task(含历史 attempt)的迟到结果:忽略
		})
		sel.AddReceive(hbCh, func(c workflow.ReceiveChannel, _ bool) {
			var hb TaskHeartbeat
			c.Receive(ctx, &hb)
			if hb.TaskID == taskID {
				leaseExpiry = workflow.Now(ctx).Add(lease) // 续租(§8.2)
			}
		})
		sel.AddFuture(timer, func(workflow.Future) {}) // 唤醒后由循环头重新判定
		sel.Select(ctx)
		cancelTimer()

		if res != nil {
			return res, ""
		}
		if ctx.Err() != nil {
			return nil, "workflow canceled"
		}
	}
}

// buildNotification 生成飞书纯文本(Phase 1,§12.6;交互卡片属 Phase 2)。
func buildNotification(in DeviceTestInput, out *DeviceTestOutput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[hermes-devops] %s g%s p%d (v%s)\n", in.Project, in.Commit, in.PipelineID, in.Version)
	if len(out.Tasks) == 0 {
		b.WriteString("无可测变体(Android 包缺失或未配置)")
		return b.String()
	}
	for _, tk := range out.Tasks {
		fmt.Fprintf(&b, "- %s: %s", tk.Variant, tk.Verdict)
		if tk.Category != "" && tk.Verdict != string(rules.VerdictPassed) {
			fmt.Fprintf(&b, "(%s)", tk.Category)
		}
		fmt.Fprintf(&b, " attempt=%d %s\n", tk.Attempt, tk.Reason)
		for _, att := range tk.Attachments {
			fmt.Fprintf(&b, "  · %s → %s\n", att.Name, att.ObjectKey)
		}
	}
	return b.String()
}
