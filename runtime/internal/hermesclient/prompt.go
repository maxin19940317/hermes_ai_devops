package hermesclient

import (
	_ "embed"
	"strings"
)

//go:embed prompts/analyze_v1.md
var PromptAnalyze string

//go:embed prompts/cmd_translate_v3.md
var PromptTranslate string

//go:embed prompts/plan_v1.md
var PromptPlan string

// PromptVersionAnalyze 是 Analyzer 当前 prompt 版本号,随请求发送便于平台侧追踪。
const PromptVersionAnalyze = "analyze_v1"

// PromptVersionTranslate 是意图翻译当前 prompt 版本号。
const PromptVersionTranslate = "cmd_translate_v3"

// PromptVersionPlan 是规划器当前 prompt 版本号。
const PromptVersionPlan = "plan_v1"

// PromptPlanWithVariants 用给定的变体列表渲染 plan prompt 中的 {{variants}} 占位符。
func PromptPlanWithVariants(variants []string) string {
	return strings.Replace(PromptPlan, "{{variants}}", strings.Join(variants, "\n"), 1)
}
