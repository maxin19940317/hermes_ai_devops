// Package workflow 定义 DeviceTestWorkflow 及其输入输出类型。
// Phase 1.5 先定义输入契约(Trigger 启动 workflow 用);
// workflow 本体在 Phase 1.6 实现。
package workflow

import "strconv"

// DeviceTestWorkflowName 是跨服务引用的 workflow 类型名。
// Trigger 按名字启动,避免编译期依赖尚未实现的 workflow 函数。
const DeviceTestWorkflowName = "DeviceTestWorkflow"

// PackageRef 对应 bundle.packages[] 一项(contracts/bundle.schema.json)。
type PackageRef struct {
	Variant        string `json:"variant"`
	PackageFile    string `json:"package_file"`
	URL            string `json:"url"`
	SHA256         string `json:"sha256"`
	Size           int64  `json:"size"`
	ManifestDigest string `json:"manifest_digest"`
	// Requirements / FailureSignatures 是业务仓库 variants.yaml 声明的设备
	// 调度约束与失败签名(2026-08-12 解耦:触发端从 bundle/kick 带入,workflow
	// 据此做设备匹配与证据提取,不再查 Runtime 自己的变体配置)。
	// 旧触发载荷(改动前)为空 → SelectTestSpecs 按既有行为降级。
	Requirements      *VariantRequirements `json:"requirements,omitempty"`
	FailureSignatures []VariantSignature   `json:"failure_signatures,omitempty"`
}

// VariantRequirements 是业务仓库 variants.yaml 声明的设备调度约束
// (与 Manifest requirements 同构;bundle/kick 携带,2026-08-12 解耦)。
// 定义在 workflow 包:store 依赖 workflow(store → workflow),反向会循环。
type VariantRequirements struct {
	OS               string   `json:"os"`
	ABI              string   `json:"abi"`
	SOC              []string `json:"soc,omitempty"`
	Capabilities     []string `json:"capabilities,omitempty"`
	MinFreeStorageMB int      `json:"min_free_storage_mb,omitempty"`
}

// VariantSignature 是失败签名(业务仓库 variants.yaml 声明;证据提取与
// 规则归类用,与 Manifest failure_signatures 同构)。
type VariantSignature struct {
	ID       string `json:"id"`
	Where    string `json:"where"`
	Pattern  string `json:"pattern"`
	Classify string `json:"classify"`
}

// DeviceTestInput 是 DeviceTestWorkflow 的启动输入,由 Trigger 从 bundle 派生。
type DeviceTestInput struct {
	Project    string       `json:"project"`
	Commit     string       `json:"commit"`      // short sha(bundle.commit)
	PipelineID int          `json:"pipeline_id"` // CI_PIPELINE_IID
	Version    string       `json:"version"`
	Packages   []PackageRef `json:"packages"`
	// Scope 区分触发粒度:空 = 完整 bundle(pipeline webhook);
	// 变体级触发(CI 直发 /kick,§6.3)时为该变体名,参与 workflow ID 去重。
	Scope string `json:"scope,omitempty"`
	// RuleVersion 路由规则引擎实现(原则 2/差距 #7);空 = 缺省 verdict-rules-v1。
	RuleVersion string `json:"rule_version,omitempty"`
	// Attempt 显式 retry 序号(差距 #11):>0 时 workflow ID 加 -r{N} 后缀,
	// N 取自 artifacts.workflow_attempt 原子递增;0 = 普通触发。
	Attempt int `json:"attempt,omitempty"`
	// SourceWorkflowID 指向触发本次重跑的权威 workflow run;普通触发为空。
	SourceWorkflowID string `json:"source_workflow_id,omitempty"`
}

// RecordWorkflowRunRequest 是 workflow 与持久化活动之间的稳定载荷。
type RecordWorkflowRunRequest struct {
	WorkflowID       string   `json:"workflow_id"`
	Project          string   `json:"project"`
	CommitSHA        string   `json:"commit_sha"`
	PipelineID       int      `json:"pipeline_id"`
	Version          string   `json:"version"`
	RuleVersion      string   `json:"rule_version"`
	Scope            string   `json:"scope"`
	Attempt          int      `json:"attempt"`
	Variants         []string `json:"variants"`
	SourceWorkflowID string   `json:"source_workflow_id,omitempty"`
}

// BaseWorkflowID 是 workflow ID 去掉 scope/attempt 后缀的公共前缀。
// RecentRuns 用它回查 tasks(设计文档 §3.2):格式只此一处定义,Go 侧单一真相来源,
// 不在 SQL 里重复拼接,格式漂移在编译期就不可能发生。
func BaseWorkflowID(project, commit string, pipelineID int) string {
	return "device-test-" + project + "-g" + commit + "-p" + strconv.Itoa(pipelineID)
}

// WorkflowID 返回确定性的 workflow ID:同一 bundle(或同一变体 kick)重复
// 触发得到同一 ID,由 Temporal 的 ID 唯一性完成天然去重(幂等键思想,§3 规则 7);
// 显式 retry(Attempt>0)派生新 ID 起新 run,普通重放永远命中原 ID 被拒绝。
func (in DeviceTestInput) WorkflowID() string {
	id := BaseWorkflowID(in.Project, in.Commit, in.PipelineID)
	if in.Scope != "" {
		id += "-" + in.Scope
	}
	if in.Attempt > 0 {
		id += "-r" + strconv.Itoa(in.Attempt)
	}
	return id
}

// SpecSelection 是 SelectTestSpecs 活动的输出(§12 变体级触发引入 fleet 感知)。
type SpecSelection struct {
	Specs   []TestSpec    `json:"specs"`
	Skipped []SkippedSpec `json:"skipped,omitempty"`
}

// SkippedSpec 是一个被跳过的变体:fleet 中无任何设备满足其 selector
// (秒级结论,不进 acquire 等待),或 OS 尚未接入设备测试链路(Phase 4)。
type SkippedSpec struct {
	Variant string `json:"variant"`
	Reason  string `json:"reason"`
}
