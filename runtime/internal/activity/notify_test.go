package activity

import (
	"context"
	"errors"
	"strings"
	"testing"

	"hermes-devops/runtime/internal/feishu"
)

// fakeSender 记录 SendText 调用并按预设失败。
type fakeSender struct {
	texts []string
	err   error
}

func (f *fakeSender) SendText(_ context.Context, text string) error {
	f.texts = append(f.texts, text)
	return f.err
}

// Notify 委托给注入的 feishu.Sender(双模判定在 feishu.NewSender,
// 见该包测试);错误包装后上抛(触发 workflow 降级:只记日志)。
func TestNotifyDelegatesToSender(t *testing.T) {
	fs := &fakeSender{}
	a := &Acts{Feishu: fs}
	if err := a.Notify(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	if len(fs.texts) != 1 || fs.texts[0] != "hello" {
		t.Errorf("texts = %v", fs.texts)
	}

	fs.err = errors.New("feishu api: code 230001: bot not in chat")
	err := a.Notify(ctx, "hi")
	if err == nil || !strings.Contains(err.Error(), "230001") {
		t.Errorf("发送失败应包装错误码上抛, got %v", err)
	}
}

// Sender 为 nil(双模均未配置)时静默成功(开发模式)。
func TestNotifyNilSenderIsSilent(t *testing.T) {
	a := &Acts{}
	if err := a.Notify(ctx, "hello"); err != nil {
		t.Errorf("nil sender 应静默成功: %v", err)
	}
}

// 确保 feishu.Sender 接口满足(编译期断言)。
var _ feishu.Sender = (*fakeSender)(nil)
