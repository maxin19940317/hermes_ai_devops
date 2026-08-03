package workflow

import (
	"encoding/json"
	"fmt"
	"sort"
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
	// EvidenceSnapshotID 引用 evidence_snapshots(差距 #6 决策可回放);
	// 仅 hermes 裁决携带(基于 evidence),rule 裁决基于 result,为空。
	// 快照未持久化(MinIO 未配置/上传失败,降级)时亦为空。
	EvidenceSnapshotID string `json:"evidence_snapshot_id,omitempty"`
}

// EscalationGateRequest 是 EscalationGate 活动的入参(升级门槛评估,
// 设计 §5):阈值与判重等配置/存储状态经活动进入 workflow,保持确定性。
type EscalationGateRequest struct {
	TaskID   string `json:"task_id"`
	Category string `json:"category"`
}

// EscalationGateResponse 是升级门槛的配置侧输入:Enabled=false(未配置
// ESCALATION_ENDPOINT)时 workflow 完全跳过升级旁路。
type EscalationGateResponse struct {
	Enabled          bool    `json:"enabled"`
	MinConfidence    float64 `json:"min_confidence"`
	AlreadyEscalated bool    `json:"already_escalated"` // decisions 已有 actor='escalation'
}

// EscalationRequest 是 Escalate 活动的入参:组信封所需的全部事实
// (信封结构 contracts/escalation.schema.json)。
type EscalationRequest struct {
	TaskID     string `json:"task_id"`
	Project    string `json:"project"`
	Commit     string `json:"commit"`
	PipelineID int    `json:"pipeline_iid"`
	Variant    string `json:"variant"`
	Verdict    string `json:"verdict"`
	Category   string `json:"category"`
	Reason     string `json:"reason"`
	// SignatureOrCategory 幂等键尾段(设计 §4):有签名命中取首个签名 id,
	// 否则取 rule category。
	SignatureOrCategory string                 `json:"signature_or_category"`
	Analysis            *hermesclient.Analysis `json:"analysis,omitempty"`
	// EvidenceSnapshotID 引用已持久化快照;空 = 快照降级未持久化,
	// 信封 evidence 段省略,不阻断升级。
	EvidenceSnapshotID string `json:"evidence_snapshot_id,omitempty"`
}

// EscalationResponse 是 Escalate 活动的结果(落 decisions 的 output 同源)。
type EscalationResponse struct {
	KanbanTaskID   string `json:"kanban_task_id"`
	IdempotencyKey string `json:"idempotency_key"`
	Result         string `json:"result"` // created | existing
}

// ExtractEvidenceRequest 是 ExtractEvidence 活动的入参(§12 Phase 2)。
type ExtractEvidenceRequest struct {
	TaskID  string           `json:"task_id"`
	Variant string           `json:"variant"`
	Project string           `json:"project"` // 基线查询键(§9 metrics 表)
	Result  TaskResultSignal `json:"result"`
}

// ExtractEvidenceResponse 携带 evidence.json 序列化形态及其 sha256 摘要;
// 摘要在 decisions 表充当 hermes 裁决的 input_digest(§11 可回放)。
// MatchedSignatures 是 runtime 侧确定性提取的签名命中 id 列表(按声明序),
// 作为规则引擎判定的额外输入(判定权仍在规则引擎,§9)。
// SnapshotID 是 evidence_snapshots.evidence_id(差距 #6);空 = 快照未持久化
// (MinIO 未配置/上传失败,降级,evidence 本体仍随 EvidenceJSON 内存传递)。
type ExtractEvidenceResponse struct {
	EvidenceJSON      json.RawMessage `json:"evidence_json"`
	Digest            string          `json:"digest"`
	MatchedSignatures []string        `json:"matched_signatures,omitempty"`
	SnapshotID        string          `json:"snapshot_id,omitempty"`
}

// AnalyzeRequest 是 Analyze 活动的入参;RuleCategory 为规则引擎判定类别(§9),
// 供 Analyzer 参考,verdict 判定权始终在规则引擎。
type AnalyzeRequest struct {
	TaskID       string          `json:"task_id"`
	RuleCategory string          `json:"rule_category"`
	EvidenceJSON json.RawMessage `json:"evidence_json"`
}

// FailScope 是一次设备释放的失败归因(设计文档 §4)。四个取值互斥:
//
//	ok     终态且非 INFRA 类判定 → 两个计数器都清零
//	device 设备级失败           → devices.fail_streak+1,达阈值 QUARANTINED
//	client Client Agent 或与它之间的网络 → clients.fail_streak+1
//	none   Runtime 自身故障/取消/成因两可 → 两个计数器都不动
//
// none 与 ok 不可合并:Runtime 挂了既不是设备健康的证据(不能清零),
// 也不是设备的错(不能加一)。改动前这两种情况都被记成"设备又坏了一次"。
type FailScope string

