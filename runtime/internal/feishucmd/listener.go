package feishucmd

import (
	"context"
	"encoding/json"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// Listener 是飞书长连接(WebSocket)事件接收的最薄封装(不可单测,
// 逻辑全在 Executor):im.message.receive_v1 事件 → 提取 sender open_id
// 与文本 → Executor.HandleMessage。
type Listener struct {
	AppID     string
	AppSecret string
	Exec      *Executor
}

// Run 阻塞运行长连接,直到 ctx 取消(SDK 自带自动重连;
// Start 返回的错误上抛,由调用方记日志)。
func (l *Listener) Run(ctx context.Context) error {
	handler := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(ctx context.Context, ev *larkim.P2MessageReceiveV1) error {
			openID, text := extractMessage(ev)
			if openID == "" {
				return nil // 非用户消息/结构异常:静默忽略
			}
			l.Exec.HandleMessage(ctx, openID, text)
			return nil
		})
	cli := larkws.NewClient(l.AppID, l.AppSecret,
		larkws.WithEventHandler(handler),
		larkws.WithLogLevel(larkcore.LogLevelError),
		larkws.WithAutoReconnect(true),
	)
	return cli.Start(ctx)
}

// extractMessage 从事件中提取发送者 open_id 与消息文本;
// 非文本消息(message_type != "text")返回空文本(→ usage)。
func extractMessage(ev *larkim.P2MessageReceiveV1) (openID, text string) {
	if ev == nil || ev.Event == nil || ev.Event.Sender == nil ||
		ev.Event.Sender.SenderId == nil || ev.Event.Sender.SenderId.OpenId == nil {
		return "", ""
	}
	openID = *ev.Event.Sender.SenderId.OpenId
	msg := ev.Event.Message
	if msg == nil || msg.MessageType == nil || *msg.MessageType != "text" || msg.Content == nil {
		return openID, ""
	}
	var content struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(*msg.Content), &content); err != nil {
		return openID, ""
	}
	return openID, content.Text
}
