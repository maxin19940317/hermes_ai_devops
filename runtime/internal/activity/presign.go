package activity

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// EvidenceFiles 是 dispatch 时按 §3.7 固定预签的证据键集;
// 键形如 runs/{task_id}/{file}。glob 命中的集合外文件本轮不上传(CONTRACT-ISSUE,§1)。
var EvidenceFiles = []string{"result.json", "junit.xml", "logcat.txt", "stdout.log", "stderr.log"}

// PresignedUpload 是 §8.1 派单载荷 presigned_uploads[] 的条目。
type PresignedUpload struct {
	ObjectKey string `json:"object_key"`
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
}

// presignEnabled:endpoint 或凭据缺失即禁用(优雅降级,非错误,§3.7)。
func (c Config) presignEnabled() bool {
	return c.MinIOEndpoint != "" && c.MinIOAccessKey != "" && c.MinIOSecretKey != ""
}

// presignClient 构造 minio client。AWS V4 签名覆盖 Host 头,因此 client 必须用
// MINIO_PUBLIC_ENDPOINT 的 host 构造——预签名是纯离线操作(不发起网络请求),
// 集群内不可达的 public host 不影响签名;事后改写 URL host 会使签名失效,不可取。
// MINIO_PUBLIC_ENDPOINT 为空时退回 MINIO_ENDPOINT(仅同 host 可达时正确)。
func presignClient(c Config) (*minio.Client, error) {
	endpoint := c.MinIOPublicEndpoint
	if endpoint == "" {
		endpoint = c.MinIOEndpoint
	}
	secure := false
	host := endpoint
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		host = u.Host
		secure = u.Scheme == "https"
	}
	// Region 固定:不设则 minio-go 预签时会先发起 GetBucketLocation 网络请求,
	// 而预签名必须是纯离线操作(dispatch 活动不应依赖 MinIO 可达性)。
	return minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(c.MinIOAccessKey, c.MinIOSecretKey, ""),
		Secure: secure,
		Region: "us-east-1",
	})
}

// presignedUploads 对固定键集预签 PUT。任何失败降级为空集(附件缺失不构成
// INFRA 重试理由,结果回流优先,§3.7)。预签名 URL 含签名,永不落日志;只记 object key。
func (a *Acts) presignedUploads(ctx context.Context, taskID string) []PresignedUpload {
	uploads := []PresignedUpload{}
	if !a.Cfg.presignEnabled() {
		a.warnf("minio presigning disabled (endpoint/credentials empty); presigned_uploads empty")
		return uploads
	}
	cli, err := presignClient(a.Cfg)
	if err != nil {
		a.warnf("minio presign client init failed: %v; presigned_uploads empty", err)
		return uploads
	}
	ttl := a.Cfg.MinIOPresignTTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	bucket := a.Cfg.MinIOBucket
	expires := time.Now().UTC().Add(ttl)
	for _, f := range EvidenceFiles {
		key := fmt.Sprintf("runs/%s/%s", taskID, f)
		u, err := cli.PresignedPutObject(ctx, bucket, key, ttl)
		if err != nil {
			a.warnf("minio presign PUT failed for key %s: %v; presigned_uploads empty", key, err)
			return []PresignedUpload{}
		}
		uploads = append(uploads, PresignedUpload{
			ObjectKey: key,
			URL:       u.String(),
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