const (
	FailScopeOK     FailScope = "ok"
	FailScopeDevice FailScope = "device"
	FailScopeClient FailScope = "client"
	FailScopeNone   FailScope = "none"
)

type ReleaseRequest struct {
	DeviceID string `json:"device_id"`
	TaskID   string `json:"task_id"`
	// InfraFail 是改动前的归因字段。**保留不删**:它进过 workflow history,
	// 在途 workflow 重放时会原样送回来(设计文档 §5)。
	InfraFail bool `json:"infra_fail"`
	// FailScope 是新的四值归因(差距 #10)。为空 = 旧载荷,活动按 InfraFail 翻译。
	FailScope FailScope `json:"fail_scope,omitempty"`
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

	actualID := workflow.GetInfo(ctx).WorkflowExecution.ID

	if workflow.GetVersion(
		ctx, "record-workflow-run-v1", workflow.DefaultVersion, 1,
	) != workflow.DefaultVersion {
		if actualID != in.WorkflowID() {
			return nil, fmt.Errorf(
				"workflow execution id %q does not match input id %q",
				actualID, in.WorkflowID(),
			)
		}
		recordCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: 30 * time.Second,
			RetryPolicy: &temporal.RetryPolicy{
				InitialInterval:    2 * time.Second,
				BackoffCoefficient: 2,
				MaximumInterval:    time.Minute,
				MaximumAttempts:    0,
			},
		})
		req := newRecordWorkflowRunRequest(actualID, in, ruleVersion)
		if err := workflow.ExecuteActivity(
			recordCtx, "RecordWorkflowRun", req,
		).Get(recordCtx, nil); err != nil {
			return nil, fmt.Errorf("record workflow run: %w", err)
		}
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
		out.Tasks = append(out.Tasks, runTest(ctx, in, spec, ruleVersion, resultCh))
	}

	// notify-card 版本分支(设计文档 §5):在途 workflow(重放旧 history)必须原样
	// 发纯文本,新 workflow 一律发交互卡片。buildNotification 两个分支都要调用——
	// 旧分支直接发送,新分支作为卡片的降级文本随载荷下发,因此不是死代码。
	if workflow.GetVersion(ctx, "notify-card", workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		if err := workflow.ExecuteActivity(ctx, "Notify", buildNotification(in, out)).Get(ctx, nil); err != nil {
			workflow.GetLogger(ctx).Error("notify failed", "error", err)
		}
	} else {
		req := NotifyCardRequest{
			Card:         buildNotificationCard(in, out, actualID),
			FallbackText: buildNotification(in, out), // 原函数,原样调用
		}
		if err := workflow.ExecuteActivity(ctx, "NotifyCard", req).Get(ctx, nil); err != nil {
			workflow.GetLogger(ctx).Error("notify card failed", "error", err)
		}
	}
	return out, nil
}

func newRecordWorkflowRunRequest(
	workflowID string,
	in DeviceTestInput,
	ruleVersion string,
) RecordWorkflowRunRequest {
	variants := make([]string, 0, len(in.Packages))
	for _, pkg := range in.Packages {
		if pkg.Variant != "" {
			variants = append(variants, pkg.Variant)
		}
	}
	sort.Strings(variants)
	canonical := variants[:0]
	for _, variant := range variants {
		if len(canonical) == 0 || canonical[len(canonical)-1] != variant {
			canonical = append(canonical, variant)
		}
	}
	return RecordWorkflowRunRequest{
		WorkflowID:       workflowID,
		Project:          in.Project,
		CommitSHA:        in.Commit,
		PipelineID:       in.PipelineID,
		Version:          in.Version,
		RuleVersion:      ruleVersion,
		Scope:            in.Scope,
		Attempt:          in.Attempt,
		Variants:         canonical,
		SourceWorkflowID: in.SourceWorkflowID,
	}
}

// runTest 执行一个测试(含 INFRA 机械重试,§10 缺省 ≤2 次)。
func runTest(ctx workflow.Context, in DeviceTestInput, spec TestSpec, ruleVersion string, resultCh workflow.ReceiveChannel) TaskSummary {
	maxAttempts := spec.MaxInfraRetries + 1
	var sum TaskSummary
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		sum = runAttempt(ctx, in, spec, ruleVersion, attempt, resultCh)
		if !sum.retryable || attempt == maxAttempts {
			break
		}
		workflow.GetLogger(ctx).Info("infra failure, mechanical retry",
			"test", spec.TestID, "attempt", attempt, "reason", sum.Reason)
	}
	return sum
}

// releaseSite 标识释放发生在 workflow 的哪个失败分支(设计文档 §4)。
// 用枚举而不是解析 reason 字符串:workflow 本来就知道自己站在哪个分支上。
type releaseSite string

