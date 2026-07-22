package hermesclient

import _ "embed"

// PromptVersion 是当前 prompt 版本号,随请求发送便于平台侧追踪。
const PromptVersion = "analyze_v1"

// Prompt 是编译进二进制的 prompt 文本(prompts/analyze_v1.md)。
// 约束:只依据 evidence 分析、证据不足明说、禁止臆测、只输出符合契约的 JSON。
//
//go:embed prompts/analyze_v1.md
var Prompt string
