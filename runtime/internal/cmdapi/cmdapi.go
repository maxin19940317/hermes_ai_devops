// Package cmdapi 提供 Hermes/MCP 侧调用的受控命令 HTTP 接口(2026-08-07)。
//
// 设计(CLAUDE.md §3):Hermes 只对 Runtime 说话,输入必须是受 JSON Schema
// 约束的结构化数据。本包把「封闭枚举指令」暴露为 POST /api/v1/cmd:
//
//	{"command": "devices", "args": []}           → 只读查询
//	{"command": "test", "args": ["<variant>"]}   → 触发设备测试(副作用)
//
// 与飞书指令共享同一套执行逻辑(feishucmd.ExecuteCommand),但:
//   - 鉴权用 Bearer Token(CMD_API_TOKEN),不是飞书 open_id 白名单;
//   - 不经过自然语言翻译旁路(输入必须是结构化指令,不允许自由文本);
//   - 不返回卡片,只返回文本(与飞书回复同构,便于 MCP bridge 透传)。
//
// 安全边界:本接口只接受 command.schema.json 的封闭枚举,未知指令一律拒绝;
// 不提供任何任意 shell/ADB 能力(§3.3/§14 红线)。
package cmdapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"

	"hermes-devops/runtime/internal/feishucmd"
)

// Command 是受控命令请求体(command.schema.json 的封闭枚举子集)。
// command 必须落在 feishucmd.Parse 的枚举内;args 语义由具体指令决定
// (test: <variant> [commit]; rerun: <source_workflow_id> [variant] 等)。
type Command struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// Executor 是命令执行器(worker 装配时传入,内部复用 feishucmd.Executor)。
type Executor interface {
	// ExecuteCommand 执行一条封闭枚举指令并返回回复文本(与飞书回复同构)。
	ExecuteCommand(ctx context.Context, cmd feishucmd.Command) (string, error)
}

// Handler 处理 POST /api/v1/cmd。
type Handler struct {
	Token string // Bearer 共享密钥(CMD_API_TOKEN);空 = 未启用(401)
	Exec  Executor
}

// ServeHTTP 实现 http.Handler。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "只接受 POST")
		return
	}
	if h.Token == "" {
		writeErr(w, http.StatusUnauthorized, "not_configured", "CMD_API_TOKEN 未配置")
		return
	}
	// Bearer 鉴权:恒定时间比对,防时序侧信道。
	got := r.Header.Get("Authorization")
	if len(got) <= len("Bearer ") || subtle.ConstantTimeCompare(
		[]byte(got[len("Bearer "):]), []byte(h.Token)) != 1 {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "Bearer token 无效")
		return
	}

	var req Command
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_payload", "请求体不是合法 JSON")
		return
	}
	if req.Command == "" {
		writeErr(w, http.StatusBadRequest, "bad_command", "缺少 command")
		return
	}
	// 封闭枚举:未知命令 → 400(与飞书 Parse 的"未知→help"不同,这里是受控
	// 接口,LLM 不该发未知指令;发 help 也不接受,避免把 usage 当正常输出)。
	if !validCommand(req.Command) {
		writeErr(w, http.StatusBadRequest, "unknown_command", "未知指令: "+req.Command)
		return
	}

	// 提交人身份(2026-08-18):mcp_bridge 按 profile 注入 X-Submitter 头
	// (open_id / profile 名);test/rerun 启动 workflow 时携带,按提交人分发
	// 飞书通知。空 = 无身份来源(CI 等)。
	submitter := r.Header.Get("X-Submitter")
	reply, err := h.Exec.ExecuteCommand(r.Context(), feishucmd.Command{
		Name: req.Command, Args: req.Args, Submitter: submitter,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "execute_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"reply": reply})
}

// validCommand 判定 command 是否在封闭枚举内(与 feishucmd.Parse 同步;
// 不含 help/none——受控接口不接受这两个)。
func validCommand(name string) bool {
	switch name {
	case "status", "devices", "test", "rerun", "unquarantine", "quarantine",
		"runs", "result", "metrics", "artifacts", "cancel":
		return true
	default:
		return false
	}
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"code": code, "message": msg})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
