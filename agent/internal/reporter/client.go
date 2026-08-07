package reporter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 回调端点路径(契约 callbacks-api.openapi.yaml;拼接在 callback base 之后)。
const (
	pathHeartbeat  = "/callbacks/v1/heartbeat"
	pathTaskEvents = "/callbacks/v1/task-events"
	pathResults    = "/callbacks/v1/results"
)

// DefaultTimeout 是单次回调 HTTP 请求的默认超时。
const DefaultTimeout = 10 * time.Second

// maxErrorBody 是错误响应体带入 StatusError 的截断上限。
const maxErrorBody = 512

// StatusError 表示 Runtime 返回的非 2xx 响应。Code 为 HTTP 状态码,
// Body 为截断后的响应体(便于定位 schema_violation 等)。
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("reporter: runtime responded %d: %s", e.Code, e.Body)
}

// Retryable 报告回调错误是否值得重发:网络错误与 5xx(如 signal_error)
// 可重发;4xx(如 schema_violation / unknown_task / 事件不合法)是永久
// 拒绝,重发无意义。
func Retryable(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code >= 500
	}
	return true
}

// DeviceState 是心跳中的设备状态(契约 Device.state)。
type DeviceState string

const (
	DeviceIdle    DeviceState = "IDLE"
	DeviceBusy    DeviceState = "BUSY"
	DeviceOffline DeviceState = "OFFLINE"
)

