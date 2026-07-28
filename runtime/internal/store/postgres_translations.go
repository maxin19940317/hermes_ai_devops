package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// SaveCommandTranslation 追加一行翻译审计(设计文档 §4.3)。
// output 非合法 JSON 时(平台跑飞返回自由文本)包装成 {"raw": "..."} 存入,
// 保证 JSONB 列可写——审计要留证,不能因为输出是垃圾就丢掉。
func (s *PGStore) SaveCommandTranslation(ctx context.Context, row CommandTranslation) error {
	out := truncateOutput(row.Output)
	if len(out) == 0 || !json.Valid(out) {
		wrapped, err := json.Marshal(map[string]string{"raw": string(out)})
		if err != nil {
			return fmt.Errorf("save command translation: wrap output: %w", err)
		}
		out = wrapped
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO command_translations
		    (open_id, raw_text, prompt_version, model, context_digest, output, rendered, outcome)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		row.OpenID, row.RawText, row.PromptVersion, row.Model, row.ContextDigest,
		string(out), row.Rendered, row.Outcome)
	if err != nil {
		return fmt.Errorf("save command translation: %w", err)
	}
	return nil
}

// ListCommandTranslations 按时间倒序返回某 open_id 的翻译审计(最新在前)。
func (s *PGStore) ListCommandTranslations(ctx context.Context, openID string, limit int) ([]CommandTranslation, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT open_id, raw_text, prompt_version, model, context_digest, output::text, rendered, outcome
		FROM command_translations
		WHERE open_id = $1
		ORDER BY created_at DESC, translation_id DESC
		LIMIT $2`, openID, limit)
	if err != nil {
		return nil, fmt.Errorf("list command translations: %w", err)
	}
	defer rows.Close()
	out := []CommandTranslation{}
	for rows.Next() {
		var r CommandTranslation
		var output string
		if err := rows.Scan(&r.OpenID, &r.RawText, &r.PromptVersion, &r.Model,
			&r.ContextDigest, &output, &r.Rendered, &r.Outcome); err != nil {
			return nil, fmt.Errorf("list command translations: scan: %w", err)
		}
		r.Output = []byte(output)
		out = append(out, r)
	}
	return out, rows.Err()
}
