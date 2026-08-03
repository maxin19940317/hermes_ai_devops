package activity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"hermes-devops/runtime/internal/evidence"
	"hermes-devops/runtime/internal/hermesclient"
	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

// evidenceFileKey 证据附件文件名 → 提取器 Files 键(§3.7 固定键集)。
var evidenceFileKey = map[string]string{
	"logcat.txt": "logcat",
	"stdout.log": "stdout",
	"stderr.log": "stderr",
	"junit.xml":  "junit",
}

// evidenceFileNames 固定顺序,保证 missing 输出确定。
var evidenceFileNames = []string{"logcat.txt", "stdout.log", "stderr.log", "junit.xml"}

// ExtractEvidence 从 MinIO 拉取任务附件并做确定性证据提取(§12 Phase 2)。
// MinIO 未配置、附件缺失、拉取失败一律降级进 evidence.inputs.missing,
// 不返回错误——证据缺失不构成重试理由,结果回流优先(§3.7)。
func (a *Acts) ExtractEvidence(ctx context.Context, req wf.ExtractEvidenceRequest) (*wf.ExtractEvidenceResponse, error) {
	in := evidence.Input{
		TaskID: req.TaskID, Variant: req.Variant,
		Status: req.Result.Status, ExitCode: req.Result.ExitCode, DurationSec: req.Result.DurationSec,
		CasesTotal:            req.Result.CasesTotal,
		CasesFailed:           req.Result.CasesFailed,
		SignaturesHitReported: req.Result.SignaturesHit,
		Metrics:               req.Result.Metrics,
	}
	if a.SpecCfg != nil {
		in.Signatures = a.SpecCfg.SignaturesForVariant(req.Variant)
	}
	in.Files, in.Missing = a.fetchEvidenceFiles(ctx, req.Result.Attachments)
	ev := evidence.Extract(in)
	for _, r := range in.Files {
		if c, ok := r.(io.Closer); ok {
			_ = c.Close()
		}
	}

	// ---- metrics 基线比较(§9 PERF_REGRESSION 的基础) ----
	// 每条指标与过去 N 次 PASSED 中位数比较,填充 evidence 的 metrics_baseline。
	// 基线不足(< 3 个样本)时 baseline/delta 为 nil,Analyzer 读到 nil 应判定"证据不足"。
	if len(ev.Metrics) > 0 && a.Store != nil {
		ev.MetricsBaseline = computeMetricsBaseline(ctx, a.Store, req.Project, req.Variant, ev.Metrics)
	}

	// PASSED 任务:落 metrics 表,供后续基线计算。
	if ev.Status == "COMPLETED" && ev.ExitCode == 0 && ev.Cases.Failed == 0 && len(ev.Metrics) > 0 && a.Store != nil {
		points := make([]store.MetricPoint, 0, len(ev.Metrics))
		for name, val := range ev.Metrics {
			points = append(points, store.MetricPoint{
				Project: req.Project, Variant: req.Variant, Suite: "smoke",
				MetricName: name, Value: val, TaskID: req.TaskID,
			})
		}
		if err := a.Store.SaveMetrics(ctx, points); err != nil {
			a.warnf("save metrics failed: %v", err)
		}
	}

	raw, err := json.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("marshal evidence: %w", err)
	}
	// runtime 侧确定性提取的签名命中,供规则归类复用(判定权仍在规则引擎,§9)
	matched := []string{}
	for _, sig := range ev.Signatures {
		if sig.Matched {
			matched = append(matched, sig.ID)
		}
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	return &wf.ExtractEvidenceResponse{
		EvidenceJSON:      raw,
		Digest:            digest,
		MatchedSignatures: matched,
		SnapshotID:        a.persistEvidenceSnapshot(ctx, req.TaskID, ev.EvidenceVersion, raw, digest),
	}, nil
}

// persistEvidenceSnapshot 把 evidence.json 上传 MinIO 并登记 evidence_snapshots
// (差距 #6,决策可回放);返回 evidence_id(= task_id,含 attempt 全链路唯一,
// 重复提取幂等)。MinIO 未配置/上传失败/落库失败一律降级:记日志返回空串,
// 不阻断分析——evidence 本体仍随响应内存传递(§3.7)。
func (a *Acts) persistEvidenceSnapshot(ctx context.Context, taskID string, extractorVersion int, raw []byte, digest string) string {
	if a.Store == nil || !a.Cfg.presignEnabled() {
		return "" // MinIO 未配置:快照不可用是既定降级形态,无需每任务刷日志
	}
	cli, err := evidenceClient(a.Cfg)
	if err != nil {
		a.warnf("minio evidence client init failed: %v; snapshot skipped", err)
		return ""
	}
	// object_key 与 runs/{task_id}/ 附件并排:evidence/{task_id}/evidence.json。
	// 同一任务重复提取(activity 重试)覆写同一 key、同 evidence_id 幂等。
	objectKey := "evidence/" + taskID + "/evidence.json"
	if _, err := cli.PutObject(ctx, a.Cfg.MinIOBucket, objectKey,
		bytes.NewReader(raw), int64(len(raw)),
		minio.PutObjectOptions{ContentType: "application/json"}); err != nil {
		a.warnf("evidence snapshot upload %s failed: %v", objectKey, err)
		return ""
	}
	if err := a.Store.SaveEvidenceSnapshot(ctx, store.EvidenceSnapshot{
		EvidenceID: taskID, TaskID: taskID, Attempt: attemptFromTaskID(taskID),
		ObjectKey: objectKey, SHA256: digest,
		ExtractorVersion: strconv.Itoa(extractorVersion),
	}); err != nil {
		a.warnf("save evidence snapshot %s failed: %v", taskID, err)
		return ""
	}
	return taskID
}

