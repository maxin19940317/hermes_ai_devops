package activity

import (
	"context"
	"fmt"
	"time"

	"hermes-devops/runtime/internal/presign"
)

// EvidenceFiles 是 dispatch 时预签的固定键集。差距 #8 之后它只是**回退路径**:
// 正常路径由 Agent 在收集完成后经 callbacks 的 upload-requests 按需换取 URL,
// glob 命中的文件(logs/*.log、dumps/**)因此已能上传——原 CONTRACT-ISSUE 关闭,
// 详见 docs/superpowers/specs/2026-07-29-on-demand-presign-design.md。
var EvidenceFiles = []string{"result.json", "junit.xml", "logcat.txt", "stdout.log", "stderr.log"}

// PresignedUpload 是 §8.1 派单载荷 presigned_uploads[] 的条目。
type PresignedUpload struct {
	ObjectKey string `json:"object_key"`
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
}

// presignEnabled:endpoint 或凭据缺失即禁用(优雅降级,非错误,§3.7)。
func (c Config) presignEnabled() bool { return c.presignConfig().Enabled() }

// presignConfig 把 activity 配置投影成签名包的配置。
func (c Config) presignConfig() presign.Config {
	return presign.Config{
		Endpoint: c.MinIOEndpoint, PublicEndpoint: c.MinIOPublicEndpoint,
		AccessKey: c.MinIOAccessKey, SecretKey: c.MinIOSecretKey,
		Bucket: c.MinIOBucket, TTL: c.MinIOPresignTTL,
	}
}

// presignedUploads 对固定键集预签 PUT。任何失败降级为空集(附件缺失不构成
// INFRA 重试理由,结果回流优先,§3.7)。预签名 URL 含签名,永不落日志;只记 object key。
func (a *Acts) presignedUploads(ctx context.Context, taskID string) []PresignedUpload {
	uploads := []PresignedUpload{}
	if !a.Cfg.presignEnabled() {
		a.warnf("minio presigning disabled (endpoint/credentials empty); presigned_uploads empty")
		return uploads
	}
	signer, err := presign.NewSigner(a.Cfg.presignConfig())
	if err != nil {
		a.warnf("minio presign client init failed: %v; presigned_uploads empty", err)
		return uploads
	}
	for _, f := range EvidenceFiles {
		key := fmt.Sprintf("runs/%s/%s", taskID, f)
		u, expires, err := signer.PutURL(ctx, key)
		if err != nil {
			a.warnf("minio presign PUT failed for key %s: %v; presigned_uploads empty", key, err)
			return []PresignedUpload{}
		}
		uploads = append(uploads, PresignedUpload{
			ObjectKey: key,
			URL:       u,
			ExpiresAt: expires.Format(time.RFC3339),
		})
	}
	return uploads
}

// warnf 在 Acts.Log 存在时记 warn;测试装配可不设 Log。
func (a *Acts) warnf(format string, args ...any) {
	if a.Log != nil {
		a.Log.Warn().Msgf(format, args...)
	}
}
