// Package callbacks 实现 Client → Runtime 回调 API(CLAUDE.md §8.2,
// contracts/callbacks-api.openapi.yaml):心跳(设备注册 + 租约所有权条件续租,
// 不发 workflow signal——原则 6,workflow 侧用到期 Timer + CheckLease)、
// 任务事件(按 task_id+seq 去重)、终态结果(Schema 校验 → 单事务 results+outbox
// 去重 → 过渡期 best-effort signal,可靠投递由 Outbox Relay 保证)。
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

	"hermes-devops/runtime/internal/presign"
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
	RenewLease(ctx context.Context, cred store.LeaseCredential, leaseSeconds int) (bool, error)
	AppendTaskEvent(ctx context.Context, ev store.TaskEvent) (bool, error)
	SetTaskStatus(ctx context.Context, taskID, status string) error
	GetTask(ctx context.Context, taskID string) (*wf.TaskRow, error)
	SaveResultWithOutbox(ctx context.Context, rec wf.ResultRecord, ev store.OutboxEvent) (bool, error)
	VerifyLease(ctx context.Context, cred store.LeaseCredential) (bool, error)
}

// Signaler 是 temporal client.Client 的 signal 子集。
type Signaler interface {
	SignalWorkflow(ctx context.Context, workflowID, runID, signalName string, arg interface{}) error
}

type Handler struct {
	store    Store
	signaler Signaler
	log      zerolog.Logger
	leaseSec int

	// Presign 非 nil 时启用按需签发端点(差距 #8);nil = MinIO 未配置,端点返回 503。
	Presign *presign.Signer
	// UploadMaxFiles 是单次请求的文件数上限;<=0 时用 defaultUploadMaxFiles。
	UploadMaxFiles int
}

func New(s Store, sig Signaler, log *zerolog.Logger, leaseSeconds int) *Handler {
	l := zerolog.Nop()
	if log != nil {
		l = *log
	}
	if leaseSeconds <= 0 {
		leaseSeconds = 120 // §10 缺省
	}
	return &Handler{store: s, signaler: sig, log: l, leaseSec: leaseSeconds}
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
	mux.HandleFunc("POST /callbacks/v1/upload-requests", h.uploadRequests)
	return mux
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": msg})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
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
	// ActiveTaskIDs 过渡期双格式(差距 #15):元素为对象 = 携带租约所有权凭据
	// (新格式,执行条件续租);元素为纯字符串 = 旧格式,仅续 client 心跳、
	// 不续租、不报错(旧 Client 滚动升级窗口;下线时点见契约注释)。
	ActiveTaskIDs []json.RawMessage `json:"active_task_ids"`
}

// activeTask 是新格式心跳的任务项:续租必须携带派单时下发的所有权凭据(§10)。
type activeTask struct {
	TaskID     string `json:"task_id"`
	Attempt    int    `json:"attempt"`
	LeaseID    string `json:"lease_id"`
	Generation int    `json:"lease_generation"`
}

// notOwnedEntry 是心跳响应里 LEASE_NOT_OWNED 的逐项报告:
// 凭据失配 = 租约已易主/已释放,Client 必须立即停止操作该任务。
type notOwnedEntry struct {
	TaskID string `json:"task_id"`
	Code   string `json:"code"` // 恒为 LEASE_NOT_OWNED
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
	// 进行中任务 → 条件续租(§10/差距 #15):凭据(lease_id/task/attempt/client/
	// generation)全部匹配且租约未释放才续;失配 → LEASE_NOT_OWNED 逐项返回,
	// Client 必须停止操作该任务(设备可能已租给其他 workflow)。
	// 旧格式(纯字符串)不续租不报错:租约过期后由 AcquireDevice 懒回收。
	// 未知任务(如 Runtime 已重启丢内存)忽略,租约过期由 workflow 的
	// CheckLease → on_infra_error 兜底。不再发任何 workflow signal(原则 6)。
	var notOwned []notOwnedEntry
	for _, raw := range req.ActiveTaskIDs {
		var legacy string
		if err := json.Unmarshal(raw, &legacy); err == nil {
			continue // 旧格式:仅续上面的 client 心跳
		}
		var at activeTask
		if err := json.Unmarshal(raw, &at); err != nil || at.TaskID == "" || at.LeaseID == "" {
			writeErr(w, http.StatusBadRequest, "bad_heartbeat", "invalid active task entry")
			return
		}
		row, err := h.store.GetTask(r.Context(), at.TaskID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		if row == nil || row.DeviceID == "" {
			continue // 未知/未绑设备的任务:忽略(见上注释)
		}
		ok, err := h.store.RenewLease(r.Context(), store.LeaseCredential{
			DeviceID: row.DeviceID, ClientID: req.ClientID, TaskID: at.TaskID,
			Attempt: at.Attempt, LeaseID: at.LeaseID, Generation: at.Generation,
		}, h.leaseSec)
		if err != nil {
			h.log.Error().Err(err).Str("task_id", at.TaskID).Msg("renew lease failed")
			continue
		}
		if !ok {
			notOwned = append(notOwned, notOwnedEntry{TaskID: at.TaskID, Code: "LEASE_NOT_OWNED"})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{"ok": true}
	if len(notOwned) > 0 {
		resp["not_owned"] = notOwned
	}
	_ = json.NewEncoder(w).Encode(resp)
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
	// 事务性 Outbox(docs/device-test-sequence.md 原则 3,差距清单 #1):
	// results + outbox 单事务写入(幂等键 {task_id}:result),消灭"写库成功但
	// signal 失败"的双写窗口;重发回调两侧各自去重,不产生第二行、不重投。
	payload, err := json.Marshal(store.ResultEventPayload{WorkflowID: row.WorkflowID, Result: sig})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	ins, err := h.store.SaveResultWithOutbox(r.Context(), wf.ResultRecord{TaskID: req.TaskID, Result: sig},
		store.OutboxEvent{
			AggregateType: "task", AggregateID: req.TaskID,
			EventType: store.EventTypeTaskResult, EventKey: req.TaskID + ":result",
			Payload: payload,
		})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if ins {
		// 过渡期双通道:Outbox Relay 是可靠投递路径(至少一次,接收端幂等);
		// 这里的直发只为 Relay 部署前保持低延迟,失败不影响收敛(outbox 行
		// 会由 Relay 补投),只记日志不回 5xx——结果已持久化,回调语义是成功。
		// TODO(差距清单 #1 收尾):Relay 全量部署后下线直发,本块删除。
		if err := h.signaler.SignalWorkflow(r.Context(), row.WorkflowID, "",
			wf.SignalTaskResult, sig); err != nil {
			h.log.Error().Err(err).Str("task_id", req.TaskID).Msg("result signal failed (outbox will redeliver)")
		}
	}
	w.WriteHeader(http.StatusOK)
}
