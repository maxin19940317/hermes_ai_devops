package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
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
			Type     string `json:"type"`
			Token    string `json:"token"`
			Username string `json:"username"` // basic(Deploy Token)用户名;契约只加不删
		} `json:"auth"`
	} `json:"artifact"`
	ManifestDigest  string `json:"manifest_digest"`
	DeviceSerial    string `json:"device_serial"`
	CallbackBaseURL string `json:"callback_base_url"`
	// 租约所有权凭据(差距 #15,契约只加不删):新 Runtime 派单携带,
	// 落库后心跳续租时原样回传;旧 Runtime 不携带(空 = 无凭据,
	// 心跳按旧字符串格式上报)。
	LeaseID         string `json:"lease_id"`
	LeaseGeneration int    `json:"lease_generation"`
	// UploadRequestURL 是按需签发端点的完整 URL(差距 #8);为空表示 Runtime
	// 未启用按需签发(旧 Runtime 或未配置 CALLBACK_BASE_URL),沿用
	// PresignedUploads 固定键集(设计 §4.2)。
	UploadRequestURL string `json:"upload_request_url"`
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
		// 租约所有权凭据随任务落库:崩溃重启后心跳仍按新格式续租(§10)
		LeaseID:         d.LeaseID,
		LeaseGeneration: d.LeaseGeneration,
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
		s.cancelRunning(t.TaskID)
	}
	writeJSON(w, http.StatusAccepted, s.taskStatus(ctx, t))
}

// cancelRunning 调用运行中 Executor 的 Cancel(尽力而为);未在运行表
// 中幂等空转。DELETE 取消与 LEASE_NOT_OWNED 停止共用此路径。
func (s *Server) cancelRunning(taskID string) {
	s.mu.Lock()
	exec := s.running[taskID]
	s.mu.Unlock()
	if exec != nil {
		exec.Cancel()
	}
}

