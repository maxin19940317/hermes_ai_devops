package activity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Notify 发飞书自定义机器人纯文本(Phase 1,§12.6;交互卡片属 Phase 2)。
// 未配置 webhook 时静默成功(开发模式)。
func (a *Acts) Notify(ctx context.Context, text string) error {
	if a.Cfg.FeishuWebhookURL == "" {
		return nil
	}
	body, err := json.Marshal(map[string]any{
		"msg_type": "text",
		"content":  map[string]string{"text": text},
	})
	if err != nil {
		return err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Cfg.FeishuWebhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := a.HTTP.Do(hr)
	if err != nil {
		return fmt.Errorf("feishu notify: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("feishu notify: status %d", resp.StatusCode)
	}
	var ack struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		return fmt.Errorf("feishu notify: decode ack: %w", err)
	}
	if ack.Code != 0 {
		return fmt.Errorf("feishu notify: code %d: %s", ack.Code, ack.Msg)
	}
	return nil
}
