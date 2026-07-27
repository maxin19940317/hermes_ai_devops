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
// 与 presignClient(纯离线签名,用 public host)不同,这里发起真实网络请求,
// 必须用集群内可达的 MINIO_ENDPOINT。
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
