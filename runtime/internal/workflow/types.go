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
}

// WorkflowID 返回确定性的 workflow ID:同一 bundle(或同一变体 kick)重复
// 触发得到同一 ID,由 Temporal 的 ID 唯一性完成天然去重(幂等键思想,§3 规则 7)。
func (in DeviceTestInput) WorkflowID() string {
	id := "device-test-" + in.Project + "-g" + in.Commit + "-p" + strconv.Itoa(in.PipelineID)
	if in.Scope != "" {
		id += "-" + in.Scope
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
