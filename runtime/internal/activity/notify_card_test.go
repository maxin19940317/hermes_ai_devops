package activity

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"hermes-devops/runtime/internal/feishu"
	wf "hermes-devops/runtime/internal/workflow"
)

// cardFake 嵌入 fakeSender(SendText)并加 SendCard,满足 feishu.CardSender。
type cardFake struct {
	fakeSender // 提供 SendText / texts / err
	cards      []any
	failCard   bool
}

func (f *cardFake) SendCard(ctx context.Context, card any) error {
	f.cards = append(f.cards, card)
	if f.failCard {
		return errors.New("card rejected")
	}
	return nil
}

func TestNotifyCardOrder(t *testing.T) {
	big := cardOfExactSize(t, 40*1024)
	small := cardOfExactSize(t, 512)
	cases := []struct {
		name      string
		sender    feishu.Sender // nil 表示未配置飞书
		card      wf.NotificationCard
		wantCards int
		wantTexts int
	}{
		{"nil sender 静默", nil, small, 0, 0},
		{"非 CardSender → 只发文本", &fakeSender{}, small, 0, 1},
		{"超预算 → 只发文本,SendCard 零调用", &cardFake{}, big, 0, 1},
		{"正常 → 只发卡片", &cardFake{}, small, 1, 0},
		{"卡片失败 → 降级发文本", &cardFake{failCard: true}, small, 1, 1},
		// 边界必须精确锁定,否则把 > 写成 >= 不会被发现:
		{"恰好 30*1024 → 发卡片", &cardFake{}, cardOfExactSize(t, 30*1024), 1, 0},
		{"30*1024+1 → 零调用,降级", &cardFake{}, cardOfExactSize(t, 30*1024+1), 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Acts{Feishu: tc.sender}
			if err := a.NotifyCard(ctx, wf.NotifyCardRequest{
				Card: tc.card, FallbackText: "fb"}); err != nil {
				t.Fatalf("这些用例都应成功: %v", err)
			}
			var cards, texts int
			switch f := tc.sender.(type) {
			case *cardFake:
				cards, texts = len(f.cards), len(f.texts)
			case *fakeSender:
				texts = len(f.texts)
			}
			// 断言**精确调用次数**;"超预算"那条的 wantCards=0
			// 是设计 §5.2 第 3 步的机械判据(不是"试了卡片失败再降级")。
			if cards != tc.wantCards || texts != tc.wantTexts {
				t.Errorf("calls card/text = %d/%d, want %d/%d",
					cards, texts, tc.wantCards, tc.wantTexts)
			}
		})
	}
}

// 边界用例靠 cardOfExactSize——只有"正常小卡"和">30KB 大卡"两档时,把 `>` 写成 `>=`
// (或反之)不会有任何测试变红。
//
// marshalCard 是**本文件(activity 包)内**的辅助。
// 不要复用 workflow 测试里的同名 helper——那在另一个包,不可见,会编译失败。
func marshalCard(t *testing.T, c wf.NotificationCard) []byte {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal card: %v", err)
	}
	return raw
}

// cardOfExactSize 造一个 json.Marshal 后**恰好** n 字节的卡片。
// 常数次 Marshal:填充是纯 ASCII 且无需转义,所以每多 1 个字符长度就 +1,
// 先量出骨架长度再一次性补足即可(不要对 n 做线性搜索,那是三万次 Marshal)。
func cardOfExactSize(t *testing.T, n int) wf.NotificationCard {
	t.Helper()
	mk := func(pad int) wf.NotificationCard {
		return wf.NotificationCard{
			Header: wf.CardHeader{
				Title:    wf.CardText{Tag: "plain_text", Content: "x"},
				Template: "green",
			},
			Elements: []wf.CardElement{{Tag: "div", Text: &wf.CardText{
				Tag: "plain_text", Content: strings.Repeat("a", pad)}}},
		}
	}
	base := len(marshalCard(t, mk(0)))
	if n < base {
		t.Fatalf("目标 %d 字节小于骨架 %d 字节", n, base)
	}
	c := mk(n - base)
	if got := len(marshalCard(t, c)); got != n {
		t.Fatalf("造出 %d 字节,想要 %d(填充与长度不是 1:1?)", got, n)
	}
	return c
}

// 降级发送本身失败时,错误必须保留 cause(便于排查是 token 还是网络)。
func TestNotifyCardFallbackFailureWrapsCause(t *testing.T) {
	sentinel := errors.New("boom")
	f := &cardFake{fakeSender: fakeSender{err: sentinel}, failCard: true}
	a := &Acts{Feishu: f}
	err := a.NotifyCard(ctx, wf.NotifyCardRequest{FallbackText: "x"})
	if err == nil {
		t.Fatal("降级也失败时必须返回错误")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("错误应保留 cause, got %v", err)
	}
}

// 降级文本必须原样来自载荷,activity 不得自己拼。
func TestNotifyCardFallbackTextIsVerbatim(t *testing.T) {
	f := &cardFake{failCard: true}
	a := &Acts{Feishu: f}
	const want = "任意文本 —— activity 不该改动它"
	if err := a.NotifyCard(ctx, wf.NotifyCardRequest{FallbackText: want}); err != nil {
		t.Fatal(err)
	}
	if len(f.texts) != 1 || f.texts[0] != want {
		t.Errorf("降级文本 = %v, want 原样 %q", f.texts, want)
	}
}

// TestNotifyCardRoutesBySubmitter:FeishuSenders 命中提交人时用该 sender;
// 未命中/空提交人用默认 Feishu(2026-08-18 按提交人分发)。
func TestNotifyCardRoutesBySubmitter(t *testing.T) {
	small := cardOfExactSize(t, 512)
	def := &cardFake{}
	gene := &cardFake{}
	a := &Acts{
		Feishu: def,
		FeishuSenders: map[string]feishu.Sender{
			"ou_gene": gene,
		},
	}
	// gene 提交 → gene sender 收到,默认 sender 不收
	if err := a.NotifyCard(ctx, wf.NotifyCardRequest{
		Card: small, FallbackText: "fb", Submitter: "ou_gene"}); err != nil {
		t.Fatal(err)
	}
	if len(gene.cards) != 1 || len(def.cards) != 0 {
		t.Errorf("gene 提交: gene=%d def=%d, want gene=1 def=0", len(gene.cards), len(def.cards))
	}
	// 未知提交人 → 回退默认 sender
	if err := a.NotifyCard(ctx, wf.NotifyCardRequest{
		Card: small, FallbackText: "fb", Submitter: "ou_unknown"}); err != nil {
		t.Fatal(err)
	}
	if len(def.cards) != 1 {
		t.Errorf("未知提交人应回退默认 sender, def=%d want 1", len(def.cards))
	}
	// 空提交人(CI 触发)→ 默认 sender
	if err := a.NotifyCard(ctx, wf.NotifyCardRequest{
		Card: small, FallbackText: "fb"}); err != nil {
		t.Fatal(err)
	}
	if len(def.cards) != 2 {
		t.Errorf("空提交人应走默认 sender, def=%d want 2", len(def.cards))
	}
}
