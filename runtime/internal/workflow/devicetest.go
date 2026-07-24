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
	SignalTaskResult = "task-result"
)

// VerdictSkipped 标记在 SelectTestSpecs 阶段被跳过的变体(fleet 无匹配设备/
// OS 未接入)。只出现在 TaskSummary 与通知文案中,不经过规则引擎,
// §9 的 verdict 集合不变。
const VerdictSkipped = "SKIPPED"

// TaskResultSignal 是 /callbacks/v1/results 经 API 转投的终态(§8.2)。
// 事务性 Outbox 链路(差距清单 #1/#2)落地后,workflow 只消费其中的 task_id
// 做匹配去重,结果本体经 LoadResult 活动回读 results 表(权威读);
// 全量字段在过渡期保留以兼容直发双通道,Relay 全量部署后可收缩为轻量载荷。
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
// LeaseID/Generation 是租约所有权凭据(§10/差距 #15):随派单透传 Client,
// 心跳续租时必须原样携带,失配即 LEASE_NOT_OWNED。
type Lease struct {
	DeviceID      string `json:"device_id"`
	Serial        string `json:"serial"`
	ClientID      string `json:"client_id"`
	ClientBaseURL string `json:"client_base_url"`
	LeaseID       string `json:"lease_id"`
	Generation    int    `json:"lease_generation"`
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
	TaskID          string `json:"task_id"`
	IdempotencyKey  string `json:"idempotency_key"`
	Attempt         int    `json:"attempt"`
	PackageURL      string `json:"package_url"`
	PackageSHA256   string `json:"package_sha256"`
	ManifestDigest  string `json:"manifest_digest"`
	DeviceSerial    string `json:"device_serial"`
	ClientBaseURL   string `json:"client_base_url"`
	LeaseID         string `json:"lease_id"`         // 租约所有权凭据,Client 心跳续租时原样回传(§10)
	LeaseGeneration int    `json:"lease_generation"` // 同上
}

// CheckLeaseRequest 是 CheckLease 活动的入参(原则 6:租约到期 Timer 触发的
// 低频检查,非轮询)。
type CheckLeaseRequest struct {
	TaskID string `json:"task_id"`
}

type CancelRequest struct {
	TaskID        string `json:"task_id"`
	ClientBaseURL string `json:"client_base_url"`
}

// ResultRecord 是 results 表一行;由回调服务随 outbox 事件单事务落库
// (SaveResultWithOutbox 去重,原则 3),workflow 收到结果 signal 后经
// LoadResult 活动按 task_id 回读(权威读,差距清单 #2),不再消费 signal 载荷。
type ResultRecord struct {
	TaskID string           `json:"task_id"`
	Result TaskResultSignal `json:"result"`
}

// LoadResultRequest 是 LoadResult 活动的入参:signal 只作唤醒提示,
// 结果本体以 results 表为准(docs/device-test-sequence.md 时序图 §7)。
type LoadResultRequest struct {
	TaskID string `json:"task_id"`
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
// SelectTestSpecs → 逐测试 [acquire_device → dispatch → await_result(signal 唤醒,
// 租约到期 Durable Timer + CheckLease 低频检查,过期按 on_infra_error 机械重试 ≤2)
// → LoadResult 权威读 → 规则引擎按 rule_version 判 verdict → release_device]
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

	// 规则版本路由(原则 2/差距 #7):未知 rule_version 拒绝启动并明确报错,
	// 绝不静默用最新版判定——同一版本在重放与未来执行中必须得到同一裁决。
	ruleVersion := in.RuleVersion
	if ruleVersion == "" {
		ruleVersion = rules.DefaultVersion
	}
	if err := rules.ValidateVersion(ruleVersion); err != nil {
		return nil, fmt.Errorf("device test %s: %w", in.WorkflowID(), err)
	}

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

	for _, spec := range sel.Specs {
		out.Tasks = append(out.Tasks, runTest(ctx, spec, ruleVersion, resultCh))
	}

	text := buildNotification(in, out)
	if err := workflow.ExecuteActivity(ctx, "Notify", text).Get(ctx, nil); err != nil {
		workflow.GetLogger(ctx).Error("notify failed", "error", err)
	}
	return out, nil
}

// runTest 执行一个测试(含 INFRA 机械重试,§10 缺省 ≤2 次)。
func runTest(ctx workflow.Context, spec TestSpec, ruleVersion string, resultCh workflow.ReceiveChannel) TaskSummary {
	maxAttempts := spec.MaxInfraRetries + 1
	var sum TaskSummary
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		sum = runAttempt(ctx, spec, ruleVersion, attempt, resultCh)
		if !sum.retryable || attempt == maxAttempts {
			break
		}
		workflow.GetLogger(ctx).Info("infra failure, mechanical retry",
			"test", spec.TestID, "attempt", attempt, "reason", sum.Reason)
	}
	return sum
}