const (
	siteCreateTaskFailed releaseSite = "create_task_failed"
	siteDispatchFailed   releaseSite = "dispatch_failed"
	siteLeaseExpired     releaseSite = "lease_expired"
	siteCheckLeaseFailed releaseSite = "check_lease_failed"
	siteHardDeadline     releaseSite = "hard_deadline"
	siteCanceled         releaseSite = "canceled"
	siteLoadResultFailed releaseSite = "load_result_failed"
	siteTerminal         releaseSite = "terminal"
)

// failScope 决定一次释放该记在谁头上(设计文档 §4 归因表)。纯函数,表驱动单测。
//
// 终态分支需要 resultStatus:d.Category 单独不足以区分 FAILED(client 侧流水线
// 失败)与 TIMEOUT(工作负载属性)——两者都是 CategoryInfra。
func failScope(site releaseSite, category rules.Category, resultStatus string) FailScope {
	switch site {
	case siteDispatchFailed, siteLeaseExpired:
		// 已知盲区(设计文档 §4.1):callbacks 进程自身宕机 ≥120s 时,心跳送不达、
		// 租约照样过期,这里会把 Runtime 的故障记成 client 失联。workflow 视角内
		// 无法区分。本轮无代价(计数不驱动行为);若将来用它做自动处置,必须先解决,
		// 否则 Runtime 重启一次就会把整个 fleet 的 client 全停掉。
		// 判别特征:callbacks 宕机时全 fleet 的 client 计数同时上涨。
		return FailScopeClient
	case siteCreateTaskFailed, siteCheckLeaseFailed, siteHardDeadline,
		siteCanceled, siteLoadResultFailed:
		return FailScopeNone
	case siteTerminal:
		switch {
		case resultStatus == "CANCELED":
			// 取消不是任何一方的错,也不是"干完了"的证据(设计 §4:取消归 none)。
			// 与 siteCanceled 保持一致:同一类事件不因谁先观察到而改变归因。
			return FailScopeNone
		case category == rules.CategoryDevice:
			// 目前无人产出 rules.CategoryDevice(设计 §7:设备级信号源本轮不做),
			// 这个分支恒不可达,device_fail_streak 因此恒为 0——不是缺陷,是保留位。
			return FailScopeDevice
		case category == rules.CategoryInfra && resultStatus == "FAILED":
			return FailScopeClient
		case category == rules.CategoryInfra:
			// 覆盖 TIMEOUT,以及未来任何 classify: INFRA 的签名命中
			// (ci/variants.yaml 新增该分类时,判定落在这里,不落 FAILED 分支)。
			return FailScopeNone
		default:
			return FailScopeOK
		}
	}
	return FailScopeNone // 未覆盖组合保守处理:不加不减
}

