// Package hermesclient 是 hermes-agent 平台(Phase 2 LLM Analyzer)的薄客户端
// 适配层(CLAUDE.md §12)。平台确切请求/响应格式本仓库未知,本包是后续对接真实
// 平台时唯一需要调整的适配点;Client 接口、prompt 管理与输出 Schema 校验保持稳定。
//
// Analyzer 输出必须结构化,经 contracts/analysis.schema.json 校验;校验不过即
// 视为 Analyzer 失败,由调用方回退规则引擎(§9 的 verdict 判定权永远在规则引擎)。
package hermesclient

import (
	"context"
	"encoding/json"
)

// Analysis 与 contracts/analysis.schema.json 字段一一对应,是 Analyzer 的结构化输出。
type Analysis struct {
	AnalysisVersion   int      `json:"analysis_version"` // 契约固定为 1
	Summary           string   `json:"summary"`
	RootCause         string   `json:"root_cause,omitempty"`
	SuggestedCategory string   `json:"suggested_category"` // INFRA/BUILD/CODE/MODEL/DELEGATE/DEVICE/PERF/UNKNOWN
	Confidence        float64  `json:"confidence"`
	NextActions       []string `json:"next_actions,omitempty"`
	DisagreesWithRule bool     `json:"disagrees_with_rule"`
}

// AnalyzeRequest 是一次分析请求的入参。Evidence 为 evidence.json 本体;
// RuleCategory 是规则引擎判定类别(evidence 之外的独立字段,§9);
// Model 可选透传,模型主体由平台配置决定。
type AnalyzeRequest struct {
	TaskID       string
	RuleCategory string
	Model        string
	Evidence     json.RawMessage
}

// Client 是 hermes-agent 平台分析能力的抽象。实现需保证:所有调用尊重 ctx 超时;
// 响应必须通过内嵌 analysis.schema.json 校验,否则返回错误。
type Client interface {
	Analyze(ctx context.Context, req AnalyzeRequest) (*Analysis, error)
}
