package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"hermes-devops/runtime/internal/hermesclient"
	"hermes-devops/runtime/internal/rules"
)

// ---- signal 契约(callbacks API → workflow) ----

const (
	SignalTaskResult    = "task-result"
	SignalTaskHeartbeat = "task-heartbeat"
)

// VerdictSkipped 标记在 SelectTestSpecs 阶段被跳过的变体(fleet 无匹配设备/
// OS 未接入)。只出现在 TaskSummary 与通知文案中,不经过规则引擎,
// §9 的 verdict 集合不变。
const VerdictSkipped = "SKIPPED"

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

// ResultRecord 是 results 表一行;由回调服务在投 signal 前落库(SaveResult 去重),
// workflow 不再经手结果持久化。
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

// DecisionRow 是 decisions 表一行(§11):规则引擎与 LLM 的每次裁决都落表,可回放。
type DecisionRow struct {
	TaskID        string          `json:"task_id"`
	Actor         string          `json:"actor"`        // hermes|rule|human
	InputDigest   string          `json:"input_digest"` // 输入摘要(evidence sha256;rule 可为空)
	Model         string          `json:"model"`
	PromptVersion string          `json:"prompt_version"`
	Output        json.RawMessage `json:"output"` // 已是 JSON(rule Decision 或 analysis)
}

// ExtractEvidenceRequest 是 ExtractEvidence 活动的入参(§12 Phase 2)。
type ExtractEvidenceRequest struct {
	TaskID  string           `json:"task_id"`
	Variant string           `json:"variant"`
	Result  TaskResultSignal `json:"result"`
}

// ExtractEvidenceResponse 携带 evidence.json 序列化形态及其 sha256 摘要;
// 摘要在 decisions 表充当 hermes 裁决的 input_digest(§11 可回放)。
type ExtractEvidenceResponse struct {
	EvidenceJSON json.RawMessage `json:"evidence_json"`
	Digest       string          `json:"digest"`
}

