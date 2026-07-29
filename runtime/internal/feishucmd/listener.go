package feishucmd

import (
	"context"
	"encoding/json"
	"sync"
	"time"

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

	dedup *dedupCache
}

// Run 阻塞运行长连接,直到 ctx 取消(SDK 自带自动重连;
// Start 返回的错误上抛,由调用方记日志)。
func (l *Listener) Run(ctx context.Context) error {
	if l.dedup == nil {
		l.dedup = newDedupCache(10*time.Minute, 1000)
	}
	handler := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(_ context.Context, ev *larkim.P2MessageReceiveV1) error {
			openID, text := extractMessage(ev)
			if openID == "" {
				return nil // 非用户消息/结构异常:静默忽略
			}
			// 长连接重投去重(设计 §10):回调内做 LLM 翻译可达数十秒,超出飞书
			// 事件 ack 窗口会被原样重投(实证 2026-07-29:同一消息 17s 后重复执行)。
			if id := messageID(ev); id != "" && !l.dedup.addIfNew(id, time.Now()) {
				return nil
			}
			// 异步处理:回调必须立即返回 ack,慢路径(NL 翻译)在 goroutine 里跑,
			// 用 Run 的生命周期 ctx 而非回调 ctx(回调返回即取消)。
			go l.Exec.HandleMessage(ctx, openID, text)
			return nil
		}).
		// 消息已读回执:无业务用途,注册空操作避免 SDK 刷 "not found handler" 错误日志。
		OnP2MessageReadV1(func(_ context.Context, _ *larkim.P2MessageReadV1) error { return nil })
	cli := larkws.NewClient(l.AppID, l.AppSecret,
		larkws.WithEventHandler(handler),
		larkws.WithLogLevel(larkcore.LogLevelError),
		larkws.WithAutoReconnect(true),
	)
	return cli.Start(ctx)
}

// messageID 提取事件 message_id(去重键);取不到时返回空(调用方跳过去重)。
func messageID(ev *larkim.P2MessageReceiveV1) string {
	if ev == nil || ev.Event == nil || ev.Event.Message == nil || ev.Event.Message.MessageId == nil {
		return ""
	}
	return *ev.Event.Message.MessageId
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

// dedupCache 是 message_id 去重缓存:TTL + 容量上限,惰性清理。
type dedupCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
	ttl  time.Duration
	max  int
}

func newDedupCache(ttl time.Duration, max int) *dedupCache {
	return &dedupCache{seen: map[string]time.Time{}, ttl: ttl, max: max}
}

// addIfNew 首次见到(或已过期)返回 true 并记录;TTL 内的重复返回 false。
func (c *dedupCache) addIfNew(id string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.seen[id]; ok && now.Sub(t) < c.ttl {
		return false
	}
	// 容量管理:先清过期,仍超限则删最旧一条。
	if len(c.seen) >= c.max {
		for k, t := range c.seen {
			if now.Sub(t) >= c.ttl {
				delete(c.seen, k)
			}
		}
		for len(c.seen) >= c.max {
			var oldestK string
			oldestT := now
			for k, t := range c.seen {
				if t.Before(oldestT) {
					oldestK, oldestT = k, t
				}
			}
			delete(c.seen, oldestK)
		}
	}
	c.seen[id] = now
	return true
}
