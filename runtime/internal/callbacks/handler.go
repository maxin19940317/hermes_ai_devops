// Package callbacks 实现 Client → Runtime 回调 API(CLAUDE.md §8.2,
// contracts/callbacks-api.openapi.yaml):心跳(设备注册 + 租约续期 signal)、
// 任务事件(按 task_id+seq 去重)、终态结果(Schema 校验 → SaveResult 去重 → signal)。
package callbacks

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/rs/zerolog"
	"github.com/santhosh-tekuri/jsonschema/v5"

	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

//go:embed result.schema.json
var resultSchemaJSON string

var resultSchema = mustCompileResultSchema()

func mustCompileResultSchema() *jsonschema.Schema {
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2020
	if err := c.AddResource("result.schema.json", strings.NewReader(resultSchemaJSON)); err != nil {
		panic(err)
	}
	return c.MustCompile("result.schema.json")
}

// Store 是回调服务依赖的持久层子集。
type Store interface {
	UpsertClientDevices(ctx context.Context, c store.Client, devs []store.Device) error
	AppendTaskEvent(ctx context.Context, ev store.TaskEvent) (bool, error)
	SetTaskStatus(ctx context.Context, taskID, status string) error
	GetTask(ctx context.Context, taskID string) (*wf.TaskRow, error)
	SaveResult(ctx context.Context, rec wf.ResultRecord) (bool, error)
}

// Signaler 是 temporal client.Client 的 signal 子集。
type Signaler interface {
	SignalWorkflow(ctx context.Context, workflowID, runID, signalName string, arg interface{}) error
}

type Handler struct {
	store    Store
	signaler Signaler
	log      zerolog.Logger
}

func New(s Store, sig Signaler, log *zerolog.Logger) *Handler {
	l := zerolog.Nop()
	if log != nil {
		l = *log
	}
	return &Handler{store: s, signaler: sig, log: l}
}

func (h *Handler) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /callbacks/v1/heartbeat", h.heartbeat)
	mux.HandleFunc("POST /callbacks/v1/task-events", h.taskEvent)
	mux.HandleFunc("POST /callbacks/v1/results", h.result)
	return mux
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": msg})
}

// ---- heartbeat ----

type heartbeatReq struct {
	ClientID     string `json:"client_id"`
	AgentVersion string `json:"agent_version"`
	BaseURL      string `json:"base_url"` // 契约新增可选字段(见 openapi CONTRACT-ISSUE)
	Devices      []struct {
		Serial string `json:"serial"`
		State  string `json:"state"`
		Props  struct {
			SOC          string   `json:"soc"`
			ABI          string   `json:"abi"`
			Capabilities []string `json:"capabilities"`
		} `json:"props"`
	} `json:"devices"`
	ActiveTaskIDs []string `json:"active_task_ids"`
}