func runAttempt(ctx workflow.Context, in DeviceTestInput, spec TestSpec, ruleVersion string, attempt int, resultCh workflow.ReceiveChannel) TaskSummary {
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
	// 清理动作的错误不改变 workflow 结论(设备最终会被 AcquireDevice 懒回收,
	// 终态以 results/tasks 表为准),但绝不能静默:释放失败意味着设备要等到
	// 租约过期才回池,落终态失败意味着 tasks 表与 workflow 结论不一致——
	// 两者都只能靠日志发现。
	// legacyInfraFail 是改动前传给 activity 的原值;scope 是新归因(差距 #10)。
	// 两者并存是为了让 workflow.GetVersion 的旧分支能重放出一模一样的载荷(设计 §5)。
	release := func(legacyInfraFail bool, scope FailScope) {
		if released {
			return
		}
		released = true
		req := ReleaseRequest{DeviceID: lease.DeviceID, TaskID: taskID}
		if workflow.GetVersion(dctx, "release-fail-scope", workflow.DefaultVersion, 1) == workflow.DefaultVersion {
			req.InfraFail = legacyInfraFail // 在途 workflow:原样重放
		} else {
			req.FailScope = scope
		}
		if err := workflow.ExecuteActivity(dctx, "ReleaseDevice", req).Get(dctx, nil); err != nil {
			workflow.GetLogger(dctx).Error("release device failed, lease will expire on its own",
				"task", taskID, "device", lease.DeviceID, "scope", scope, "error", err)
		}
	}

	// ---- 登记任务 + dispatch ----
	if err := workflow.ExecuteActivity(ctx, "CreateTask", TaskRow{
		TaskID: taskID, WorkflowID: wfID, TestID: spec.TestID, Attempt: attempt,
		IdempotencyKey: taskID, ClientID: lease.ClientID, DeviceID: lease.DeviceID,
		Status: "DISPATCHING",
	}).Get(ctx, nil); err != nil {
		release(false, failScope(siteCreateTaskFailed, "", ""))
		return infra("create task: "+err.Error(), true)
	}
	finish := func(status, verdict, category, reason string) {
		if err := workflow.ExecuteActivity(dctx, "FinishTask", FinishRequest{
			TaskID: taskID, Status: status, Verdict: verdict, Category: category, Reason: reason,
		}).Get(dctx, nil); err != nil {
			workflow.GetLogger(dctx).Error("finish task failed, tasks row left non-terminal",
				"task", taskID, "status", status, "verdict", verdict, "error", err)
		}
	}
	// 取消是尽力而为(§8.1):Client 可能已离线,失败不改变结论,但要留痕——
	// 取消没送达意味着设备上可能还有进程在跑。
	cancel := func(why string) {
		if err := workflow.ExecuteActivity(dctx, "CancelTask",
			CancelRequest{TaskID: taskID, ClientBaseURL: lease.ClientBaseURL}).Get(dctx, nil); err != nil {
			workflow.GetLogger(dctx).Error("cancel task failed, device process may still be running",
				"task", taskID, "why", why, "error", err)
		}
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
		release(true, failScope(siteDispatchFailed, "", ""))
		return infra("dispatch: "+err.Error(), true)
	}

	// ---- await_result:signal 驱动 + 租约到期 Durable Timer/CheckLease(原则 6,§14) ----
	if site, infraReason := awaitResult(ctx, taskID, spec, resultCh); infraReason != "" {
		cancel(infraReason)
		finish("FAILED", string(rules.VerdictInfraError), string(rules.CategoryInfra), infraReason)
		release(true, failScope(site, "", ""))
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
		cancel(reason)
		finish("FAILED", string(rules.VerdictInfraError), string(rules.CategoryInfra), reason)
		release(true, failScope(siteLoadResultFailed, "", ""))
		return infra(reason, true)
	}
	res := &rec.Result

	// ---- 规则引擎判 verdict(结果本体已由回调服务单事务落库,§8.2/原则 3;
	// 按 rule_version 路由,启动时已校验,此处不会出错) ----
	d, _ := rules.Decide(ruleVersion, rules.Input{
		Status: res.Status, ExitCode: res.ExitCode, CasesFailed: res.CasesFailed,
		SignaturesHit: res.SignaturesHit, SignatureCategory: spec.SignatureCategory,
	})

	// 非 PASSED:提取一次证据,复用给规则归类(签名命中)与 Hermes 分析。
	// 顺序语义:先规则后分析,分析永不影响判定(§9 红线)。
	var ev *ExtractEvidenceResponse
	if d.Verdict != rules.VerdictPassed {
		ev = extractEvidenceOnce(ctx, taskID, spec, res, in.Project)
		if ev != nil {
			// 规则归类修复:runtime 侧确定性提取的签名命中(SDK 测试程序不自报)
			// 作为规则引擎的额外输入。设备自报优先(同名冲突类别以自报为准,
			// 首个命中定类别);verdict 不因签名变"好"(refined 只会是
			// TEST_FAILED 或更具体的 INFRA 类,典型修复:CODE → DELEGATE)。
			if merged, added := mergeSignatureHits(res.SignaturesHit, ev.MatchedSignatures); added {
				d2, _ := rules.Decide(ruleVersion, rules.Input{
					Status: res.Status, ExitCode: res.ExitCode, CasesFailed: res.CasesFailed,
					SignaturesHit: merged, SignatureCategory: spec.SignatureCategory,
				})
				d = d2
			}
		}
	}
	sum.Verdict, sum.Category, sum.Reason = string(d.Verdict), string(d.Category), d.Reason
	sum.DurationSec, sum.CasesTotal, sum.CasesFailed = res.DurationSec, res.CasesTotal, res.CasesFailed
	sum.Attachments = res.Attachments
	sum.retryable = d.Retry
	// 规则裁决落 decisions 表(§11 可回放):落的是归类修复后的最终裁决
	// (reason 含签名 id 可与初判区分);INFRA 早退路径的裁决已随 FinishTask 落 tasks 表
	saveRuleDecision(dctx, taskID, d)
	// Phase 2:非 PASSED 交 Analyzer 补充分析(降级设计,不影响主链路)
	if d.Verdict != rules.VerdictPassed {
		sum.Analysis = runAnalysis(ctx, dctx, taskID, d, ev)
		// 升级旁路(设计 §5):有稳定诊断的非 INFRA 失败派给 PM;
		// 全程 fire-and-forget,失败只记日志
		maybeEscalate(ctx, in, taskID, spec, res, d, ev, sum.Analysis)
	}
	finish(res.Status, sum.Verdict, sum.Category, sum.Reason)
	release(d.Category == rules.CategoryInfra, failScope(siteTerminal, d.Category, res.Status))
	return sum
}

