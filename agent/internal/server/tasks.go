package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"hermes-devops/agent/internal/artifact"
	"hermes-devops/agent/internal/executor"
	"hermes-devops/agent/internal/reporter"
	"hermes-devops/agent/internal/store"
	"hermes-devops/agent/internal/uploader"
)

// safeOutDirName 把 task_id 净化为单级安全目录名:仅保留
// [A-Za-z0-9._-],其余字符(含 '/' ':' '\')一律替换为 '_';
// 结果为空、"." 或 ".." 时返回 "_",保证 join 后不越出 RunsRoot。
func safeOutDirName(taskID string) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, taskID)
	if name == "" || name == "." || name == ".." {
		return "_"
	}
	return name
}

// Dispatch 是契约 TaskDispatchRequest(已过嵌入 Schema 校验后解码)。
type Dispatch struct {
	TaskID         string `json:"task_id"`
	IdempotencyKey string `json:"idempotency_key"`
	Attempt        int    `json:"attempt"`
	Artifact       struct {
		URL    string `json:"url"`
		SHA256 string `json:"sha256"`
		Auth   struct {
			Type  string `json:"type"`
			Token string `json:"token"`
		} `json:"auth"`
	} `json:"artifact"`
	ManifestDigest   string `json:"manifest_digest"`
	DeviceSerial     string `json:"device_serial"`
	CallbackBaseURL  string `json:"callback_base_url"`
	PresignedUploads []struct {
		ObjectKey string `json:"object_key"`
		URL       string `json:"url"`
		ExpiresAt string `json:"expires_at"`
	} `json:"presigned_uploads"`
}

// dispatchTask 实现 POST /api/v1/tasks:Schema 校验 → 幂等检查 →
// 202 入队 → 后台 goroutine 异步执行(设计 §3.5,幂等语义 §4)。
func (s *Server) dispatchTask(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxDispatchBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "read body: "+err.Error())
		return
	}
	// 红线 §14:未经 Schema 校验不消费 dispatch 载荷
	if err := ValidateDispatch(body); err != nil {
		writeErr(w, http.StatusBadRequest, "schema_violation", err.Error())
		return
	}
	var d Dispatch
	if err := json.Unmarshal(body, &d); err != nil { // 已过 Schema,理论上不可达
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// task_id 由 Runtime 按 {workflow_id}:{test_id}:a{attempt} 生成,
	// 含项目路径 '/' 与分隔符 ':'(合法);只有文件系统路径需要净化:
	// out_dir 用safe 化目录名,杜绝路径穿越,同时兼容 Windows 禁用的 ':'。
	outDir := filepath.Join(s.cfg.RunsRoot, safeOutDirName(d.TaskID))

	ctx := r.Context()
	// 同幂等键 → 返回既有任务当前状态,不重复执行(§4)
	if t, err := s.cfg.Store.LookupByIdempotencyKey(ctx, d.IdempotencyKey); err == nil {
		writeJSON(w, http.StatusAccepted, s.taskStatus(ctx, t))
		return
	}
	// 同 task_id 异键 → 契约冲突,409
	if _, err := s.cfg.Store.GetTask(ctx, d.TaskID); err == nil {
		writeErr(w, http.StatusConflict, "task_conflict",
			"task_id "+d.TaskID+" already exists with a different idempotency_key")
		return
	} else if !errors.Is(err, store.ErrTaskNotFound) {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}

	task := store.Task{
		TaskID:         d.TaskID,
		IdempotencyKey: d.IdempotencyKey,
		State:          store.StateQueued,
		Attempt:        d.Attempt,
		DispatchJSON:   string(body),
		OutDir:         outDir,
	}
	if err := s.cfg.Store.CreateTask(ctx, task); err != nil {
		// 并发窗口:预检与插入之间被另一个相同请求抢先——按其结果应答
		if t, lerr := s.cfg.Store.LookupByIdempotencyKey(ctx, d.IdempotencyKey); lerr == nil {
			writeJSON(w, http.StatusAccepted, s.taskStatus(ctx, t))
			return
		}
		if _, gerr := s.cfg.Store.GetTask(ctx, d.TaskID); gerr == nil {
			writeErr(w, http.StatusConflict, "task_conflict",
				"task_id "+d.TaskID+" already exists with a different idempotency_key")
			return
		}
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}

	// 先登记 Executor 再应答:应答后的 DELETE 才能立即找到并取消
	s.startTask(d, outDir)

	t, err := s.cfg.Store.GetTask(ctx, d.TaskID)
	if err != nil { // 刚插入,不可达;兜底按入参应答
		t = task
		t.StartedAt = time.Now()
	}
	writeJSON(w, http.StatusAccepted, s.taskStatus(ctx, t))
}

// getTask 实现 GET /api/v1/tasks/{task_id}:200 现状 / 404。
func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	t, err := s.cfg.Store.GetTask(r.Context(), r.PathValue("task_id"))
	if errors.Is(err, store.ErrTaskNotFound) {
		writeErr(w, http.StatusNotFound, "task_not_found", "no such task: "+r.PathValue("task_id"))
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.taskStatus(r.Context(), t))
}