// AnalyzeRequest 是 Analyze 活动的入参;RuleCategory 为规则引擎判定类别(§9),
// 供 Analyzer 参考,verdict 判定权始终在规则引擎。
type AnalyzeRequest struct {
	TaskID       string          `json:"task_id"`
	RuleCategory string          `json:"rule_category"`
	EvidenceJSON json.RawMessage `json:"evidence_json"`
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
	DurationSec float64      `json:"duration_sec,omitempty"`
	CasesTotal  int          `json:"cases_total,omitempty"`
	CasesFailed int          `json:"cases_failed,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	// Analysis 是 Phase 2 LLM Analyzer 的补充结论(仅非 PASSED 且 Analyzer 启用时
	// 非空);随输出与通知透出,判定权仍在规则引擎(§9)。
	Analysis  *hermesclient.Analysis `json:"analysis,omitempty"`
	retryable bool
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

	var sel SpecSelection
	if err := workflow.ExecuteActivity(ctx, "SelectTestSpecs", in).Get(ctx, &sel); err != nil {
		return nil, fmt.Errorf("select test specs: %w", err)
	}

	out := &DeviceTestOutput{}
	// fleet 无匹配设备/OS 未接入的变体:秒级标记 SKIPPED,不占设备不等待
	for _, sk := range sel.Skipped {
		out.Tasks = append(out.Tasks, TaskSummary{
			TestID: sk.Variant, Variant: sk.Variant,
			Verdict: VerdictSkipped, Reason: sk.Reason,
		})
	}
	resultCh := workflow.GetSignalChannel(ctx, SignalTaskResult)
	hbCh := workflow.GetSignalChannel(ctx, SignalTaskHeartbeat)

	for _, spec := range sel.Specs {
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

	// ---- 规则引擎判 verdict(结果本体已由回调服务 SaveResult 落库,§8.2) ----
	d := rules.Decide(rules.Input{
		Status: res.Status, ExitCode: res.ExitCode, CasesFailed: res.CasesFailed,
		SignaturesHit: res.SignaturesHit, SignatureCategory: spec.SignatureCategory,
	})
	sum.Verdict, sum.Category, sum.Reason = string(d.Verdict), string(d.Category), d.Reason
	sum.DurationSec, sum.CasesTotal, sum.CasesFailed = res.DurationSec, res.CasesTotal, res.CasesFailed
	sum.Attachments = res.Attachments
	sum.retryable = d.Retry
	// 规则裁决落 decisions 表(§11 可回放);INFRA 早退路径的裁决已随 FinishTask 落 tasks 表
	saveRuleDecision(dctx, taskID, d)
	// Phase 2:非 PASSED 提取证据并交 Analyzer 补充分析(降级设计,不影响主链路)
	if d.Verdict != rules.VerdictPassed {
		sum.Analysis = runAnalysis(ctx, dctx, taskID, spec, res, d)
	}
	finish(res.Status, sum.Verdict, sum.Category, sum.Reason)
	release(d.Category == rules.CategoryInfra)
	return sum
}

// saveRuleDecision 把规则引擎裁决落 decisions 表;失败只记日志(用 disconnected
// ctx:workflow 被取消也尽量留痕)。
func saveRuleDecision(dctx workflow.Context, taskID string, d rules.Decision) {
	out, err := json.Marshal(d)
	if err != nil {
		workflow.GetLogger(dctx).Error("marshal rule decision failed", "task", taskID, "error", err)
		return
	}
	row := DecisionRow{TaskID: taskID, Actor: "rule", Output: out}
	if err := workflow.ExecuteActivity(dctx, "SaveDecision", row).Get(dctx, nil); err != nil {
		workflow.GetLogger(dctx).Error("save rule decision failed", "task", taskID, "error", err)
	}
}

// runAnalysis 提取证据并交 LLM Analyzer 补充分析,分析结论落 decisions 表。
// 返回分析本体供输出/通知透出;提取/分析失败或 Analyzer 未启用返回 nil
// (全程降级,verdict 判定权永远在规则引擎,§9;§12 Hermes 不可用 → 规则引擎保底)。
func runAnalysis(ctx, dctx workflow.Context, taskID string, spec TestSpec, res *TaskResultSignal, d rules.Decision) *hermesclient.Analysis {
	logger := workflow.GetLogger(ctx)
	var ev ExtractEvidenceResponse
	if err := workflow.ExecuteActivity(ctx, "ExtractEvidence", ExtractEvidenceRequest{
		TaskID: taskID, Variant: spec.Variant, Result: *res,
	}).Get(ctx, &ev); err != nil {
		logger.Error("extract evidence failed, skip analysis", "task", taskID, "error", err)
		return nil
	}
	var analysis *hermesclient.Analysis
	if err := workflow.ExecuteActivity(ctx, "Analyze", AnalyzeRequest{
		TaskID: taskID, RuleCategory: string(d.Category), EvidenceJSON: ev.EvidenceJSON,
	}).Get(ctx, &analysis); err != nil {
		logger.Error("analyze failed, rule decision stands", "task", taskID, "error", err)
		return nil
	}
	if analysis == nil {
		return nil // Analyzer 未启用(HERMES_ENDPOINT 空)
	}
	out, err := json.Marshal(analysis)
	if err != nil {
		logger.Error("marshal analysis failed", "task", taskID, "error", err)
		return nil
	}
	row := DecisionRow{
		TaskID: taskID, Actor: "hermes", InputDigest: ev.Digest,
		PromptVersion: hermesclient.PromptVersion, Output: out,
	}
	if err := workflow.ExecuteActivity(dctx, "SaveDecision", row).Get(dctx, nil); err != nil {
		logger.Error("save hermes decision failed", "task", taskID, "error", err)
	}
	return analysis
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
		// 精练格式(§12.6):耗时与用例通过数是性能一瞥;附件 key 不进通知,
		// 需要时按 task_id 到 MinIO 取。
		if tk.CasesTotal > 0 {
			fmt.Fprintf(&b, " %.1fs cases=%d/%d", tk.DurationSec, tk.CasesTotal-tk.CasesFailed, tk.CasesTotal)
		}
		fmt.Fprintf(&b, " attempt=%d %s\n", tk.Attempt, tk.Reason)
		// Phase 2:LLM Analyzer 的总结性结论随通知透出(仅非 PASSED 且分析成功时存在)
		if tk.Analysis != nil && tk.Analysis.Summary != "" {
			fmt.Fprintf(&b, "  · hermes: %s\n", tk.Analysis.Summary)
		}
	}
	return b.String()
}
