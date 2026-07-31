// Package feishu 实现飞书通知发送(双模):
// 企业自建应用机器人(app_id/app_secret/chat_id 三件套齐全时优先,
// tenant_access_token 缓存 + 过期强制刷新重试一次);
// 群自定义机器人 webhook(未配置应用凭据时兜底,行为与历史完全一致)。
// 纯文本走 Sender.SendText;交互卡片走可选的 CardSender.SendCard
// (两种模式各自实现,wire 形态不对称,见 CardSender 注释)。
package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// defaultBaseURL 是飞书开放平台地址;测试经 Config.BaseURL 指向 fake。
const defaultBaseURL = "https://open.feishu.cn"

// tokenRefreshMargin 是 token 缓存的提前刷新余量(expire 7200s,提前 5min)。
const tokenRefreshMargin = 5 * time.Minute

// tokenExpiredCodes 是 tenant_access_token 过期/无效的业务错误码,
// 命中后强制刷新 token 并重试一次。
var tokenExpiredCodes = map[int]bool{99991663: true, 99991661: true}

// Sender 发送纯文本消息。
type Sender interface {
	SendText(ctx context.Context, text string) error
}

// CardSender 是能发交互卡片的 Sender(终态通知卡片化)。
// 单独成接口而非往 Sender 上加方法:后者会让 activity/notify_test.go 与
// feishucmd/executor_test.go 里只实现 SendText 的既有 fake 直接编译失败。
type CardSender interface {
	Sender
	SendCard(ctx context.Context, card any) error
}

// CardUpdater 是能更新已发送卡片消息的发送方能力。
// 该能力仅适用于企业自建应用机器人;群自定义机器人不支持更新消息。
type CardUpdater interface {
	PatchCard(ctx context.Context, messageID string, card any) error
}

// Config 是发送方配置;Mode 由 NewSender 按凭据齐全度判定。
type Config struct {
	AppID     string // 企业自建应用 app_id(三件套齐全才启用 app 模式)
	AppSecret string
	ReceiveID string // 接收方 id:open_id(个人单聊)或 chat_id(群)
	// ReceiveIDType 对应 receive_id_type:chat_id|open_id;空 → 缺省 "chat_id"。
	// 不校验合法性,原样发送,非法值由飞书侧报错上抛(简单优先)。
	ReceiveIDType string
	WebhookURL    string // 群自定义机器人 webhook(兜底/开发模式)
	BaseURL       string // 开放平台地址;空 → defaultBaseURL(测试注入 fake)
	HTTP          *http.Client
	Timeout       time.Duration // 单次请求超时;0 → 10s
}

// NewSender 按凭据构造发送方并报告所选模式:
// 三件套(AppID/AppSecret/ReceiveID)齐全 → "app";否则有 webhook → "webhook"
// (应用凭据缺任一项即回退);都没有 → (nil, "disabled"),调用方按静默成功处理(开发模式)。
func NewSender(c Config) (Sender, string) {
	if c.AppID != "" && c.AppSecret != "" && c.ReceiveID != "" {
		return newAppSender(c), "app"
	}
	if c.WebhookURL != "" {
		return &webhookSender{cfg: c}, "webhook"
	}
	return nil, "disabled"
}

func (c Config) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (c Config) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 10 * time.Second
}

// apiError 是飞书业务错误(code != 0)。
type apiError struct {
	Code int
	Msg  string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("feishu api: code %d: %s", e.Code, e.Msg)
}

func (e *apiError) businessErrorCode() int {
	return e.Code
}

type httpStatusError struct {
	operation string
	status    int
	body      string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("feishu: %s: status %d: %s", e.operation, e.status, e.body)
}

func (e *httpStatusError) httpErrorStatus() int {
	return e.status
}

// BusinessErrorCode returns the Feishu business code carried by err.
func BusinessErrorCode(err error) (int, bool) {
	var coded interface{ businessErrorCode() int }
	if !errors.As(err, &coded) {
		return 0, false
	}
	return coded.businessErrorCode(), true
}

