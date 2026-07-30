package activity

import (
	"context"
	"encoding/json"
	"fmt"

	"hermes-devops/runtime/internal/feishu"
	wf "hermes-devops/runtime/internal/workflow"
)

// NotifyCard 发终态通知卡片,失败降级纯文本(设计 §5.2 的固定顺序)。
// 降级文本来自载荷(workflow 侧调 buildNotification 生成),activity **绝不自行拼文本**:
// 两处实现同一格式必然漂移,而"降级内容与改动前逐字节相同"是本轮的验收项。
func (a *Acts) NotifyCard(ctx context.Context, req wf.NotifyCardRequest) error {
	if a.Feishu == nil {
		return nil // 未配置:静默成功(开发模式),与 Notify 一致
	}
	cs, ok := a.Feishu.(feishu.CardSender)
	if !ok {
		// 注入的 Sender 不支持卡片(旧测试 fake):直接降级,不是错误
		return a.sendFallback(ctx, req.FallbackText)
	}
	raw, err := json.Marshal(req.Card)
	if err != nil || len(raw) > cardSizeBudget {
		// 超预算不调 SendCard(设计 §5.2 第 3 步):执行路径唯一,可机械断言
		a.warnf("notification card over budget (%d bytes) or unmarshalable; sending text", len(raw))
		return a.sendFallback(ctx, req.FallbackText)
	}
	if err := cs.SendCard(ctx, req.Card); err != nil {
		a.warnf("feishu send card failed: %v; falling back to text", err)
		return a.sendFallback(ctx, req.FallbackText)
	}
	return nil
}

// cardSizeBudget 见设计 §4.5。判据是 `len(raw) > cardSizeBudget` 才降级——
// **恰好等于**预算是允许发卡片的(边界由 cardOfExactSize 的两条用例锁定)。
const cardSizeBudget = 30 * 1024

func (a *Acts) sendFallback(ctx context.Context, text string) error {
	if err := a.Feishu.SendText(ctx, text); err != nil {
		return fmt.Errorf("feishu notify card fallback: %w", err)
	}
	return nil
}
