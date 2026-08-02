package hermesclient

import _ "embed"

// PromptVersionAnalyze 是 Analyzer 当前 prompt 版本号,随请求发送便于平台侧追踪。
const PromptVersionAnalyze = "analyze_v1"

// PromptVersionTranslate 是意图翻译当前 prompt 版本号。
const PromptVersionTranslate = "cmd_translate_v2"

// PromptAnalyze 是编译进二进制的 prompt 文本(prompts/analyze_v1.md)。
// 约束:只依据 evidence 分析、证据不足明说、禁止臆测、只输出符合契约的 JSON。
//
//go:embed prompts/analyze_v1.md
var PromptAnalyze string

// PromptTranslate 是编译进二进制的意图翻译 prompt(prompts/cmd_translate_v2.md)。
// 约束:只输出封闭枚举内的指令、参数只能来自上下文快照、拿不准就返回 none。
//
//go:embed prompts/cmd_translate_v2.md
var PromptTranslate string