// HTTPErrorStatus returns the HTTP status carried by a Feishu transport error.
func HTTPErrorStatus(err error) (int, bool) {
	var status interface{ httpErrorStatus() int }
	if !errors.As(err, &status) {
		return 0, false
	}
	return status.httpErrorStatus(), true
}

func isTokenExpired(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	return tokenExpiredCodes[ae.Code]
}

// post 发 JSON POST 并解析飞书应答;code != 0 归一为 *apiError。
func post(ctx context.Context, c Config, url string, headers map[string]string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("feishu: encode payload: %w", err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("feishu: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("feishu: post %s: %w", url, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("feishu: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("feishu: post %s: status %d: %s", url, resp.StatusCode, truncate(string(respBody), 256))
	}
	var ack struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if len(bytes.TrimSpace(respBody)) > 0 {
		if err := json.Unmarshal(respBody, &ack); err != nil {
			return fmt.Errorf("feishu: decode ack: %w", err)
		}
	}
	if ack.Code != 0 {
		return &apiError{Code: ack.Code, Msg: ack.Msg}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---- webhook 模式(群自定义机器人;行为与历史 notify.go 完全一致) ----

type webhookSender struct {
	cfg Config
}

func (s *webhookSender) SendText(ctx context.Context, text string) error {
	return post(ctx, s.cfg, s.cfg.WebhookURL, nil, map[string]any{
		"msg_type": "text",
		"content":  map[string]string{"text": text},
	})
}

// SendCard 发交互卡片。webhook 自定义机器人的卡片走顶层 card 字段。
func (s *webhookSender) SendCard(ctx context.Context, card any) error {
	return post(ctx, s.cfg, s.cfg.WebhookURL, nil, map[string]any{
		"msg_type": "interactive",
		"card":     card,
	})
}

// ---- app 模式(企业自建应用机器人) ----

type appSender struct {
	cfg    Config
	base   string
	mu     sync.Mutex
	token  string
	expire time.Time // 缓存 token 的过期时刻(已含刷新余量判定)
}

func newAppSender(c Config) *appSender {
	base := c.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	return &appSender{cfg: c, base: base}
}

// SendText 发纯文本;token 过期错误码 → 强制刷新重试一次。
func (s *appSender) SendText(ctx context.Context, text string) error {
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return fmt.Errorf("feishu: encode message content: %w", err)
	}
	return s.send(ctx, "text", string(content))
}

// SendCard 发交互卡片;content 与 SendText 同形——序列化后的字符串,
// 而非对象(app 消息端点对所有 msg_type 的 content 字段一律要求字符串)。
func (s *appSender) SendCard(ctx context.Context, card any) error {
	content, err := json.Marshal(card)
	if err != nil {
		return fmt.Errorf("feishu: encode card content: %w", err)
	}
	return s.send(ctx, "interactive", string(content))
}

// PatchCard 更新应用机器人已发送的卡片消息。
func (s *appSender) PatchCard(ctx context.Context, messageID string, card any) error {
	if strings.TrimSpace(messageID) == "" {
		return fmt.Errorf("feishu: message ID is required")
	}
	content, err := json.Marshal(card)
	if err != nil {
		return fmt.Errorf("feishu: encode patch card content: %w", err)
	}
	return s.patch(ctx, messageID, string(content))
}

// send 是 SendText/SendCard 共用的发送逻辑:取 token(缓存)→ 发消息;
// token 过期错误码 → 强制刷新重试一次。
func (s *appSender) send(ctx context.Context, msgType, content string) error {
	tok, err := s.tenantToken(ctx, false)
	if err != nil {
		return err
	}
	if err := s.sendMessage(ctx, tok, msgType, content); err != nil {
		if !isTokenExpired(err) {
			return err
		}
		// token 提前失效(服务端回收/时钟漂移):强制刷新重试一次
		tok, err = s.tenantToken(ctx, true)
		if err != nil {
			return err
		}
		return s.sendMessage(ctx, tok, msgType, content)
	}
	return nil
}

// patch 复用 send 的 token 缓存与过期重试策略更新一条卡片消息。
func (s *appSender) patch(ctx context.Context, messageID, content string) error {
	tok, err := s.tenantToken(ctx, false)
	if err != nil {
		return err
	}
	if err := s.patchMessage(ctx, tok, messageID, content); err != nil {
		if !isTokenExpired(err) {
			return err
		}
		tok, err = s.tenantToken(ctx, true)
		if err != nil {
			return err
		}
		return s.patchMessage(ctx, tok, messageID, content)
	}
	return nil
}

// tenantToken 取 tenant_access_token:缓存未过期(含 5min 余量)且非强制
// 刷新时直接复用;否则重新获取并缓存。
func (s *appSender) tenantToken(ctx context.Context, force bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !force && s.token != "" && time.Now().Add(tokenRefreshMargin).Before(s.expire) {
		return s.token, nil
	}
	// 复用通用 post 会丢 token 字段,这里单独解析
	var ack struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	body, err := json.Marshal(map[string]string{
		"app_id": s.cfg.AppID, "app_secret": s.cfg.AppSecret,
	})
	if err != nil {
		return "", fmt.Errorf("feishu: encode token payload: %w", err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, s.cfg.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		s.base+"/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("feishu: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.cfg.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("feishu: fetch tenant token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("feishu: fetch tenant token: status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		return "", fmt.Errorf("feishu: decode token ack: %w", err)
	}
	if ack.Code != 0 {
		return "", &apiError{Code: ack.Code, Msg: ack.Msg}
	}
	if ack.TenantAccessToken == "" || ack.Expire <= 0 {
		return "", fmt.Errorf("feishu: token ack missing token/expire")
	}
	s.token = ack.TenantAccessToken
	s.expire = time.Now().Add(time.Duration(ack.Expire) * time.Second)
	return s.token, nil
}

// sendMessage 向 im/v1/messages 发送已序列化的 content(text/interactive 通用)。
func (s *appSender) sendMessage(ctx context.Context, token, msgType, content string) error {
	idType := s.cfg.ReceiveIDType
	if idType == "" {
		idType = "chat_id" // 缺省群聊;个人单聊配 open_id
	}
	return post(ctx, s.cfg,
		s.base+"/open-apis/im/v1/messages?receive_id_type="+url.QueryEscape(idType),
		map[string]string{"Authorization": "Bearer " + token},
		map[string]any{
			"receive_id": s.cfg.ReceiveID,
			"msg_type":   msgType,
			"content":    content,
		})
}

// patchMessage 向 im/v1/messages/{message_id} 发送已序列化的完整卡片。
func (s *appSender) patchMessage(ctx context.Context, token, messageID, content string) error {
	body, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		return fmt.Errorf("feishu: encode patch payload: %w", err)
	}
	endpoint := s.base + "/open-apis/im/v1/messages/" + url.PathEscape(messageID)
	reqCtx, cancel := context.WithTimeout(ctx, s.cfg.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPatch, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("feishu: build patch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.cfg.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("feishu: patch %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("feishu: read patch response: %w", err)
	}
	var ack struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	var decodeErr error
	if len(bytes.TrimSpace(respBody)) > 0 {
		decodeErr = json.Unmarshal(respBody, &ack)
		if decodeErr == nil && ack.Code != 0 &&
			resp.StatusCode < http.StatusInternalServerError &&
			resp.StatusCode != http.StatusTooManyRequests {
			return &apiError{Code: ack.Code, Msg: ack.Msg}
		}
	}
	if resp.StatusCode != http.StatusOK {
		return &httpStatusError{
			operation: "patch " + endpoint,
			status:    resp.StatusCode,
			body:      truncate(string(respBody), 256),
		}
	}
	if decodeErr != nil {
		return fmt.Errorf("feishu: decode patch ack: %w", decodeErr)
	}
	return nil
}
