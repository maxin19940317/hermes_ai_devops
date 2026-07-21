// Package server 实现 Runtime → Client Agent 的 RPC 壳(设计 §3.5,
// 契约 contracts/client-agent-api.openapi.yaml v1):
//
//   - POST /api/v1/tasks:请求体过嵌入 JSON Schema → store 幂等
//     (同幂等键返回既有状态,同 task_id 异键 409)→ 202 入队异步执行;
//   - GET/DELETE /api/v1/tasks/{task_id}:现状查询 / 尽力取消(executor.Cancel);
//   - GET /api/v1/devices:设备清单(与心跳共用 reporter.Prober 探测逻辑);
//   - POST /api/v1/diagnostics:adb_devices|logcat_tail|df|getprop 白名单
//     探测,输出截断——禁止任意 shell(红线 §14);
//   - GET /healthz:存活探针。
package server

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"hermes-devops/agent/internal/adb"
	"hermes-devops/agent/internal/executor"
	"hermes-devops/agent/internal/reporter"
	"hermes-devops/agent/internal/store"
	"hermes-devops/agent/internal/uploader"
)

// tsLayout 是 API 时间戳格式:UTC ISO-8601 毫秒精度(契约 TaskStatus.updated_at)。
const tsLayout = "2006-01-02T15:04:05.000Z"

// maxDispatchBody 是 dispatch 请求体上限(防内存攻击)。
const maxDispatchBody = 1 << 20

// EmbeddedDispatchSchema 是 TaskDispatchRequest 的 JSON Schema,由
// contracts/client-agent-api.openapi.yaml 组件忠实派生;一致性由
// TestEmbeddedSchemaMatchesContract 防漂移(同 manifest 包模式)。
//
//go:embed dispatch.schema.json
var EmbeddedDispatchSchema []byte

var compiledDispatchSchema = mustCompileDispatchSchema()

func mustCompileDispatchSchema() *jsonschema.Schema {
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2020
	if err := c.AddResource("dispatch.schema.json", bytes.NewReader(EmbeddedDispatchSchema)); err != nil {
		panic(fmt.Sprintf("embedded dispatch schema unreadable: %v", err))
	}
	return c.MustCompile("dispatch.schema.json")
}

// ValidateDispatch 用嵌入 Schema 校验 dispatch 请求体原文。
func ValidateDispatch(data []byte) error {
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("decode dispatch json: %w", err)
	}
	if err := compiledDispatchSchema.Validate(doc); err != nil {
		return fmt.Errorf("dispatch schema validation: %w", err)
	}
	return nil
}

// Config 是 Server 的依赖与参数。Store/Runner/Events/Results 必填;
// Uploader 为 nil 时跳过预签名直传(附件缺失不阻断结果上报)。
type Config struct {
	Store    *store.Store
	Runner   adb.Runner // 设备交互(可注入 fake)
	Events   *reporter.EventReporter
	Results  *reporter.ResultReporter
	Uploader *uploader.Uploader

	RunsRoot      string // out_dir = RunsRoot/<task_id>
	AgentVersion  string
	DeviceWorkdir string            // 设备 df 探测路径;空 → reporter.DefaultDeviceWorkdir
	SOCAliases    map[string]string // 平台代号 → SoC 型号(如 trinket→QCM6125)
	HTTP          *http.Client      // executor 下载用;nil → http.DefaultClient

	DiagnosticsMaxBytes int // 诊断输出截断上限;0 → DefaultDiagnosticsMaxBytes

	Logf func(format string, args ...any) // nil → 静默

	// NewExecutor 是测试注入点;nil → 默认构造(Runner/HTTP/Logf 来自 Config)。
	NewExecutor func() *executor.Executor
}

// Server 是契约 v1 的 HTTP 处理器。running 登记进行中任务的 Executor,
// 供 DELETE 取消使用。
type Server struct {
	cfg Config

	mu      sync.Mutex
	running map[string]*executor.Executor
}

// New 构造 Server;Config.Store/Runner/Events/Results 为 nil 会 panic
// (接线错误应尽早暴露)。
func New(cfg Config) *Server {
	if cfg.Store == nil || cfg.Runner == nil || cfg.Events == nil || cfg.Results == nil {
		panic("server: Store/Runner/Events/Results 为必填依赖")
	}
	return &Server{cfg: cfg, running: map[string]*executor.Executor{}}
}

func (s *Server) logf(format string, args ...any) {
	if s.cfg.Logf != nil {
		s.cfg.Logf(format, args...)
	}
}

// Mux 返回契约 v1 的路由(method 模式,同 runtime callbacks handler)。
func (s *Server) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/tasks", s.dispatchTask)
	mux.HandleFunc("GET /api/v1/tasks/{task_id}", s.getTask)
	mux.HandleFunc("DELETE /api/v1/tasks/{task_id}", s.cancelTask)
	mux.HandleFunc("GET /api/v1/devices", s.listDevices)
	mux.HandleFunc("POST /api/v1/diagnostics", s.runDiagnostics)
	mux.HandleFunc("GET /healthz", s.healthz)
	return mux
}

// Error 是契约 Error 组件。
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Error{Code: code, Message: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// TaskStatus 是契约 TaskStatus 组件。
type TaskStatus struct {
	TaskID         string `json:"task_id"`
	IdempotencyKey string `json:"idempotency_key"`
	Attempt        int    `json:"attempt"`
	State          string `json:"state"`
	Detail         string `json:"detail,omitempty"`
	UpdatedAt      string `json:"updated_at"`
}

// taskStatus 由 store 任务 + 最近一条事件组装 TaskStatus
// (updated_at 取最近事件时刻,无事件时退化为 started_at)。
func (s *Server) taskStatus(ctx context.Context, t store.Task) TaskStatus {
	st := TaskStatus{
		TaskID:         t.TaskID,
		IdempotencyKey: t.IdempotencyKey,
		Attempt:        t.Attempt,
		State:          string(t.State),
		UpdatedAt:      t.StartedAt.UTC().Format(tsLayout),
	}
	if ev, ok, err := s.cfg.Store.LatestEvent(ctx, t.TaskID); err == nil && ok {
		st.UpdatedAt = ev.Ts.UTC().Format(tsLayout)
		st.Detail = ev.Detail
	}
	return st
}

// healthz 实现 GET /healthz(契约:status 恒 ok,adb_server_port 恒 5137)。
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "ok",
		"agent_version":   s.cfg.AgentVersion,
		"adb_server_port": adb.DefaultServerPort,
	})
}
