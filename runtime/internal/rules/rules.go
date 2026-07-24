// Package rules 是确定性规则引擎(CLAUDE.md §12.6):终态后判定 verdict 与
// error_category(§9),不接 LLM;Phase 2 起作为 Hermes 不可用时的保底裁决。
// 纯函数,无 I/O,可在 workflow 内直接调用(确定性)。
//
// 版本化(docs/device-test-sequence.md 原则 2/差距 #7):Decide 按 rule_version
// 路由;历史版本实现一旦存在历史 workflow 就冻结不得修改,新逻辑只能加新版本
// 分支。未知版本报错而非静默用最新版(重放安全:同一 rule_version 在历史
// 重放与未来执行中必须得到同一裁决)。
package rules

import "fmt"

// VersionV1 是首个规则版本(bundle/rule_version 缺省值,ci/gen_bundle.py 写入)。
const VersionV1 = "verdict-rules-v1"

// DefaultVersion 是 bundle/kick 未携带 rule_version 时的缺省(契约只加不删,
// 旧产物无此字段)。
const DefaultVersion = VersionV1

// ValidateVersion 校验 rule_version 是否为本实现支持的版本。
func ValidateVersion(version string) error {
	if _, err := route(version); err != nil {
		return err
	}
	return nil
}

// Decide 按 version 路由到对应版本的裁决实现;version 为空按 DefaultVersion
// 处理(兼容旧输入)。未知版本返回错误,调用方必须显式处理。
func Decide(version string, in Input) (Decision, error) {
	decide, err := route(version)
	if err != nil {
		return Decision{}, err
	}
	return decide(in), nil
}

func route(version string) (func(Input) Decision, error) {
	if version == "" {
		version = DefaultVersion
	}
	switch version {
	case VersionV1:
		return decideV1, nil
	default:
		return nil, fmt.Errorf("unknown rule_version %q (supported: %s)", version, VersionV1)
	}
}

// Category 错误分类(§9)。
type Category string

const (
	CategoryInfra    Category = "INFRA"
	CategoryBuild    Category = "BUILD"
	CategoryCode     Category = "CODE"
	CategoryModel    Category = "MODEL"
	CategoryDelegate Category = "DELEGATE"
	CategoryDevice   Category = "DEVICE"
	CategoryPerf     Category = "PERF"
	CategoryUnknown  Category = "UNKNOWN"
)

// Verdict 终态判定(§9,与 status 正交)。
type Verdict string

const (
	VerdictPassed       Verdict = "PASSED"
	VerdictTestFailed   Verdict = "TEST_FAILED"
	VerdictPerfRegress  Verdict = "PERF_REGRESSION"
	VerdictInfraError   Verdict = "INFRA_ERROR"
	VerdictInconclusive Verdict = "INCONCLUSIVE"
)

// Input 是裁决输入:来自 result.json 回调与 Runtime 自身观测。
type Input struct {
	Status            string // COMPLETED | FAILED | TIMEOUT | CANCELED
	InfraReason       string // 非空 = Runtime 判定的基础设施故障(租约过期/派单失败等)
	ExitCode          int
	CasesFailed       int
	SignaturesHit     []string            // result.json signatures_hit
	SignatureCategory map[string]Category // 签名 id → 类别(源自 variants.yaml classify)
}

// Decision 是裁决输出。Retry 仅表示"属于可机械重试的类别",次数上限由调用方控制。
// JSON tag 用于 decisions 表 output 列落库(§11 可回放)。
type Decision struct {
	Verdict  Verdict  `json:"verdict"`
	Category Category `json:"category"`
	Reason   string   `json:"reason"`
	Retry    bool     `json:"retry"`
}

// decideV1 按 §9 的优先级裁决(verdict-rules-v1,已冻结:
// 有历史 workflow 后不得修改,行为演进只能新增版本):
// CANCELED → INCONCLUSIVE;Runtime 基础设施故障/Client FAILED → INFRA(可重试);
// 命中签名 → 按 classify(比 TIMEOUT/用例计数更具体,不重试);
// TIMEOUT → INFRA;用例失败/非零退出码 → CODE。
func decideV1(in Input) Decision {
	if in.Status == "CANCELED" {
		return Decision{Verdict: VerdictInconclusive, Category: CategoryUnknown,
			Reason: "task canceled"}
	}
	if in.InfraReason != "" {
		return Decision{Verdict: VerdictInfraError, Category: CategoryInfra,
			Reason: "infra: " + in.InfraReason, Retry: true}
	}
	if in.Status == "FAILED" {
		return Decision{Verdict: VerdictInfraError, Category: CategoryInfra,
			Reason: "client-side pipeline failure", Retry: true}
	}
	if len(in.SignaturesHit) > 0 {
		sig := in.SignaturesHit[0] // 按 Manifest 声明序,首个命中定类别
		cat, ok := in.SignatureCategory[sig]
		if !ok {
			cat = CategoryUnknown
		}
		return Decision{Verdict: VerdictTestFailed, Category: cat,
			Reason: "signature hit: " + sig}
	}
	if in.Status == "TIMEOUT" {
		return Decision{Verdict: VerdictInfraError, Category: CategoryInfra,
			Reason: "test timed out", Retry: true}
	}
	if in.CasesFailed > 0 {
		return Decision{Verdict: VerdictTestFailed, Category: CategoryCode,
			Reason: "test cases failed"}
	}
	if in.ExitCode != 0 {
		return Decision{Verdict: VerdictTestFailed, Category: CategoryCode,
			Reason: "non-zero exit code"}
	}
	return Decision{Verdict: VerdictPassed, Reason: "all criteria met"}
}