// cancelTask 实现 DELETE /api/v1/tasks/{task_id}:未知 404;已终态 202
// 返回现状(幂等);进行中 202 并调用运行中 Executor 的 Cancel(尽力而为,
// 终态以 task-events/results 回调为准)。
func (s *Server) cancelTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, err := s.cfg.Store.GetTask(ctx, r.PathValue("task_id"))
	if errors.Is(err, store.ErrTaskNotFound) {
		writeErr(w, http.StatusNotFound, "task_not_found", "no such task: "+r.PathValue("task_id"))
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if !store.IsTerminal(t.State) {
		s.mu.Lock()
		exec := s.running[t.TaskID]
		s.mu.Unlock()
		if exec != nil {
			exec.Cancel()
		}
	}
	writeJSON(w, http.StatusAccepted, s.taskStatus(ctx, t))
}

// newExecutor 构造一次运行的 Executor(可用 Config.NewExecutor 注入 fake)。
func (s *Server) newExecutor() *executor.Executor {
	if s.cfg.NewExecutor != nil {
		return s.cfg.NewExecutor()
	}
	return &executor.Executor{
		Runner:     s.cfg.Runner,
		HTTP:       s.cfg.HTTP,
		Logf:       s.cfg.Logf,
		SOCAliases: s.cfg.SOCAliases,
	}
}

// startTask 登记并异步启动任务执行。
func (s *Server) startTask(d Dispatch, outDir string) {
	exec := s.newExecutor()
	s.mu.Lock()
	s.running[d.TaskID] = exec
	s.mu.Unlock()
	go s.runTask(d, outDir, exec)
}

// runTask 是单个任务的后台执行体:QUEUED→ACCEPTED 起始迁移 →
// executor 流水线(每次迁移经 EventReporter 落盘+即发)→ 终态后
// 预签名直传附件(降级不阻断)→ result 上报。
func (s *Server) runTask(d Dispatch, outDir string, exec *executor.Executor) {
	defer func() {
		s.mu.Lock()
		delete(s.running, d.TaskID)
		s.mu.Unlock()
	}()
	ctx := context.Background() // 与请求生命周期解耦;取消走 executor.Cancel

	// 执行开始:QUEUED → ACCEPTED(executor.Status 无 QUEUED/ACCEPTED,直接转换)
	s.cfg.Events.OnTransition(d.TaskID,
		executor.Status(store.StateQueued), executor.Status(store.StateAccepted), "")

	exec.OnTransition = func(to executor.Status) {
		s.cfg.Events.OnTransition(d.TaskID, s.currentStatus(d.TaskID), to, "")
	}

	if _, err := exec.Execute(ctx, executor.Options{
		PackageURL: d.Artifact.URL,
		SHA256:     d.Artifact.SHA256,
		Auth:       &artifact.Auth{Type: d.Artifact.Auth.Type, Token: d.Artifact.Auth.Token},
		Serial:     d.DeviceSerial,
		OutDir:     outDir,
	}); err != nil {
		s.logf("task %s: execute: %v", d.TaskID, err) // FAILED 迁移已由 executor 发出
	}

	attachments := s.uploadAttachments(ctx, d, outDir)
	if err := s.cfg.Results.Report(ctx, d.TaskID, attachments); err != nil {
		s.logf("task %s: report result: %v", d.TaskID, err)
	}
}

// currentStatus 读 store 当前状态作为事件 from-state;读失败回退 ACCEPTED
// (Transition 会再次校验 from,失败只丢事件即发,后台补报不依赖此处)。
func (s *Server) currentStatus(taskID string) executor.Status {
	t, err := s.cfg.Store.GetTask(context.Background(), taskID)
	if err != nil {
		return executor.Status(store.StateAccepted)
	}
	return executor.Status(t.State)
}

// wellKnownFiles 是固定键集(设计决策 1):runs/{task_id}/ 下的对象名 →
// out_dir 内相对路径。
var wellKnownFiles = map[string]string{
	"result.json": "device/results/result.json",
	"junit.xml":   "device/results/junit.xml",
	"logcat.txt":  "logcat.txt",
	"stdout.log":  "stdout.log",
	"stderr.log":  "stderr.log",
}

// uploadAttachments 按 dispatch.presigned_uploads 直传收集到的固定键集文件;
// 键不在映射内或文件缺失均降级跳过(uploader 语义,设计 §3.4)。
func (s *Server) uploadAttachments(ctx context.Context, d Dispatch, outDir string) []reporter.Attachment {
	if s.cfg.Uploader == nil || len(d.PresignedUploads) == 0 {
		return nil
	}
	prefix := "runs/" + d.TaskID + "/"
	presigned := make([]uploader.PresignedUpload, 0, len(d.PresignedUploads))
	files := map[string]string{}
	for _, p := range d.PresignedUploads {
		pu := uploader.PresignedUpload{ObjectKey: p.ObjectKey, URL: p.URL}
		if p.ExpiresAt != "" {
			if ts, err := time.Parse(time.RFC3339, p.ExpiresAt); err == nil {
				pu.ExpiresAt = ts
			} else {
				s.logf("task %s: %s expires_at 不可解析(%v),按不过期处理", d.TaskID, p.ObjectKey, err)
			}
		}
		presigned = append(presigned, pu)
		name := strings.TrimPrefix(p.ObjectKey, prefix)
		if rel, ok := wellKnownFiles[name]; ok {
			files[p.ObjectKey] = filepath.Join(outDir, filepath.FromSlash(rel))
		} else {
			s.logf("task %s: object_key %s 不在固定键集映射内,跳过上传", d.TaskID, p.ObjectKey)
		}
	}
	return s.cfg.Uploader.Upload(ctx, presigned, files)
}
