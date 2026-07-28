package store

import "context"

// 翻译审计 outcome 取值(设计文档 §4.3)。追加式:确认流程不更新已有行,只追加新行。
const (
	OutcomeExecuted              = "executed"
	OutcomePendingConfirm        = "pending_confirm"
	OutcomeConfirmed             = "confirmed"
	OutcomeDeclined              = "declined"
	OutcomeExpired               = "expired"
	OutcomeRejectedSchema        = "rejected_schema"
	OutcomeRejectedNone          = "rejected_none"
	OutcomeRejectedArgs          = "rejected_args"
	OutcomeRejectedLowConfidence = "rejected_low_confidence"
	OutcomeTranslatorError       = "translator_error"
)

// outputLimit 是 output 列的落库上限(4KB)。Schema 校验失败时平台可能返回任意
// 长度的自由文本,审计要留证但不该让一行把表撑坏(设计文档 §4.3)。
const outputLimit = 4096

// truncatedMark 是被截断输出的尾标记。
const truncatedMark = "...(truncated)"

// CommandTranslation 对应 command_translations 表一行(设计文档 §4.3)。
type CommandTranslation struct {
	OpenID        string
	RawText       string
	PromptVersion string
	Model         string
	ContextDigest string // 上下文快照 sha256,可回放"当时看到了什么"
	Output        []byte // LLM 原始输出(校验失败也存),落库前截断至 outputLimit
	Rendered      string // 渲染出的那行指令
	Outcome       string
}

// truncateOutput 把 output 截断到 outputLimit 并加尾标记;短于上限时原样返回。
func truncateOutput(b []byte) []byte {
	if len(b) <= outputLimit {
		return b
	}
	out := make([]byte, 0, outputLimit+len(truncatedMark))
	out = append(out, b[:outputLimit]...)
	return append(out, truncatedMark...)
}

// SaveCommandTranslation 追加一行翻译审计。
func (s *MemStore) SaveCommandTranslation(_ context.Context, row CommandTranslation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row.Output = truncateOutput(row.Output)
	s.translations = append(s.translations, row)
	return nil
}

// ListCommandTranslations 按时间倒序返回某 open_id 的翻译审计(最新在前)。
func (s *MemStore) ListCommandTranslations(_ context.Context, openID string, limit int) ([]CommandTranslation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []CommandTranslation{}
	for i := len(s.translations) - 1; i >= 0 && len(out) < limit; i-- {
		if s.translations[i].OpenID == openID {
			out = append(out, s.translations[i])
		}
	}
	return out, nil
}
