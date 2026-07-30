package feishu

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

var ctx = context.Background()

// fakeOpenAPI 模拟飞书开放平台:token 端点计数,消息端点按脚本应答。
type fakeOpenAPI struct {
	mu             sync.Mutex
	tokenCalls     int
	messageCalls   int
	messageBodies  []map[string]any
	messageQueries []string
	tokenBodies    []map[string]any
	messageReplies []string // 依次消费;耗尽后默认 {"code":0}
}

func (f *fakeOpenAPI) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/tenant_access_token/internal"):
			f.tokenCalls++
			var b map[string]any
			_ = json.Unmarshal(body, &b)
			f.tokenBodies = append(f.tokenBodies, b)
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"tok-` + json.Number(jsonString(f.tokenCalls)) + `","expire":7200}`))
		case strings.HasSuffix(r.URL.Path, "/im/v1/messages"):
			f.messageCalls++
			f.messageQueries = append(f.messageQueries, r.URL.RawQuery)
			var b map[string]any
			_ = json.Unmarshal(body, &b)
			f.messageBodies = append(f.messageBodies, b)
			reply := `{"code":0,"msg":"ok"}`
			if len(f.messageReplies) > 0 {
				reply = f.messageReplies[0]
				f.messageReplies = f.messageReplies[1:]
			}
			_, _ = w.Write([]byte(reply))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func jsonString(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func (f *fakeOpenAPI) counts() (token, message int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenCalls, f.messageCalls
}

func (f *fakeOpenAPI) lastMessage() (body map[string]any, query string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.messageBodies) == 0 {
		return nil, ""
	}
	return f.messageBodies[len(f.messageBodies)-1], f.messageQueries[len(f.messageQueries)-1]
}

func (f *fakeOpenAPI) lastTokenReq() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tokenBodies) == 0 {
		return nil
	}
	return f.tokenBodies[len(f.tokenBodies)-1]
}

func appCfg(base string) Config {
	return Config{AppID: "cli_a1", AppSecret: "sec", ReceiveID: "oc_chat", BaseURL: base}
}

// 双模判定(表驱动):三件套齐全 → app;缺任一项/仅 webhook → webhook;
// 全空 → disabled(静默)。
func TestNewSenderModeSelection(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"三件套齐全→app", appCfg("http://x"), "app"},
		{"缺 receive_id→webhook", Config{AppID: "a", AppSecret: "s", WebhookURL: "http://hook"}, "webhook"},
		{"缺 secret→webhook", Config{AppID: "a", ReceiveID: "c", WebhookURL: "http://hook"}, "webhook"},
		{"仅 webhook→webhook", Config{WebhookURL: "http://hook"}, "webhook"},
		{"全空→disabled", Config{}, "disabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, mode := NewSender(tc.cfg)
			if mode != tc.want {
				t.Errorf("mode = %q, want %q", mode, tc.want)
			}
			if (s == nil) != (tc.want == "disabled") {
				t.Errorf("sender nil=%v, mode=%q", s == nil, mode)
			}
		})
	}
}

// webhook 模式:载荷与历史行为完全一致(msg_type=text + content.text)。
func TestWebhookSenderPayload(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = w.Write([]byte(`{"code":0,"msg":"success"}`))
	}))
	defer srv.Close()
	s, mode := NewSender(Config{WebhookURL: srv.URL})
	if mode != "webhook" {
		t.Fatalf("mode = %q", mode)
	}
	if err := s.SendText(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	if got["msg_type"] != "text" || got["content"].(map[string]any)["text"] != "hello" {
		t.Errorf("payload = %v", got)
	}
}

// app 模式:首次发送取 token 并缓存;第二次发送复用缓存,不再取 token。
func TestAppSenderTokenCached(t *testing.T) {
	f := &fakeOpenAPI{}
	srv := f.server(t)
	s, mode := NewSender(appCfg(srv.URL))
	if mode != "app" {
		t.Fatalf("mode = %q", mode)
	}
	for i := 0; i < 2; i++ {
		if err := s.SendText(ctx, "hello"); err != nil {
			t.Fatalf("send #%d: %v", i+1, err)
		}
	}
	tok, msg := f.counts()
	if tok != 1 || msg != 2 {
		t.Errorf("token calls = %d, message calls = %d;want 1/2(token 缓存复用)", tok, msg)
	}
	if tr := f.lastTokenReq(); tr["app_id"] != "cli_a1" || tr["app_secret"] != "sec" {
		t.Errorf("token req = %v", tr)
	}
	m, query := f.lastMessage()
	if m["receive_id"] != "oc_chat" || m["msg_type"] != "text" {
		t.Errorf("message = %v", m)
	}
	// ReceiveIDType 缺省回退 chat_id
	if query != "receive_id_type=chat_id" {
		t.Errorf("query = %q, want receive_id_type=chat_id(缺省)", query)
	}
	var content map[string]string
	if err := json.Unmarshal([]byte(m["content"].(string)), &content); err != nil || content["text"] != "hello" {
		t.Errorf("content = %v, err=%v", m["content"], err)
	}
}

// ReceiveIDType=open_id(个人单聊):URL receive_id_type 参数化,body 带 open_id。
func TestAppSenderOpenIDReceiveType(t *testing.T) {
	f := &fakeOpenAPI{}
	srv := f.server(t)
	cfg := appCfg(srv.URL)
	cfg.ReceiveID = "ou_user1"
	cfg.ReceiveIDType = "open_id"
	s, mode := NewSender(cfg)
	if mode != "app" {
		t.Fatalf("mode = %q", mode)
	}
	if err := s.SendText(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	m, query := f.lastMessage()
	if query != "receive_id_type=open_id" {
		t.Errorf("query = %q, want receive_id_type=open_id", query)
	}
	if m["receive_id"] != "ou_user1" {
		t.Errorf("receive_id = %v, want ou_user1", m["receive_id"])
	}
}

// token 过期错误码(99991663)→ 强制刷新重试一次成功:
// token 端点应被调 2 次,消息端点 2 次,最终成功。
func TestAppSenderRefreshesExpiredToken(t *testing.T) {
	f := &fakeOpenAPI{messageReplies: []string{
		`{"code":99991663,"msg":"token expired"}`,
		`{"code":0,"msg":"ok"}`,
	}}
	srv := f.server(t)
	s, _ := NewSender(appCfg(srv.URL))
	if err := s.SendText(ctx, "hello"); err != nil {
		t.Fatalf("过期重试后应成功: %v", err)
	}
	tok, msg := f.counts()
	if tok != 2 || msg != 2 {
		t.Errorf("token calls = %d, message calls = %d;want 2/2(强制刷新重试一次)", tok, msg)
	}
}

// 飞书业务错误(code != 0,非过期码)→ 错误上抛,不重试。
func TestAppSenderBusinessErrorPropagates(t *testing.T) {
	f := &fakeOpenAPI{messageReplies: []string{`{"code":230001,"msg":"bot not in chat"}`}}
	srv := f.server(t)
	s, _ := NewSender(appCfg(srv.URL))
	err := s.SendText(ctx, "hello")
	if err == nil || !strings.Contains(err.Error(), "230001") {
		t.Errorf("业务错误应上抛含错误码, got %v", err)
	}
	_, msg := f.counts()
	if msg != 1 {
		t.Errorf("非过期错误不得重试, message calls = %d", msg)
	}
}

// webhook 模式业务错误同样上抛(行为与历史一致)。
func TestWebhookSenderBusinessError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":19001,"msg":"param invalid"}`))
	}))
	defer srv.Close()
	s, _ := NewSender(Config{WebhookURL: srv.URL})
	if err := s.SendText(ctx, "hello"); err == nil || !strings.Contains(err.Error(), "19001") {
		t.Errorf("got %v", err)
	}
}