// attemptFromTaskID 解析 task_id 的 :a{N} 后缀(差距 #14);失败返回 0。
func attemptFromTaskID(taskID string) int {
	i := strings.LastIndex(taskID, ":a")
	if i < 0 {
		return 0
	}
	n, err := strconv.Atoi(taskID[i+2:])
	if err != nil {
		return 0
	}
	return n
}

// fetchEvidenceFiles 按附件清单从 MinIO 拉取 4 类证据文件。返回的 reader 由
// 调用方(ExtractEvidence)在提取完成后关闭。
func (a *Acts) fetchEvidenceFiles(ctx context.Context, atts []wf.Attachment) (map[string]io.Reader, []string) {
	byName := map[string]string{} // 证据文件名 → object key
	for _, att := range atts {
		if _, ok := evidenceFileKey[att.Name]; ok {
			byName[att.Name] = att.ObjectKey
		}
	}
	var cli *minio.Client
	if a.Cfg.presignEnabled() {
		if c, err := evidenceClient(a.Cfg); err != nil {
			a.warnf("minio evidence client init failed: %v; all evidence files missing", err)
		} else {
			cli = c
		}
	}
	files := map[string]io.Reader{}
	missing := []string{}
	for _, name := range evidenceFileNames {
		key, ok := byName[name]
		if !ok || cli == nil {
			missing = append(missing, name)
			continue
		}
		// 先 Stat 确认对象存在:GetObject 是惰性的,不存在要等 Read 才报错,
		// 提前识别才能正确计入 missing(降级语义,§3.7)。
		if _, err := cli.StatObject(ctx, a.Cfg.MinIOBucket, key, minio.StatObjectOptions{}); err != nil {
			a.warnf("evidence stat %s failed: %v", key, err)
			missing = append(missing, name)
			continue
		}
		obj, err := cli.GetObject(ctx, a.Cfg.MinIOBucket, key, minio.GetObjectOptions{})
		if err != nil {
			a.warnf("evidence get %s failed: %v", key, err)
			missing = append(missing, name)
			continue
		}
		files[evidenceFileKey[name]] = obj
	}
	return files, missing
}

// evidenceClient 用集群内 endpoint 构造 MinIO 客户端(读路径);
// 与 presign.NewSigner(纯离线签名,用 public host,见 internal/presign)不同,
// 这里发起真实网络请求,必须用集群内可达的 MINIO_ENDPOINT。
func evidenceClient(c Config) (*minio.Client, error) {
	secure := false
	host := c.MinIOEndpoint
	if u, err := url.Parse(c.MinIOEndpoint); err == nil && u.Host != "" {
		host = u.Host
		secure = u.Scheme == "https"
	}
	return minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(c.MinIOAccessKey, c.MinIOSecretKey, ""),
		Secure: secure,
	})
}

// computeMetricsBaseline 对每条指标计算与历史 PASSED 中位数的差值。
// baselineBaselineN 是基线样本数上限(§10 缺省 5,对应 Plan DSL 缺省策略)。
const baselineSampleN = 5

func computeMetricsBaseline(
	ctx context.Context,
	st Store,
	project, variant string,
	metrics map[string]float64,
) evidence.MetricsBaselineMap {
	out := make(evidence.MetricsBaselineMap, len(metrics))
	for name, val := range metrics {
		mb := evidence.MetricsBaseline{Value: val}
		bl, err := st.Baseline(ctx, project, variant, "smoke", name, baselineSampleN)
		if err != nil || bl == nil {
			out[name] = mb
			continue
		}
		mb.SampleN = bl.N
		delta := val - bl.Median
		pct := 0.0
		if bl.Median != 0 {
			pct = delta / bl.Median * 100
		}
		mb.Baseline = &bl.Median
		mb.Delta = &delta
		mb.DeltaPct = &pct
		out[name] = mb
	}
	return out
}

// Analyze 调 hermes-agent 平台分析 evidence(§12 Phase 2)。
// Analyzer 未启用(Hermes 为 nil)返回 (nil, nil):workflow 跳过,规则引擎保底;
// 平台失败返回 error,由 workflow 降级——verdict 判定权永远在规则引擎(§9)。
func (a *Acts) Analyze(ctx context.Context, req wf.AnalyzeRequest) (*hermesclient.Analysis, error) {
	if a.Hermes == nil {
		return nil, nil
	}
	return a.Hermes.Analyze(ctx, hermesclient.AnalyzeRequest{
		TaskID:       req.TaskID,
		RuleCategory: req.RuleCategory,
		Model:        a.Cfg.HermesModel,
		Evidence:     req.EvidenceJSON,
	})
}
