// spike-cardaction — 验证飞书交互卡片按钮的回调能否经 WS 长连接送达。
//
// 背景:oapi-sdk-go v3.9.9 的 ws.WithCardHandler 是注释掉的,且 WS 收到
// MessageTypeCard 帧时直接 return 丢弃。唯一可能可行的路径是:飞书把
// card.action.trigger 当作 type="event" 帧下发,由 EventDispatcher.Do 优先命中
// callbackType2CallbackHandler 并把响应 marshal 回去(ws/client.go 里那句
// `if rsp != nil { // for cardCallback }`)。这一点本地无法证实,只能对真实飞书验证。
//
// 本 spike 回答三个 go/no-go 问题:
//  1. 带按钮的卡片能否发出去(卡片 1.0 的 action 模块);
//  2. 点击后 OnP2CardActionTrigger 是否被调用,action.value 与 operator.open_id 是否完整;
//  3. handler 返回的 toast 能否在点击者屏幕上回显(§10(8) 要求的同步 toast)。
//
// 用法:
//
//	export FEISHU_APP_ID=... FEISHU_APP_SECRET=... FEISHU_RECEIVE_ID=...
//	# 群聊留空 FEISHU_RECEIVE_ID_TYPE;个人单聊设 open_id
//	go run ./cmd/spike-cardaction
//
// 然后在飞书里点那两个按钮,观察本进程 stdout 与客户端 toast。Ctrl-C 结束。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"hermes-devops/runtime/internal/feishu"
)

// spikeCard 是卡片 1.0 形态:与已上线的 NotificationCard 同构
// (config/header/elements),只多一个 tag=action 的元素。选 1.0 而非 2.0 是为了
// 让 spike 的结论能直接迁移到现有卡片——本轮按钮是往它上面加,不是重写。
func spikeCard(workflowID, variant string) map[string]any {
	button := func(text, action, style string) map[string]any {
		return map[string]any{
			"tag":  "button",
			"text": map[string]any{"tag": "plain_text", "content": text},
			"type": style,
			// value 是点击后原样回传的载荷(CallBackAction.Value)。
			// 真实设计里这里只放 source_workflow_id + variant + action,
			// 其余一律从权威记录派生(§10(4))。
			"value": map[string]any{
				"action":             action,
				"source_workflow_id": workflowID,
				"variant":            variant,
			},
		}
	}
	return map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": "[spike] card.action.trigger 可达性验证"},
			"template": "red",
		},
		"elements": []any{
			map[string]any{
				"tag":  "div",
				"text": map[string]any{"tag": "plain_text", "content": variant + "  TEST_FAILED(CODE)"},
			},
			map[string]any{"tag": "hr"},
			map[string]any{
				"tag": "action",
				"actions": []any{
					button("重试该变体", "retry", "primary"),
					button("忽略", "ignore", "default"),
				},
			},
		},
	}
}

func main() {
	appID := os.Getenv("FEISHU_APP_ID")
	appSecret := os.Getenv("FEISHU_APP_SECRET")
	receiveID := os.Getenv("FEISHU_RECEIVE_ID")
	if appID == "" || appSecret == "" || receiveID == "" {
		log.Fatal("需要 FEISHU_APP_ID / FEISHU_APP_SECRET / FEISHU_RECEIVE_ID")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 1. 先起监听,再发卡片——反过来会漏掉手快的点击。
	hits := make(chan struct{}, 8)
	handler := dispatcher.NewEventDispatcher("", "").
		OnP2CardActionTrigger(func(_ context.Context, ev *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
			raw, _ := json.Marshal(ev.Event)
			fmt.Printf("\n✅ 收到 card.action.trigger\n   载荷: %s\n", raw)
			if ev.Event != nil {
				if op := ev.Event.Operator; op != nil {
					fmt.Printf("   operator.open_id = %q\n", op.OpenID)
				}
				if act := ev.Event.Action; act != nil {
					fmt.Printf("   action.value     = %#v\n", act.Value)
				}
				fmt.Printf("   token(更新卡片用) = %q\n", ev.Event.Token)
			}
			select {
			case hits <- struct{}{}:
			default:
			}
			// 同步 toast:§10(8) 要求非白名单提示必须走这条路,不能用 SendText。
			return &callback.CardActionTriggerResponse{
				Toast: &callback.Toast{Type: "info", Content: "spike: 回调已送达 Runtime"},
			}, nil
		}).
		// 注册消息事件只为证明"长连接本身是通的",便于区分
		// "连接没建起来" 与 "连接通了但卡片回调不走这条通道"。
		OnP2MessageReceiveV1(func(_ context.Context, _ *larkim.P2MessageReceiveV1) error {
			fmt.Println("（收到一条普通消息事件 → 长连接本身是通的）")
			return nil
		})

	cli := larkws.NewClient(appID, appSecret,
		larkws.WithEventHandler(handler),
		larkws.WithLogLevel(larkcore.LogLevelInfo),
		larkws.WithAutoReconnect(true),
	)
	go func() {
		if err := cli.Start(ctx); err != nil && ctx.Err() == nil {
			log.Printf("长连接退出: %v", err)
		}
	}()
	time.Sleep(3 * time.Second) // 等连接建立

	// 2. 发一张带按钮的卡片。
	sender, mode := feishu.NewSender(feishu.Config{
		AppID: appID, AppSecret: appSecret,
		ReceiveID: receiveID, ReceiveIDType: os.Getenv("FEISHU_RECEIVE_ID_TYPE"),
	})
	if mode != "app" {
		log.Fatalf("需要 app 模式(当前 %q):按钮回调只有企业自建应用能收", mode)
	}
	cs, ok := sender.(feishu.CardSender)
	if !ok {
		log.Fatal("sender 不支持卡片")
	}
	card := spikeCard("device-test-grp/p-gabcd1234-p42-v1", "aarch64_Android_SNPE_2.21")
	if err := cs.SendCard(ctx, card); err != nil {
		log.Fatalf("发送卡片失败(→ 按钮卡片本身发不出去,no-go): %v", err)
	}
	fmt.Println("卡片已发送。请在飞书里点击按钮,并观察:")
	fmt.Println("  (a) 本进程是否打印 ✅ 收到 card.action.trigger")
	fmt.Println("  (b) 飞书客户端是否弹出 toast「spike: 回调已送达 Runtime」")
	fmt.Println("Ctrl-C 结束。")

	select {
	case <-hits:
		fmt.Println("\n→ go:回调经 WS 送达,可按 WS 方案设计。")
		fmt.Println("  仍请确认 toast 是否回显(决定同步提示能否用)。")
		<-ctx.Done()
	case <-ctx.Done():
	case <-time.After(10 * time.Minute):
		fmt.Println("\n→ 10 分钟内未收到回调。若期间确实点过按钮,视为 no-go:")
		fmt.Println("  WS 不投递 card.action.trigger,需改用公网 HTTPS 回调端点。")
	}
}
