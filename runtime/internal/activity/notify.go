package activity

import (
	"context"
	"fmt"
)

// Notify 发飞书纯文本(Phase 1,§12.6;交互卡片属后续版本)。
// 双模(feishu 包):企业自建应用机器人优先,群 webhook 兜底;
// Sender 为 nil(均未配置)时静默成功(开发模式)。
func (a *Acts) Notify(ctx context.Context, text string) error {
	if a.Feishu == nil {
		return nil
	}
	if err := a.Feishu.SendText(ctx, text); err != nil {
		return fmt.Errorf("feishu notify: %w", err)
	}
	return nil
}