// extractEvidenceOnce 执行一次证据提取(非 PASSED 路径),供规则归类与
// Hermes 分析复用;失败返回 nil(降级:归类与分析都按现状进行,
// 证据缺失不构成重试理由,§3.7)。
func extractEvidenceOnce(ctx workflow.Context, taskID string, spec TestSpec, res *TaskResultSignal, project string) *ExtractEvidenceResponse {
	var ev ExtractEvidenceResponse
	if err := workflow.ExecuteActivity(ctx, "ExtractEvidence", ExtractEvidenceRequest{
		TaskID: taskID, Variant: spec.Variant, Project: project, Result: *res,
	}).Get(ctx, &ev); err != nil {
		workflow.GetLogger(ctx).Error("extract evidence failed, rule decision stands", "task", taskID, "error", err)
		return nil
	}
	return &ev
}

// mergeSignatureHits 合并设备自报与 runtime 提取的签名命中:设备自报在前
// (优先),runtime 命中按声明序去重追加;返回合并列表与是否有新增。
func mergeSignatureHits(reported, extracted []string) ([]string, bool) {
	merged := append([]string{}, reported...)
	seen := make(map[string]bool, len(reported)+len(extracted))
	for _, s := range reported {
		seen[s] = true
	}
	added := false
	for _, s := range extracted {
		if !seen[s] {
			seen[s] = true
			merged = append(merged, s)
			added = true
		}
	}
	return merged, added
}

// escalatableCategories:有稳定诊断的非 INFRA 失败才升级(设计 §5);
// INFRA 类由机械重试与归因计数负责,不打扰 PM。
func escalatableCategory(c rules.Category) bool {
	switch c {
	case rules.CategoryCode, rules.CategoryModel, rules.CategoryDelegate, rules.CategoryDevice:
		return true
	}
	return false
}

