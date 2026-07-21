package reporter

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"hermes-devops/agent/internal/executor"
	"hermes-devops/agent/internal/store"
)

// EmbeddedResultSchema 是 contracts/result.schema.json 的编译期副本;
// 与源文件的一致性由 TestEmbeddedSchemaMatchesContract 防漂移(改契约后
// cp 同步,同 manifest 包模式)。
//
//go:embed result.schema.json
var EmbeddedResultSchema []byte

var compiledResultSchema = mustCompileResultSchema()

func mustCompileResultSchema() *jsonschema.Schema {
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2020
	if err := c.AddResource("result.schema.json", bytes.NewReader(EmbeddedResultSchema)); err != nil {
		panic(fmt.Sprintf("embedded result schema unreadable: %v", err))
	}
	return c.MustCompile("result.schema.json")
}

// resultReporter 默认值:5xx(signal_error)重发,指数退避。
const (
	DefaultResultMaxAttempts    = 5
	DefaultResultInitialBackoff = time.Second
	DefaultResultMaxBackoff     = 30 * time.Second
)

// Attachment 描述已由 uploader(Task 5)预签名直传 MinIO 的附件;
// result.json 只携带对象键清单,附件不经 Runtime 中转(红线 §14)。
type Attachment struct {
	Name      string `json:"name"`
	ObjectKey string `json:"object_key"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
}

// CaseFailure 是单个失败用例的记录。
type CaseFailure struct {
	Name    string `json:"name"`
	Message string `json:"message,omitempty"`
}

// Cases 是用例计数。Failures 始终为非 nil(契约要求数组,不许 null)。
type Cases struct {
	Total    int           `json:"total"`
	Passed   int           `json:"passed"`
	Failed   int           `json:"failed"`
	Skipped  int           `json:"skipped"`
	Failures []CaseFailure `json:"failures"`
}

// ArtifactInfo 是 result.json 的 artifact 块(可选;仅在有数据时上送)。
type ArtifactInfo struct {
	Commit     string `json:"commit,omitempty"`
	PipelineID *int64 `json:"pipeline_id,omitempty"`
}

// Result 是 result.json v1(contracts/result.schema.json)。
// SignaturesHit/Metrics/Attachments 始终为非 nil:契约对已知字段类型
// 严格校验,null 不合法。
type Result struct {
	ResultVersion int                `json:"result_version"`
	TaskID        string             `json:"task_id"`
	Attempt       int                `json:"attempt"`
	Status        string             `json:"status"`
	ExitCode      int                `json:"exit_code"`
	DurationSec   float64            `json:"duration_sec"`
	Cases         Cases              `json:"cases"`
	SignaturesHit []string           `json:"signatures_hit"`
	Metrics       map[string]float64 `json:"metrics"`
	Environment   map[string]string  `json:"environment,omitempty"`
	Artifact      *ArtifactInfo      `json:"artifact,omitempty"`
	Attachments   []Attachment       `json:"attachments"`
}

// deviceResult 是设备端脚本自产的 result.json 中 Client 采信的字段
// (其余字段忽略;设备数据不可信,最终整体仍须过 Schema)。
type deviceResult struct {
	Cases         *Cases             `json:"cases"`
	SignaturesHit []string           `json:"signatures_hit"`
	Metrics       map[string]float64 `json:"metrics"`
	Artifact      *ArtifactInfo      `json:"artifact"`
}

// deviceResultPath 是 collect 拉回的设备端 result.json 的约定位置
// (out_dir/device/results/result.json,见 smoke 包与 README)。
func deviceResultPath(outDir string) string {
	return filepath.Join(outDir, "device", "results", "result.json")
}

// ValidateResult 用嵌入的 result.schema.json 校验已编码的 result.json。
func ValidateResult(data []byte) error {
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("decode result json: %w", err)
	}
	if err := compiledResultSchema.Validate(doc); err != nil {
		return fmt.Errorf("result schema validation: %w", err)
	}
	return nil
}

// ResultReporter 在任务终态后组装并上报 result.json v1:
// store 任务(attempt/幂等键/dispatch_json)+ executor 写盘的
// run-summary.json + 设备端 result.json(如有)合成全文,先过嵌入
// Schema 再 POST;成功 MarkResultRecorded(§4 去重)。
type ResultReporter struct {
	Store  *store.Store
	Client *Client
	Logf   func(format string, args ...any) // nil → 静默

	MaxAttempts    int           // 5xx/网络错误的最大尝试次数;0 → DefaultResultMaxAttempts
	InitialBackoff time.Duration // 0 → DefaultResultInitialBackoff
	MaxBackoff     time.Duration // 0 → DefaultResultMaxBackoff
}

func (r *ResultReporter) maxAttempts() int {
	if r.MaxAttempts > 0 {
		return r.MaxAttempts
	}
	return DefaultResultMaxAttempts
}

func (r *ResultReporter) initialBackoff() time.Duration {
	if r.InitialBackoff > 0 {
		return r.InitialBackoff
	}
	return DefaultResultInitialBackoff
}

func (r *ResultReporter) maxBackoff() time.Duration {
	if r.MaxBackoff > 0 {
		return r.MaxBackoff
	}
	return DefaultResultMaxBackoff
}

func (r *ResultReporter) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

// Report 上报单个终态任务的结果。attachments 由调用方(uploader)传入;
// 任务未终态或 run-summary.json 缺失返回错误;已上报(ResultRecorded)
// 直接返回 nil。发送前必过嵌入 Schema;Runtime 400(schema_violation /
// unknown_task)不重发,5xx/网络错误按指数退避重发至 MaxAttempts。
func (r *ResultReporter) Report(ctx context.Context, taskID string, attachments []Attachment) error {
	task, err := r.Store.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("report result: %w", err)
	}
	if !store.IsTerminal(task.State) {
		return fmt.Errorf("report result %s: task not terminal (state %s)", taskID, task.State)
	}
	if task.ResultRecorded {
		return nil // §4:结果按 task_id 去重,已上报则跳过
	}

	data, err := r.build(task, attachments)
	if err != nil {
		return fmt.Errorf("report result %s: %w", taskID, err)
	}
	rep := ResultReport{
		TaskID:         task.TaskID,
		IdempotencyKey: task.IdempotencyKey,
		Result:         data,
	}

	backoff := r.initialBackoff()
	for attempt := 1; ; attempt++ {
		err := r.Client.ReportResult(ctx, rep)
		if err == nil {
			if err := r.Store.MarkResultRecorded(ctx, taskID); err != nil {
				return fmt.Errorf("report result %s: %w", taskID, err)
			}
			return nil
		}
		if !Retryable(err) {
			// 400:永久拒绝,重发无意义——大声记日志并上抛,由人工排查
			r.logf("result: %s 被 Runtime 拒绝(不重发): %v", taskID, err)
			return fmt.Errorf("report result %s: %w", taskID, err)
		}
		if attempt >= r.maxAttempts() {
			return fmt.Errorf("report result %s: exhausted %d attempts: %w", taskID, r.maxAttempts(), err)
		}
		r.logf("result: %s 上报失败(第 %d 次),%s 后重试: %v", taskID, attempt, backoff, err)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("report result %s: %w", taskID, ctx.Err())
		case <-timer.C:
		}
		backoff *= 2
		if backoff > r.maxBackoff() {
			backoff = r.maxBackoff()
		}
	}
}

// RecoverPending 崩溃恢复:对 store 中已终态但结果未上报的任务逐一
// 重投(attachments 不可得——崩溃前 uploader 的产物未持久化,本轮以空
// 清单上报,附件缺失不阻断结果回流,见设计 §3.4 降级语义)。
// 单个任务失败不影响其余任务;错误聚合返回。
func (r *ResultReporter) RecoverPending(ctx context.Context) error {
	inf, err := r.Store.LoadInflight(ctx)
	if err != nil {
		return fmt.Errorf("recover pending results: %w", err)
	}
	var errs []error
	for _, t := range inf.PendingResults {
		if err := r.Report(ctx, t.TaskID, nil); err != nil {
			r.logf("result: recover %s: %v", t.TaskID, err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// build 组装 result.json v1 并过嵌入 Schema,返回可直接上送的编码。
func (r *ResultReporter) build(task store.Task, attachments []Attachment) ([]byte, error) {
	sum, err := readSummary(task.OutDir)
	if err != nil {
		return nil, err
	}

	res := Result{
		ResultVersion: 1,
		TaskID:        task.TaskID,
		Attempt:       task.Attempt,
		Status:        string(task.State), // 终态集合与契约 enum 一致(由 store 测试保证)
		ExitCode:      sum.ExitCode,
		DurationSec:   sum.DurationSec,
		SignaturesHit: []string{},
		Metrics:       map[string]float64{},
		Attachments:   attachments,
		Environment:   sum.Environment,
	}
	if res.Attachments == nil {
		res.Attachments = []Attachment{}
	}

	// 设备端 result.json 优先提供用例计数/签名/指标;缺失或损坏时按
	// 退出判据合成计数(total=1,passed=0/1)——此时唯一的"用例"就是
	// entry 本身是否满足 success 判据。
	dev, devErr := readDeviceResult(deviceResultPath(task.OutDir))
	switch {
	case devErr == nil && dev.Cases != nil:
		res.Cases = *dev.Cases
		if res.Cases.Failures == nil {
			res.Cases.Failures = []CaseFailure{}
		}
	case devErr == nil || os.IsNotExist(devErr):
		res.Cases = synthesizeCases(sum)
	default:
		// 设备结果损坏:记日志后按合成计数上报,结果回流优先
		r.logf("result: %s 设备 result.json 不可解析(%v),按退出判据合成 cases", task.TaskID, devErr)
		res.Cases = synthesizeCases(sum)
	}
	if devErr == nil {
		if dev.SignaturesHit != nil {
			res.SignaturesHit = dev.SignaturesHit
		}
		if dev.Metrics != nil {
			res.Metrics = dev.Metrics
		}
		res.Artifact = dev.Artifact
	}

	// artifact 信息兜底:dispatch_json 的 artifact 块(契约当前只有
	// url/sha256/auth;commit/pipeline_id 为前向兼容解析,有则上送)
	if res.Artifact == nil {
		res.Artifact = artifactFromDispatch(task.DispatchJSON)
	}
	if res.Artifact != nil && res.Artifact.Commit == "" && res.Artifact.PipelineID == nil {
		res.Artifact = nil // 空块不上送
	}

	data, err := json.Marshal(res)
	if err != nil {
		return nil, fmt.Errorf("encode result: %w", err)
	}
	if err := ValidateResult(data); err != nil {
		return nil, err // 本地校验不过绝不发送,避免无谓的 400
	}
	return data, nil
}

// readSummary 读取 executor 在终态时写入 out_dir 的 run-summary.json。
func readSummary(outDir string) (*executor.Summary, error) {
	data, err := os.ReadFile(filepath.Join(outDir, "run-summary.json"))
	if err != nil {
		return nil, fmt.Errorf("read run-summary.json: %w", err)
	}
	var sum executor.Summary
	if err := json.Unmarshal(data, &sum); err != nil {
		return nil, fmt.Errorf("parse run-summary.json: %w", err)
	}
	return &sum, nil
}

// readDeviceResult 读取设备端 result.json;不存在返回 os.IsNotExist 错误。
func readDeviceResult(path string) (*deviceResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var dr deviceResult
	if err := json.Unmarshal(data, &dr); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &dr, nil
}

// synthesizeCases 在无设备 result.json 时按退出判据合成用例计数。
func synthesizeCases(sum *executor.Summary) Cases {
	passed := 0
	if sum.SuccessCriteriaMet {
		passed = 1
	}
	return Cases{
		Total:    1,
		Passed:   passed,
		Failed:   1 - passed,
		Skipped:  0,
		Failures: []CaseFailure{},
	}
}

// artifactFromDispatch 从 dispatch_json 的 artifact 块解析 commit /
// pipeline_id;没有可上送信息时返回 nil。
func artifactFromDispatch(dispatchJSON string) *ArtifactInfo {
	var d struct {
		Artifact struct {
			Commit     string `json:"commit"`
			PipelineID *int64 `json:"pipeline_id"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal([]byte(dispatchJSON), &d); err != nil {
		return nil
	}
	if d.Artifact.Commit == "" && d.Artifact.PipelineID == nil {
		return nil
	}
	return &ArtifactInfo{Commit: d.Artifact.Commit, PipelineID: d.Artifact.PipelineID}
}