// DeviceProps 是心跳设备属性(与 executor 预检取同一批 getprop)。
type DeviceProps struct {
	OS           string   `json:"os,omitempty"` // Phase 4: android / linux
	SOC          string   `json:"soc,omitempty"`
	ABI          string   `json:"abi,omitempty"`
	Android      string   `json:"android,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	// MemTotalMB 是设备物理内存总量(MB,来自 /proc/meminfo MemTotal)。
	// 指针:探测失败省略,不谎报 0。
	MemTotalMB *int64 `json:"mem_total_mb,omitempty"`
}

// DeviceInfo 是心跳载荷中的单台设备。WorkdirFreeMB 为指针:
// 探测失败(如设备刚掉线)时省略该字段,而非谎报 0。
type DeviceInfo struct {
	Serial        string       `json:"serial"`
	DisplayName   string       `json:"display_name"`
	State         DeviceState  `json:"state"`
	Props         *DeviceProps `json:"props,omitempty"`
	WorkdirFreeMB *int64       `json:"workdir_free_mb,omitempty"`
}

// ActiveTask 是新格式心跳的任务项(契约 callbacks ActiveTask,差距 #15):
// 携带派单时下发的租约所有权凭据,Runtime 据此条件续租。
type ActiveTask struct {
	TaskID          string `json:"task_id"`
	Attempt         int    `json:"attempt"`
	LeaseID         string `json:"lease_id"`
	LeaseGeneration int    `json:"lease_generation"`
}

// HeartbeatRequest 是 /callbacks/v1/heartbeat 的载荷。
// ActiveTaskIDs 逐任务混合格式(契约 oneOf):元素为 ActiveTask(有凭据)
// 或 string(无凭据的旧任务)——见 Heartbeat.inflight 的兼容推理。
type HeartbeatRequest struct {
	ClientID      string       `json:"client_id"`
	BaseURL       string       `json:"base_url,omitempty"`
	AgentVersion  string       `json:"agent_version"`
	Ts            string       `json:"ts"`
	Devices       []DeviceInfo `json:"devices"`
	ActiveTaskIDs []any        `json:"active_task_ids"`
}

// NotOwnedEntry 是心跳应答里 LEASE_NOT_OWNED 的逐项报告(§10):
// 租约已易主/失效,Agent 必须立即停止操作该任务。
type NotOwnedEntry struct {
	TaskID string `json:"task_id"`
	Code   string `json:"code"` // 恒为 LEASE_NOT_OWNED
}

// HeartbeatAck 是心跳应答。契约保证 ok,not_owned 为租约失配任务清单
// (差距 #15);其余字段(min_agent_version / cancel_task_ids)属 Phase 3,
// 本轮容忍缺失。
type HeartbeatAck struct {
	OK       bool            `json:"ok"`
	NotOwned []NotOwnedEntry `json:"not_owned"`
}

// TaskEvent 是 /callbacks/v1/task-events 的载荷。Seq 单任务内从 1
// 单调递增,与 IdempotencyKey 联合去重。
type TaskEvent struct {
	TaskID         string `json:"task_id"`
	IdempotencyKey string `json:"idempotency_key"`
	Seq            int64  `json:"seq"`
	From           string `json:"from"`
	To             string `json:"to"`
	Ts             string `json:"ts"`
	Detail         string `json:"detail,omitempty"`
}

// ResultReport 是 /callbacks/v1/results 的载荷。Result 为已过
// result.schema.json 校验的 result.json v1 全文。
type ResultReport struct {
	TaskID         string          `json:"task_id"`
	IdempotencyKey string          `json:"idempotency_key"`
	Result         json.RawMessage `json:"result"`
}

// Client 是 Runtime 回调端点的薄 HTTP 客户端:JSON 编码、状态码分类,
// 无重试逻辑(重发策略由 Heartbeat/EventReporter/ResultReporter 各自决定)。
type Client struct {
	BaseURL   string            // Runtime callback base,如 http://host:18091
	HTTP      *http.Client      // nil → http.DefaultClient
	Transport http.RoundTripper // Phase 3 mTLS;非 nil 时注入 http.Client.Transport
	Timeout   time.Duration     // 单次请求超时;0 → DefaultTimeout
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	if c.Transport != nil {
		return &http.Client{Transport: c.Transport, Timeout: c.timeout()}
	}
	return http.DefaultClient
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultTimeout
}

// Heartbeat 上报心跳;out 容忍仅含 ok 的应答。
func (c *Client) Heartbeat(ctx context.Context, req HeartbeatRequest) (*HeartbeatAck, error) {
	var ack HeartbeatAck
	if err := c.post(ctx, pathHeartbeat, req, &ack); err != nil {
		return nil, err
	}
	return &ack, nil
}

// ReportEvent 上报一条状态迁移事件。
func (c *Client) ReportEvent(ctx context.Context, ev TaskEvent) error {
	return c.post(ctx, pathTaskEvents, ev, nil)
}

// ReportResult 上报复核过 Schema 的终态结果。
func (c *Client) ReportResult(ctx context.Context, rep ResultReport) error {
	return c.post(ctx, pathResults, rep, nil)
}

// post 编码 payload 并 POST 到 BaseURL+path;2xx 时按需解码应答,
// 非 2xx 归一为 *StatusError(重发判定用 Retryable)。
func (c *Client) post(ctx context.Context, path string, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("reporter: encode %s payload: %w", path, err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	url := strings.TrimRight(c.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("reporter: build %s request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("reporter: post %s: %w", path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("reporter: read %s response: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &StatusError{Code: resp.StatusCode, Body: truncate(string(respBody), maxErrorBody)}
	}
	if out != nil && len(bytes.TrimSpace(respBody)) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("reporter: decode %s response: %w", path, err)
		}
	}
	return nil
}

// ErrLeaseNotOwned 标记按需签发端点返回 401:租约已非己有(任务易主或已回收)。
// 调用方**不得**回退到派单时的 URL——继续上传会污染别人的证据。
var ErrLeaseNotOwned = errors.New("reporter: lease not owned")

// UploadRequest 是 POST /callbacks/v1/upload-requests 的请求体(差距 #8)。
type UploadRequest struct {
	TaskID          string   `json:"task_id"`
	ClientID        string   `json:"client_id"`
	DeviceID        string   `json:"device_id"`
	Attempt         int      `json:"attempt"`
	LeaseID         string   `json:"lease_id"`
	LeaseGeneration int      `json:"lease_generation"`
	Files           []string `json:"files"`
}

// UploadGrant 是单个已签发条目。
type UploadGrant struct {
	Path      string `json:"path"`
	ObjectKey string `json:"object_key"`
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
}

// UploadRequestResult 是端点响应。Rejected 里是被拒的路径与原因(部分拒绝不是错误)。
type UploadRequestResult struct {
	Uploads  []UploadGrant `json:"uploads"`
	Rejected []struct {
		Path   string `json:"path"`
		Reason string `json:"reason"`
	} `json:"rejected"`
}

// RequestUploads 换取本次实际收集到的文件的预签名 PUT URL(差距 #8)。
// endpoint 是 dispatch 载荷下发的完整 URL(不同于 c.BaseURL 的相对路径拼接,
// upload_request_url 本身就是全 URL)。
// 401 返回 ErrLeaseNotOwned;其余非 2xx 返回普通错误(调用方可回退)。
func (c *Client) RequestUploads(ctx context.Context, endpoint string, req UploadRequest) (*UploadRequestResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("request uploads: encode: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("request uploads: build: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient().Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("request uploads: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrLeaseNotOwned
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		// *StatusError(而非裸 fmt.Errorf)让调用方能用 Retryable 区分
		// "值得重试"(网络错误/5xx)与"确定性失败"(其余 4xx,如 400
		// 载荷/文件数超限):重试后者只是白等 3s。
		return nil, &StatusError{Code: resp.StatusCode, Body: truncate(string(respBody), maxErrorBody)}
	}
	var out UploadRequestResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("request uploads: decode: %w", err)
	}
	return &out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