// maybeEscalate 升级旁路(docs/superpowers/specs/2026-07-30 §2/§5):
// Hermes 分析后按门槛评估(category ∈ {CODE,MODEL,DELEGATE,DEVICE} +
// 启用 + 未升级过 + analysis 非空 + confidence ≥ 阈值),满足则组信封经
// Escalate 活动派给 PM。全程 fire-and-forget:任何失败只记日志,
// verdict/通知/审计主链路不变(§3:agent 不在执行关键路径)。
func maybeEscalate(ctx workflow.Context, in DeviceTestInput, taskID string, spec TestSpec, res *TaskResultSignal, d rules.Decision, ev *ExtractEvidenceResponse, analysis *hermesclient.Analysis) {
	logger := workflow.GetLogger(ctx)
	if !escalatableCategory(d.Category) {
		return
	}
	var gate EscalationGateResponse
	if err := workflow.ExecuteActivity(ctx, "EscalationGate",
		EscalationGateRequest{TaskID: taskID, Category: string(d.Category)}).Get(ctx, &gate); err != nil {
		logger.Error("escalation gate failed, skip escalation", "task", taskID, "error", err)
		return
	}
	if !gate.Enabled || gate.AlreadyEscalated || analysis == nil ||
		analysis.Confidence < gate.MinConfidence {
		return
	}
	// 幂等键尾段(设计 §4):有签名命中取首个签名 id(设备自报优先),否则 category
	sigOrCat := string(d.Category)
	var evSigs []string
	if ev != nil {
		evSigs = ev.MatchedSignatures
	}
	if merged, _ := mergeSignatureHits(res.SignaturesHit, evSigs); len(merged) > 0 {
		sigOrCat = merged[0]
	}
	var snapID string
	if ev != nil {
		snapID = ev.SnapshotID
	}
	var escResp EscalationResponse
	if err := workflow.ExecuteActivity(ctx, "Escalate", EscalationRequest{
		TaskID: taskID, Project: in.Project, Commit: in.Commit, PipelineID: in.PipelineID,
		Variant: spec.Variant, Verdict: string(d.Verdict), Category: string(d.Category),
		Reason: d.Reason, SignatureOrCategory: sigOrCat, Analysis: analysis,
		EvidenceSnapshotID: snapID,
	}).Get(ctx, &escResp); err != nil {
		// activity 内部已落 error 审计行(§7);此处只记日志,不影响主链路
		logger.Error("escalate failed", "task", taskID, "error", err)
	}
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

// runAnalysis 交 LLM Analyzer 补充分析(复用 runAttempt 已提取的证据,
// 不重复调用 ExtractEvidence),分析结论落 decisions 表。
// 返回分析本体供输出/通知透出;分析失败或 Analyzer 未启用返回 nil
// (全程降级,verdict 判定权永远在规则引擎,§9;§12 Hermes 不可用 → 规则引擎保底)。
func runAnalysis(ctx, dctx workflow.Context, taskID string, d rules.Decision, ev *ExtractEvidenceResponse) *hermesclient.Analysis {
	logger := workflow.GetLogger(ctx)
	if ev == nil {
		return nil // 证据提取失败(降级),无分析输入
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
		PromptVersion: hermesclient.PromptVersionAnalyze, Output: out,
		// 快照引用(差距 #6 决策可回放);降级(未持久化)时为空
		EvidenceSnapshotID: ev.SnapshotID,
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
func awaitResult(ctx workflow.Context, taskID string, spec TestSpec, resultCh workflow.ReceiveChannel) (releaseSite, string) {
	lease := time.Duration(spec.LeaseSeconds) * time.Second
	hardDeadline := workflow.Now(ctx).Add(time.Duration(spec.HardTimeoutSec) * time.Second)
	leaseExpiry := workflow.Now(ctx).Add(lease)

	matched := false
	for {
		now := workflow.Now(ctx)
		if now.After(hardDeadline) || now.Equal(hardDeadline) {
			return siteHardDeadline, "hard deadline exceeded"
		}
		if now.After(leaseExpiry) || now.Equal(leaseExpiry) {
			// 租约到期:CheckLease 读库确认(心跳只续 DB 租约,§10)
			var expiry *time.Time
			if err := workflow.ExecuteActivity(ctx, "CheckLease",
				CheckLeaseRequest{TaskID: taskID}).Get(ctx, &expiry); err != nil {
				return siteCheckLeaseFailed, "check lease: " + err.Error()
			}
			if expiry == nil || !expiry.After(now) {
				return siteLeaseExpired, "lease expired (no heartbeat)"
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
			return "", ""
		}
		if ctx.Err() != nil {
			return siteCanceled, "workflow canceled"
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

// ---- 通知卡片(Phase 2,设计文档 §4.4)----

// NotificationCard 是终态通知卡片的封闭结构(本轮范围门禁)。
// 它不含任何可表达交互的字段——没有 actions、没有 behaviors、没有 value——
// 因此按钮不可能在不修改本类型(进而不修改 spec)的前提下漏进展示卡片。
type NotificationCard struct {
	Config   CardConfig    `json:"config"`
	Header   CardHeader    `json:"header"`
	Elements []CardElement `json:"elements"`
}

type CardConfig struct {
	WideScreenMode bool `json:"wide_screen_mode"`
}

type CardHeader struct {
	Title    CardText `json:"title"`
	Template string   `json:"template"` // 只允许 green | red | orange(§4.1)
}

// CardElement 有三种形态:文本块(tag=div,Text 非空)、分隔线(tag=hr,Text 为 nil)、
// 交互按钮组(tag=action,Actions 非空)。三个形态互斥:运行时只设置一种。
type CardElement struct {
	Tag     string       `json:"tag"`               // div | hr | action
	Text    *CardText    `json:"text,omitempty"`    // tag=hr|action 时必须为 nil
	Actions []CardButton `json:"actions,omitempty"` // tag=div|hr 时必须为 nil
}

// CardText 的 Tag 恒为 plain_text(§4.5),没有 lark_md 这个选项。
type CardText struct {
	Tag     string `json:"tag"` // 恒为 "plain_text"
	Content string `json:"content"`
}

// CardAction 是飞书卡片 1.0 的 action 模块,包含一组按钮。
// 与 CardElement 共享 namespace:tag="action" 不被 div/hr 白名单拦截。
type CardAction struct {
	Tag     string       `json:"tag"`     // 恒为 "action"
	Actions []CardButton `json:"actions"` // 至少一个
}

// CardButton 是飞书卡片 1.0 的 button 元素。
type CardButton struct {
	Tag   string      `json:"tag"`   // 恒为 "button"
	Text  CardText    `json:"text"`  // 按钮文案
	Type  string      `json:"type"`  // primary | default | danger
	Value ButtonValue `json:"value"` // 点击后原样回传的载荷
}

// ButtonValue 是按钮点击后经 WS card.action.trigger 原样回传的载荷(§10(4))。
// 只放身份标识(action+source_workflow_id+variant),其余一律从权威记录派生。
type ButtonValue struct {
	Action           string `json:"action"`             // "retry" | "ignore"
	SourceWorkflowID string `json:"source_workflow_id"` // 重试来源;忽略时仍携带以便审计
	Variant          string `json:"variant"`            // 具体变体
}

// NotifyCardRequest 是 NotifyCard 活动的输入(进 workflow history)。
// FallbackText 由 workflow 调既有的 buildNotification 生成并随载荷下发:
// activity 侧**绝不自行拼文本**——两处实现同一格式必然漂移,而"降级内容与改动前
// 逐字节相同"是本轮的验收项。
type NotifyCardRequest struct {
	Card         NotificationCard `json:"card"`
	FallbackText string           `json:"fallback_text"`
}

// cardByteBudget 是卡片序列化后的总大小上限(设计 §4.5)。
const cardByteBudget = 30 * 1024

// cardReasonSummaryLimit 是单个 Reason / Analysis.Summary 的 rune 上限(设计 §4.5)。
const cardReasonSummaryLimit = 500

// cardTruncationMarker 是超长文本被截断后追加的省略标记(设计 §4.5:"截断并带省略标记")。
// 提到包级是为了让测试直接引用这个真实值断言"标记确实出现",而不是在测试里
// 另造一份字面量、悄悄漂离实现。
const cardTruncationMarker = "…(已截断)"

// truncateRunes 按 rune 边界截断超长文本并加省略标记(设计 §4.5)。
// 用 []rune 切片而非 s[:n] 字节切片:中文在 UTF-8 下是多字节,按字节切会切出
// 半个字符,导致 utf8.ValidString 为假,飞书侧可能渲染乱码甚至拒收。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	markerRunes := []rune(cardTruncationMarker)
	keep := max - len(markerRunes)
	if keep < 0 {
		keep = 0
	}
	return string(r[:keep]) + cardTruncationMarker
}

// plainCardDiv 构造一个 tag=div、text.tag=plain_text 的卡片行(设计 §4.5:
// 卡片里所有文本节点一律 plain_text,不用 lark_md)。
func plainCardDiv(content string) CardElement {
	return CardElement{Tag: "div", Text: &CardText{Tag: "plain_text", Content: content}}
}

// cardHeaderTemplate 按设计 §4.1 判定 header 颜色。
// 可判定失败 = verdict ∉ {PASSED, SKIPPED};业务失败优先:同时存在 INFRA_ERROR
// 与其他失败时判红,不判橙——否则会把"代码有问题"显示成"环境有问题"。
func cardHeaderTemplate(tasks []TaskSummary) string {
	hasNonInfra := false
	hasInfra := false
	for _, tk := range tasks {
		if tk.Verdict == string(rules.VerdictPassed) || tk.Verdict == VerdictSkipped {
			continue
		}
		if tk.Verdict == string(rules.VerdictInfraError) {
			hasInfra = true
		} else {
			hasNonInfra = true
		}
	}
	switch {
	case hasNonInfra:
		return "red"
	case hasInfra:
		return "orange"
	default:
		return "green"
	}
}

// cardVariantBlock 是单个变体在卡片里的行分组:主行(main)恒存在且不可裁剪,
// metric 按格式表条件出现但始终保留;reason/hermes 是"该变体的详情",
// 裁剪时按变体整体丢弃(设计 §4.5 第 1 步)。
// actions 是变体失败时的交互按钮组("重试该变体"/"忽略"),不占裁剪预算。
type cardVariantBlock struct {
	main    CardElement
	metric  *CardElement
	reason  *CardElement
	hermes  *CardElement
	actions *CardElement // tag=action 按钮组;nil = 该变体不可重试(已 PASSED/SKIPPED)
}

func (b cardVariantBlock) hasDetail() bool { return b.reason != nil || b.hermes != nil }

func (b *cardVariantBlock) clearDetail() {
	b.reason = nil
	b.hermes = nil
}

func (b cardVariantBlock) flatten() []CardElement {
	es := []CardElement{b.main}
	if b.metric != nil {
		es = append(es, *b.metric)
	}
	if b.reason != nil {
		es = append(es, *b.reason)
	}
	if b.hermes != nil {
		es = append(es, *b.hermes)
	}
	if b.actions != nil {
		es = append(es, *b.actions)
	}
	return es
}

// buildCardVariantBlock 逐条对齐 buildNotification 的字段(设计 §4.3),
// 唯一有意偏离:SKIPPED 不显示 attempt。
// 可判定失败(verdict ∉ {PASSED,SKIPPED})的变体附加"重试该变体"/"忽略"按钮。
func buildCardVariantBlock(tk TaskSummary, workflowID string) cardVariantBlock {
	main := fmt.Sprintf("%s  %s", tk.Variant, tk.Verdict)
	if tk.Verdict != string(rules.VerdictPassed) && tk.Category != "" {
		main += fmt.Sprintf("(%s)", tk.Category)
	}
	blk := cardVariantBlock{main: plainCardDiv(main)}

	var parts []string
	if tk.CasesTotal > 0 {
		parts = append(parts, fmt.Sprintf("%.1fs", tk.DurationSec))
		parts = append(parts, fmt.Sprintf("cases %d/%d", tk.CasesTotal-tk.CasesFailed, tk.CasesTotal))
	}
	if tk.Verdict != VerdictSkipped {
		parts = append(parts, fmt.Sprintf("attempt %d", tk.Attempt))
	}
	if len(parts) > 0 {
		m := plainCardDiv(strings.Join(parts, " · "))
		blk.metric = &m
	}

	if tk.Reason != "" {
		r := plainCardDiv(truncateRunes(tk.Reason, cardReasonSummaryLimit))
		blk.reason = &r
	}
	if tk.Analysis != nil && tk.Analysis.Summary != "" {
		h := plainCardDiv("hermes: " + truncateRunes(tk.Analysis.Summary, cardReasonSummaryLimit))
		blk.hermes = &h
	}
	// 可判定失败(verdict ∉ {PASSED,SKIPPED})的变体附加交互按钮。
	// SKIPPED(OS未接入/无匹配设备)不重试;PASSED 不重试;INFRA_ERROR 亦给按钮
	// (用户判断是否环境已恢复)。
	if tk.Verdict != string(rules.VerdictPassed) && tk.Verdict != VerdictSkipped &&
		workflowID != "" {
		blk.actions = &CardElement{
			Tag: "action",
			Actions: []CardButton{
				{
					Tag:  "button",
					Text: CardText{Tag: "plain_text", Content: "重试该变体"},
					Type: "primary",
					Value: ButtonValue{
						Action:           "retry",
						SourceWorkflowID: workflowID,
						Variant:          tk.Variant,
					},
				},
				{
					Tag:  "button",
					Text: CardText{Tag: "plain_text", Content: "忽略"},
					Type: "default",
					Value: ButtonValue{
						Action:           "ignore",
						SourceWorkflowID: workflowID,
						Variant:          tk.Variant,
					},
				},
			},
		}
	}
	return blk
}

// flattenCardBlocks 把各变体的行拼成 Elements,变体之间插 hr(首个变体前不插)。
func flattenCardBlocks(blocks []cardVariantBlock) []CardElement {
	var es []CardElement
	for i, b := range blocks {
		if i > 0 {
			es = append(es, CardElement{Tag: "hr"})
		}
		es = append(es, b.flatten()...)
	}
	return es
}

// cardOmittedNotice 是裁剪后追加的省略标注(设计 §4.5)。格式串是契约的一部分:
// TestBuildNotificationCardTrimsFromTail 按此串精确匹配,改措辞两边要同步改。
func cardOmittedNotice(n int) CardElement {
	return plainCardDiv(fmt.Sprintf("（%d 个变体的详情已省略）", n))
}

// buildNotificationCard 把 buildNotification 同源的数据渲染成飞书交互卡片
// (设计 §4)。字段对齐见 §4.3,封闭结构见 §4.4,裁剪见 §4.5。
// workflowID 是本次运行的 workflow ID,注入按钮 payload 的 source_workflow_id;
// 空值时跳过按钮(兼容纯文本降级路径)。
func buildNotificationCard(in DeviceTestInput, out *DeviceTestOutput, workflowID string) NotificationCard {
	title := fmt.Sprintf("[hermes-devops] %s g%s p%d (v%s)", in.Project, in.Commit, in.PipelineID, in.Version)
	card := NotificationCard{
		Config: CardConfig{WideScreenMode: true},
		Header: CardHeader{
			Title:    CardText{Tag: "plain_text", Content: title},
			Template: cardHeaderTemplate(out.Tasks),
		},
	}

	if len(out.Tasks) == 0 {
		card.Elements = []CardElement{plainCardDiv("无可测变体(Android 包缺失或未配置)")}
		return card
	}

	blocks := make([]cardVariantBlock, len(out.Tasks))
	for i, tk := range out.Tasks {
		blocks[i] = buildCardVariantBlock(tk, workflowID)
	}
	card.Elements = flattenCardBlocks(blocks)

	if raw, err := json.Marshal(card); err == nil && len(raw) <= cardByteBudget {
		return card
	}

	// 超预算:按变体顺序从末尾丢可选行(reason/hermes),每丢一个就连同标注
	// 重新 Marshal 测量一次——标注本身也占字节,不重测会把刚裁到边界的卡片
	// 又推回超限(设计 §4.5 第 2 步)。
	omitted := 0
	for i := len(blocks) - 1; i >= 0; i-- {
		if !blocks[i].hasDetail() {
			continue
		}
		blocks[i].clearDetail()
		omitted++

		candidate := card
		candidate.Elements = append(flattenCardBlocks(blocks), cardOmittedNotice(omitted))
		if raw, err := json.Marshal(candidate); err == nil && len(raw) <= cardByteBudget {
			return candidate
		}
	}

	// 详情已丢无可丢,仍然超预算:尽力而为,最终把关在 NotifyCard 活动侧
	// (设计 §5.2 第 3 步,重新测量后不达标直接发降级纯文本)。
	card.Elements = flattenCardBlocks(blocks)
	if omitted > 0 {
		card.Elements = append(card.Elements, cardOmittedNotice(omitted))
	}
	return card
}