// webhook:content 是对象,卡片走顶层 card 字段。
func TestWebhookSendCardWireShape(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()
	s, _ := NewSender(Config{WebhookURL: srv.URL})
	cs, ok := s.(CardSender)
	if !ok {
		t.Fatal("webhookSender 应实现 CardSender")
	}
	if err := cs.SendCard(context.Background(), map[string]any{"header": "x"}); err != nil {
		t.Fatal(err)
	}
	if got["msg_type"] != "interactive" {
		t.Errorf("msg_type = %v, want interactive", got["msg_type"])
	}
	if _, isObj := got["card"].(map[string]any); !isObj {
		t.Errorf("webhook 的 card 应是对象, got %T", got["card"])
	}
}

// app:content 是**序列化后的字符串**(与 SendText 同形),不是对象。
// 复用 feishu_test.go 既有的 fakeOpenAPI 夹具(token + /im/v1/messages 双端点、
// 自带计数与 lastMessage);不自己起裸 httptest server 去凑 token 端点。
func TestAppSendCardWireShape(t *testing.T) {
	f := &fakeOpenAPI{}
	srv := f.server(t)
	s, _ := NewSender(appCfg(srv.URL))
	cs, ok := s.(CardSender)
	if !ok {
		t.Fatal("appSender 应实现 CardSender")
	}
	card := map[string]any{"header": "x"}
	if err := cs.SendCard(ctx, card); err != nil {
		t.Fatal(err)
	}
	body, _ := f.lastMessage()
	if body["msg_type"] != "interactive" {
		t.Errorf("msg_type = %v, want interactive", body["msg_type"])
	}
	str, isStr := body["content"].(string)
	if !isStr {
		t.Fatalf("app 的 content 应是序列化字符串, got %T", body["content"])
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(str), &back); err != nil {
		t.Fatalf("content 不是合法 JSON: %v", err)
	}
	if !reflect.DeepEqual(back, card) {
		t.Errorf("content 解析回来应等于原卡片, got %v", back)
	}
}

// token 过期 → 强制刷新并且只重试一次(与 TestAppSenderRefreshesExpiredToken 同款)。
func TestAppSendCardRefreshesExpiredToken(t *testing.T) {
	f := &fakeOpenAPI{messageReplies: []string{
		`{"code":99991663,"msg":"token expired"}`,
		`{"code":0,"msg":"ok"}`,
	}}
	srv := f.server(t)
	s, _ := NewSender(appCfg(srv.URL))
	cs := s.(CardSender)
	if err := cs.SendCard(ctx, map[string]any{"h": "x"}); err != nil {
		t.Fatalf("过期重试后应成功: %v", err)
	}
	// 两个计数都要断言:只断言 token==2 挡不住"消息被重试很多次"的实现。
	tokenCalls, msgCalls := f.counts()
	if tokenCalls != 2 || msgCalls != 2 {
		t.Fatalf("calls token/message = %d/%d, want 2/2", tokenCalls, msgCalls)
	}
}
