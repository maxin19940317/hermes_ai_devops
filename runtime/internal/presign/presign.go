// Package presign 集中 MinIO 预签名 PUT 的构造与签发(§3.7)。
// 抽成独立包是因为两处需要它:activity(派单时的固定键集)与 callbacks
// (收集时的按需签发,差距 #8),而 callbacks.Handler 本身不持有 MinIO 配置。
package presign

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config 是签名所需的全部配置。
type Config struct {
	Endpoint       string // 集群内地址,仅在 PublicEndpoint 为空时用于签名
	PublicEndpoint string // 预签名 URL 的 host,须为 Client 可达
	AccessKey      string
	SecretKey      string
	Bucket         string
	TTL            time.Duration // <=0 时缺省 1h
}

// Enabled:endpoint 或凭据缺失即禁用(优雅降级,非错误,§3.7)。
func (c Config) Enabled() bool {
	return c.Endpoint != "" && c.AccessKey != "" && c.SecretKey != ""
}

// Signer 签发预签名 PUT URL。
type Signer struct {
	cli    *minio.Client
	bucket string
	ttl    time.Duration
}

// NewSigner 构造签发器。未启用时返回 (nil, nil)——调用方据此判定"降级",
// 这不是错误(§3.7:MinIO 缺失时附件留本地,结果照常回流)。
func NewSigner(c Config) (*Signer, error) {
	if !c.Enabled() {
		return nil, nil
	}
	// AWS V4 签名覆盖 Host 头,因此 client 必须用 PublicEndpoint 的 host 构造——
	// 预签名是纯离线操作(不发起网络请求),集群内不可达的 public host 不影响签名;
	// 事后改写 URL host 会使签名失效,不可取。PublicEndpoint 为空时退回 Endpoint
	// (仅同 host 可达时正确)。
	endpoint := c.PublicEndpoint
	if endpoint == "" {
		endpoint = c.Endpoint
	}
	secure := false
	host := endpoint
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		host = u.Host
		secure = u.Scheme == "https"
	}
	// Region 固定:不设则 minio-go 预签时会先发起 GetBucketLocation 网络请求,
	// 而预签名必须是纯离线操作(dispatch 活动不应依赖 MinIO 可达性)。
	cli, err := minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(c.AccessKey, c.SecretKey, ""),
		Secure: secure,
		Region: "us-east-1",
	})
	if err != nil {
		return nil, fmt.Errorf("presign: 构造 minio client 失败: %w", err)
	}
	ttl := c.TTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &Signer{cli: cli, bucket: c.Bucket, ttl: ttl}, nil
}

// TTL 返回签发使用的有效期。
func (s *Signer) TTL() time.Duration { return s.ttl }

// PutURL 为单个 object key 签发 PUT URL,并返回其过期时刻。
// URL 含签名,调用方**不得**落日志(只记 object key)。
func (s *Signer) PutURL(ctx context.Context, key string) (string, time.Time, error) {
	u, err := s.cli.PresignedPutObject(ctx, s.bucket, key, s.ttl)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("presign: 签发 %s 失败: %w", key, err)
	}
	return u.String(), time.Now().UTC().Add(s.ttl), nil
}