func (h *Handler) heartbeat(w http.ResponseWriter, r *http.Request) {
	var req heartbeatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ClientID == "" {
		writeErr(w, http.StatusBadRequest, "bad_heartbeat", "invalid heartbeat payload")
		return
	}
	devs := make([]store.Device, 0, len(req.Devices))
	for _, d := range req.Devices {
		devs = append(devs, store.Device{
			DeviceID: d.Serial, Serial: d.Serial, ClientID: req.ClientID,
			SOC: d.Props.SOC, ABI: d.Props.ABI, Capabilities: d.Props.Capabilities,
		})
	}
	if err := h.store.UpsertClientDevices(r.Context(), store.Client{
		ClientID: req.ClientID, Version: req.AgentVersion, BaseURL: req.BaseURL,
	}, devs); err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	// 进行中任务 → 续租 signal(§8.2);未知任务(如 Runtime 已重启丢内存)忽略,
	// 租约过期由 workflow 的 on_infra_error 兜底
	for _, tid := range req.ActiveTaskIDs {
		row, err := h.store.GetTask(r.Context(), tid)
		if err != nil || row == nil {
			continue
		}
		if err := h.signaler.SignalWorkflow(r.Context(), row.WorkflowID, "",
			wf.SignalTaskHeartbeat, wf.TaskHeartbeat{TaskID: tid}); err != nil {
			h.log.Error().Err(err).Str("task_id", tid).Msg("heartbeat signal failed")
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// ---- task-events ----

type taskEventReq struct {
	TaskID         string `json:"task_id"`
	IdempotencyKey string `json:"idempotency_key"`
	Seq            int    `json:"seq"`
	From           string `json:"from"`
	To             string `json:"to"`
	TS             string `json:"ts"`
	Detail         string `json:"detail"`
}

func (h *Handler) taskEvent(w http.ResponseWriter, r *http.Request) {
	var ev taskEventReq
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil || ev.TaskID == "" || ev.Seq < 1 || ev.To == "" {
		writeErr(w, http.StatusBadRequest, "bad_event", "invalid task event")
		return
	}
	ins, err := h.store.AppendTaskEvent(r.Context(), store.TaskEvent{
		TaskID: ev.TaskID, Seq: ev.Seq, From: ev.From, To: ev.To,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if ins { // 重发事件去重后无副作用(§8.2)
		if err := h.store.SetTaskStatus(r.Context(), ev.TaskID, ev.To); err != nil {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

// ---- results ----

type resultReq struct {
	TaskID         string          `json:"task_id"`
	IdempotencyKey string          `json:"idempotency_key"`
	Result         json.RawMessage `json:"result"`
}

// resultDoc 是 result.json v1 中 Runtime 消费的字段子集(校验后解析)。
type resultDoc struct {
	Status      string  `json:"status"`
	ExitCode    int     `json:"exit_code"`
	DurationSec float64 `json:"duration_sec"`
	Cases       struct {
		Total  int `json:"total"`
		Failed int `json:"failed"`
	} `json:"cases"`
	SignaturesHit []string           `json:"signatures_hit"`
	Metrics       map[string]float64 `json:"metrics"`
	Attachments   []wf.Attachment    `json:"attachments"`
}

func (h *Handler) result(w http.ResponseWriter, r *http.Request) {
	var req resultReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TaskID == "" || len(req.Result) == 0 {
		writeErr(w, http.StatusBadRequest, "bad_result", "invalid result report")
		return
	}
	// 红线 §14:未经 Schema 校验不消费 result.json
	var doc any
	if err := json.Unmarshal(req.Result, &doc); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_result", "result is not JSON")
		return
	}
	if err := resultSchema.Validate(doc); err != nil {
		writeErr(w, http.StatusBadRequest, "schema_violation", fmt.Sprintf("result.schema.json: %v", err))
		return
	}
	row, err := h.store.GetTask(r.Context(), req.TaskID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if row == nil {
		writeErr(w, http.StatusBadRequest, "unknown_task", "no such task: "+req.TaskID)
		return
	}
	var parsed resultDoc
	if err := json.Unmarshal(req.Result, &parsed); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_result", err.Error())
		return
	}
	sig := wf.TaskResultSignal{
		TaskID: req.TaskID, Status: parsed.Status, ExitCode: parsed.ExitCode,
		DurationSec: parsed.DurationSec, CasesTotal: parsed.Cases.Total,
		CasesFailed: parsed.Cases.Failed, SignaturesHit: parsed.SignaturesHit,
		Metrics: parsed.Metrics, Attachments: parsed.Attachments,
	}
	// 先落库去重再 signal:重发不重投("signal 只投递一次",§8.2)。
	// 落库成功但 signal 失败的窗口由租约过期 → on_infra_error 兜底收敛。
	ins, err := h.store.SaveResult(r.Context(), wf.ResultRecord{TaskID: req.TaskID, Result: sig})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if ins {
		if err := h.signaler.SignalWorkflow(r.Context(), row.WorkflowID, "",
			wf.SignalTaskResult, sig); err != nil {
			h.log.Error().Err(err).Str("task_id", req.TaskID).Msg("result signal failed")
			writeErr(w, http.StatusInternalServerError, "signal_error", err.Error())
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}