// StopTask 是 reporter.Heartbeat 的 LEASE_NOT_OWNED 停止钩子(§10/差距 #15):
// 心跳应答宣告租约已易主/失效,继续操作设备会干扰新持有者——走与 DELETE
// 取消相同的清理路径;executor 随后把任务迁移到 CANCELED 终态,自动离开
// 心跳的 inflight 集合。未知/已终态任务幂等空转。
func (s *Server) StopTask(taskID string) {
	t, err := s.cfg.Store.GetTask(context.Background(), taskID)
	if err != nil || store.IsTerminal(t.State) {
		return
	}
	s.logf("task %s: lease not owned by runtime, canceling local execution", taskID)
	s.cancelRunning(taskID)
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

	var collected []string
	if sum, err := exec.Execute(ctx, executor.Options{
		PackageURL: d.Artifact.URL,
		SHA256:     d.Artifact.SHA256,
		Auth:       &artifact.Auth{Type: d.Artifact.Auth.Type, Token: d.Artifact.Auth.Token, Username: d.Artifact.Auth.Username},
		Serial:     d.DeviceSerial,
		OutDir:     outDir,
	}); err != nil {
		s.logf("task %s: execute: %v", d.TaskID, err) // FAILED 迁移已由 executor 发出
		if sum != nil {
			collected = sum.Collected
		}
	} else {
		collected = sum.Collected
	}

	attachments := s.uploadAttachments(ctx, d, outDir, collected)
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
// out_dir 内相对路径。差距 #8 之后,它只服务于 uploadFixedSet 回退路径——
// 按需签发路径(uploadOnDemand)直接用 out_dir 内的相对路径本身,不再需要
// 这张映射(设计 §5.3)。
var wellKnownFiles = map[string]string{
	"result.json": "device/results/result.json",
	"junit.xml":   "device/results/junit.xml",
	"logcat.txt":  "logcat.txt",
	"stdout.log":  "stdout.log",
	"stderr.log":  "stderr.log",
}

// onDemandRetries 是按需签发端点不可达时的重试次数上限,重试间隔与
// §10 的 ADB 命令级重试同量级(设计 §5.2)。
const onDemandRetries = 2

// onDemandRetryDelay 是 onDemandRetries 每次重试之间的等待。
const onDemandRetryDelay = 3 * time.Second

// uploadAttachments 上传收集到的附件。优先按需签发(差距 #8):用本次实际收集到
// 的文件换 URL,glob 命中的文件(logs/*.log、dumps/**)因此第一次能被上传。
// 任何失败(含 401 租约失效)都回退到派单时的固定键集。
//
// 401 也回退,是 2026-07-29 final-review 的更正。原设计写的是"401 不回退,
// 继续上传会污染别人的证据",但那个理由不成立:回退用的派单期 URL 同样限定在
// runs/{task_id}/ 前缀内,而 task_id 编码了 attempt(:a{N}),所以迟到的上传只能
// 写进自己的目录——重试拿到的是不同的 task_id、不同的前缀。而不回退的代价是
// 实打实的:硬超时、租约过期、AcquireDevice 懒回收都会让 VerifyLease 判否,
// 于是跑完但迟到的任务一个附件都不传,包括 logcat——恰好是最需要 INFRA 排查
// 证据的场合把证据丢了。
//
// collected 是 executor.Summary.Collected(设备侧按 manifest collect 列表拉取
// 的、相对于 out_dir/device/ 的路径清单)。CRITICAL:不能改回遍历 out_dir——
// out_dir 根还留着下载的 package.tar.gz 与解包出的 package/ 子树(SDK 全量,
// 数百个文件),两者都不是"收集到的输出",遍历会把它们全部当附件请求签发,
// 撞上 upload-requests 的文件数上限,导致按需签发整体失败并回退到固定键集
// (等价于本分支要修的问题重新出现)。
func (s *Server) uploadAttachments(ctx context.Context, d Dispatch, outDir string, collected []string) []reporter.Attachment {
	if s.cfg.Uploader == nil {
		return nil
	}
	if d.UploadRequestURL != "" && s.cfg.Reporter != nil {
		atts, err := s.uploadOnDemand(ctx, d, outDir, collected)
		if err == nil {
			return atts
		}
		if errors.Is(err, reporter.ErrLeaseNotOwned) {
			// 租约失效值得单独记一条:它意味着任务已易主/被回收,附件虽仍会经固定
			// 键集上传(同样限定在本 task 自己的前缀内),但结果回流大概率已无人接收。
			s.logf("task %s: 租约已非己有,按需签发被拒;仍回退固定键集上传附件", d.TaskID)
		} else {
			s.logf("task %s: 按需签发失败(%v),回退固定键集", d.TaskID, err)
		}
	}
	return s.uploadFixedSet(ctx, d, outDir)
}

// agentRootFiles 是 executor 直接写在 out_dir 根、但不属于设备采集
// (executor.Summary.Collected)的产物:logcat 转储、entry 的 stdout/stderr、
// 运行摘要。是否存在取决于运行到了哪个阶段(如 dumpLogcat 失败则 logcat.txt
// 缺失),按需签发时逐个探测存在性,不存在则跳过。
var agentRootFiles = []string{"logcat.txt", "stdout.log", "stderr.log", "run-summary.json"}

// buildAttachmentPaths 组装按需签发请求的相对路径清单:collected(相对于
// out_dir/device/,加上 "device/" 前缀落到实际文件位置)+ 存在的
// agentRootFiles。不遍历 out_dir 文件系统,因此天然排除 package.tar.gz 与
// 解包出的 package/ 子树。
func buildAttachmentPaths(outDir string, collected []string) []string {
	rels := make([]string, 0, len(collected)+len(agentRootFiles))
	for _, c := range collected {
		rels = append(rels, "device/"+filepath.ToSlash(c))
	}
	for _, name := range agentRootFiles {
		if _, err := os.Stat(filepath.Join(outDir, name)); err == nil {
			rels = append(rels, name)
		}
	}
	return rels
}

// uploadOnDemand 用 buildAttachmentPaths 得到本次实际收集到的相对路径清单,向
// UploadRequestURL 换取预签名 URL 后交给既有 Uploader.Upload(差距 #8)。
// 端点不可达 / 5xx / 超时重试 onDemandRetries 次,间隔 onDemandRetryDelay;
// 401 立即返回 reporter.ErrLeaseNotOwned,调用方据此不回退(设计 §5.2)。
func (s *Server) uploadOnDemand(ctx context.Context, d Dispatch, outDir string, collected []string) ([]reporter.Attachment, error) {
	rels := buildAttachmentPaths(outDir, collected)
	if len(rels) == 0 {
		return nil, nil
	}
	req := reporter.UploadRequest{
		TaskID: d.TaskID, ClientID: s.cfg.ClientID, DeviceID: d.DeviceSerial,
		Attempt: d.Attempt, LeaseID: d.LeaseID, LeaseGeneration: d.LeaseGeneration,
		Files: rels,
	}
	var result *reporter.UploadRequestResult
	var err error
	for attempt := 0; ; attempt++ {
		result, err = s.cfg.Reporter.RequestUploads(ctx, d.UploadRequestURL, req)
		if err == nil {
			break
		}
		if errors.Is(err, reporter.ErrLeaseNotOwned) {
			return nil, err
		}
		// 只重试网络错误/超时/5xx;其余 4xx(载荷错误/文件数超限等)是
		// 确定性失败,重试只会白等 onDemandRetryDelay 后拿到同样的结果
		// (MINOR 3)。
		if attempt >= onDemandRetries || !reporter.Retryable(err) {
			return nil, err
		}
		s.logf("task %s: 请求上传 URL 失败(%v),%v 后重试(%d/%d)", d.TaskID, err, onDemandRetryDelay, attempt+1, onDemandRetries)
		// 用 select 而非裸 time.Sleep,让已取消的任务不必等满
		// onDemandRetryDelay 才发现取消(MINOR 4)。
		timer := time.NewTimer(onDemandRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}

	presigned := make([]uploader.PresignedUpload, 0, len(result.Uploads))
	files := map[string]string{}
	for _, g := range result.Uploads {
		pu := uploader.PresignedUpload{ObjectKey: g.ObjectKey, URL: g.URL}
		if g.ExpiresAt != "" {
			if ts, terr := time.Parse(time.RFC3339, g.ExpiresAt); terr == nil {
				pu.ExpiresAt = ts
			} else {
				s.logf("task %s: %s expires_at 不可解析(%v),按不过期处理", d.TaskID, g.ObjectKey, terr)
			}
		}
		presigned = append(presigned, pu)
		files[g.ObjectKey] = filepath.Join(outDir, filepath.FromSlash(g.Path))
	}
	for _, rj := range result.Rejected {
		s.logf("task %s: 按需签发拒绝 %s: %s", d.TaskID, rj.Path, rj.Reason)
	}
	return s.cfg.Uploader.Upload(ctx, presigned, files), nil
}

// uploadFixedSet 按 dispatch.presigned_uploads 直传收集到的固定键集文件;
// 键不在映射内或文件缺失均降级跳过(uploader 语义,设计 §3.4)。这是按需签发
// (uploadOnDemand)不可用时的回退路径(设计 §5.2)。
//
// object_key 匹配用 filepath.Base(取最后一段文件名)做 wellKnownFiles 查找,
// 不依赖 "runs/{task_id}/" 前缀。若 Runtime 未来调整键结构(如加 attempt 段),
// 前缀匹配会静默跳过所有项;后缀匹配则天然兼容任何前缀变更(审查 #4)。
func (s *Server) uploadFixedSet(ctx context.Context, d Dispatch, outDir string) []reporter.Attachment {
	if s.cfg.Uploader == nil || len(d.PresignedUploads) == 0 {
		return nil
	}
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
		name := filepath.Base(p.ObjectKey)
		if rel, ok := wellKnownFiles[name]; ok {
			files[p.ObjectKey] = filepath.Join(outDir, filepath.FromSlash(rel))
		} else {
			s.logf("task %s: object_key %s 不在固定键集映射内,跳过上传", d.TaskID, p.ObjectKey)
		}
	}
	return s.cfg.Uploader.Upload(ctx, presigned, files)
}