func runAttempt(ctx workflow.Context, spec TestSpec, ruleVersion string, attempt int, resultCh workflow.ReceiveChannel) TaskSummary {
	wfID := workflow.GetInfo(ctx).WorkflowExecution.ID
	// 幂等键 = {workflow_id}:{test_id}:{attempt}(§12.6),task_id 同值
	taskID := fmt.Sprintf("%s:%s:a%d", wfID, spec.TestID, attempt)
	sum := TaskSummary{TestID: spec.TestID, Variant: spec.Variant, TaskID: taskID, Attempt: attempt}
	infra := func(reason string, retryable bool) TaskSummary {
		// ruleVersion 已在 workflow 启动时 ValidateVersion,此处不会出错
		d, _ := rules.Decide(ruleVersion, rules.Input{Status: "FAILED", InfraReason: reason})
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
		// 租约所有权凭据透传 Client:心跳续租时原样回传,失配即 LEASE_NOT_OWNED(§10)
		LeaseID: lease.LeaseID, LeaseGeneration: lease.Generation,
	}).Get(ctx, nil); err != nil {
		finish("FAILED", string(rules.VerdictInfraError), string(rules.CategoryInfra), "dispatch failed")
		release(true)
		return infra("dispatch: "+err.Error(), true)
	}

	// ---- await_result:signal 驱动 + 租约到期 Durable Timer/CheckLease(原则 6,§14) ----
	if infraReason := awaitResult(ctx, taskID, spec, resultCh); infraReason != "" {
		_ = workflow.ExecuteActivity(dctx, "CancelTask",
			CancelRequest{TaskID: taskID, ClientBaseURL: lease.ClientBaseURL}).Get(dctx, nil)
		finish("FAILED", string(rules.VerdictInfraError), string(rules.CategoryInfra), infraReason)
		release(true)
		return infra(infraReason, true)
	}

	// ---- LoadResult 权威读(原则 3 + 差距清单 #2):signal 只是唤醒提示,
	// 结果本体以 results 表为准。读不到说明 outbox 链路异常(结果未随 signal
	// 落库),按 INFRA 处理走机械重试,绝不消费 signal 载荷兜底 ----
	var rec *ResultRecord
	loadErr := workflow.ExecuteActivity(ctx, "LoadResult", LoadResultRequest{TaskID: taskID}).Get(ctx, &rec)
	if loadErr != nil || rec == nil {
		reason := "load result: no row in results table"
		if loadErr != nil {
			reason = "load result: " + loadErr.Error()
		}
		_ = workflow.ExecuteActivity(dctx, "CancelTask",
			CancelRequest{TaskID: taskID, ClientBaseURL: lease.ClientBaseURL}).Get(dctx, nil)
		finish("FAILED", string(rules.VerdictInfraError), string(rules.CategoryInfra), reason)
		release(true)
		return infra(reason, true)
	}
	res := &rec.Result

	// ---- 规则引擎判 verdict(结果本体已由回调服务单事务落库,§8.2/原则 3;
	// 按 rule_version 路由,启动时已校验,此处不会出错) ----
	d, _ := rules.Decide(ruleVersion, rules.Input{
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

// awaitResult 阻塞等待本 task 的结果 signal;租约以 Durable Timer 到期驱动
// CheckLease 活动做低频检查(原则 6:心跳只续数据库租约,不向 workflow 发
// 高频 signal;Timer 到期才查库,非轮询):已续期 → 按新 expires_at 重设 Timer
// 继续等;已过期/租约易主 → 返回 infraReason 进入 INFRA 处理。
// 同 task_id 的重复 signal(Relay 至少一次重投)幂等:首个匹配即返回,
// 其余留在 channel 缓冲中随 workflow 结束丢弃;陌生/历史 attempt 的迟到
// 结果按 task_id 不匹配直接忽略。
func awaitResult(ctx workflow.Context, taskID string, spec TestSpec, resultCh workflow.ReceiveChannel) string {
	lease := time.Duration(spec.LeaseSeconds) * time.Second
	hardDeadline := workflow.Now(ctx).Add(time.Duration(spec.HardTimeoutSec) * time.Second)
	leaseExpiry := workflow.Now(ctx).Add(lease)

	matched := false
	for {
		now := workflow.Now(ctx)
		if now.After(hardDeadline) || now.Equal(hardDeadline) {
			return "hard deadline exceeded"
		}
		if now.After(leaseExpiry) || now.Equal(leaseExpiry) {
			// 租约到期:CheckLease 读库确认(心跳只续 DB 租约,§10)
			var expiry *time.Time
			if err := workflow.ExecuteActivity(ctx, "CheckLease",
				CheckLeaseRequest{TaskID: taskID}).Get(ctx, &expiry); err != nil {
				return "check lease: " + err.Error()
			}
			if expiry == nil || !expiry.After(now) {
				return "lease expired (no heartbeat)"
			}
			leaseExpiry = *expiry // 已续期:按库内新 expires_at 重设 Timer
			continue
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
				matched = true
			} // 其他 task(含历史 attempt)的迟到结果:忽略
		})
		sel.AddFuture(timer, func(workflow.Future) {}) // 唤醒后由循环头重新判定
		sel.Select(ctx)
		cancelTimer()

		if matched {
			return ""
		}
		if ctx.Err() != nil {
			return "workflow canceled"
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
