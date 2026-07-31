package activity

import (
	"context"
	"encoding/json"
	"fmt"

	"go.temporal.io/sdk/activity"

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

	displayCard := req.Card
	sendCard := displayCard
	injected := false
	var originalRaw []byte

	// 失败资格只看 header 底色。正文与 fallback 都是可变的展示内容,
	// 不能作为 workflow 身份或失败资格的权威来源。
	eligible := displayCard.Header.Template == "red" || displayCard.Header.Template == "orange"
	if eligible && a.CardActions != nil && a.CardActions.Ready() {
		workflowID := activity.GetInfo(ctx).WorkflowExecution.ID
		if workflowID != "" {
			raw, err := json.Marshal(displayCard)
			if err == nil {
				originalRaw = raw
				if a.Store == nil {
					err = fmt.Errorf("card snapshot store is nil")
				} else {
					err = a.Store.PutCardSnapshot(ctx, workflowID, raw)
				}
			}
			if err != nil {
				// 存不下原卡就不带按钮:发出能点、却无法更新的按钮是更坏的结果。
				a.warnf("card snapshot failed: %v; sending display card without buttons", err)
			} else {
				sendCard = cardWithActions(displayCard, workflowID)
				injected = true
			}
		}
	}

	var raw []byte
	var err error
	if injected || originalRaw == nil {
		raw, err = json.Marshal(sendCard)
	} else {
		raw = originalRaw
	}
	if err == nil && injected && len(raw) > cardSizeBudget {
		// 按钮让卡片超预算时宁可丢按钮,不能丢报告。
		sendCard = displayCard
		raw = originalRaw
		injected = false
	}
	if injected && !a.CardActions.Ready() {
		// 快照持久化和序列化都可能耗时。发送前再次 fail closed,
		// 尽量避免发出到达用户时已经不可用的按钮。
		sendCard = displayCard
		raw = originalRaw
		injected = false
	}
	if err != nil || len(raw) > cardSizeBudget {
		// 超预算不调 SendCard(设计 §5.2 第 3 步):执行路径唯一,可机械断言
		a.warnf("notification card over budget (%d bytes) or unmarshalable; sending text", len(raw))
		return a.sendFallback(ctx, req.FallbackText)
	}
	if err := cs.SendCard(ctx, sendCard); err != nil {
		a.warnf("feishu send card failed: %v; falling back to text", err)
		return a.sendFallback(ctx, req.FallbackText)
	}
	return nil
}

func cardWithActions(displayCard wf.NotificationCard, workflowID string) wf.NotificationCard {
	sendCard := displayCard
	elements := make([]wf.CardElement, len(displayCard.Elements), len(displayCard.Elements)+1)
	copy(elements, displayCard.Elements)
	sendCard.Elements = append(elements, wf.CardElement{
		Tag: "action",
		Actions: []wf.CardButton{
			{
				Tag:  "button",
				Text: wf.CardText{Tag: "plain_text", Content: "重试失败变体"},
				Type: "primary",
				Value: wf.CardActionValue{
					Action:           "retry",
					SourceWorkflowID: workflowID,
				},
			},
			{
				Tag:  "button",
				Text: wf.CardText{Tag: "plain_text", Content: "忽略"},
				Type: "default",
				Value: wf.CardActionValue{
					Action:           "ignore",
					SourceWorkflowID: workflowID,
				},
			},
		},
	})
	return sendCard
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
