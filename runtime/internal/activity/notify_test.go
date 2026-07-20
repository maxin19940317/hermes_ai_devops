package activity

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNotifyPostsFeishuText(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = w.Write([]byte(`{"code":0,"msg":"success"}`))
	}))
	defer srv.Close()
	a := &Acts{HTTP: srv.Client(), Cfg: Config{FeishuWebhookURL: srv.URL}}
	if err := a.Notify(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	if got["msg_type"] != "text" || got["content"].(map[string]any)["text"] != "hello" {
		t.Errorf("payload = %v", got)
	}
}

func TestNotifyFeishuBusinessError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":19001,"msg":"param invalid"}`))
	}))
	defer srv.Close()
	a := &Acts{HTTP: srv.Client(), Cfg: Config{FeishuWebhookURL: srv.URL}}
	if err := a.Notify(ctx, "hello"); err == nil || !strings.Contains(err.Error(), "19001") {
		t.Errorf("飞书业务错误码应报错, got %v", err)
	}
}

func TestNotifyNoWebhookConfigured(t *testing.T) {
	a := &Acts{}
	if err := a.Notify(ctx, "hello"); err != nil {
		t.Errorf("未配置 webhook 应静默成功(开发模式): %v", err)
	}
}
