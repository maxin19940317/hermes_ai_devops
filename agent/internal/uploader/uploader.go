// Package uploader 负责把收集到的附件经预签名 URL 直传 MinIO(红线 §14:
// 附件不经 Runtime 中转)。单项失败/过期降级为"该附件未上传,本地保留",
// 不阻断结果回流(设计 §3.4)。
package uploader

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"hermes-devops/agent/internal/artifact"
	"hermes-devops/agent/internal/reporter"
)

// DefaultUploadTimeout 是单个附件上传的默认超时。
const DefaultUploadTimeout = 2 * time.Minute

// PresignedUpload 对应契约 TaskDispatchRequest.presigned_uploads[] 的单项
// (contracts/client-agent-api.openapi.yaml);ExpiresAt 为零值表示未提供,
// 视为不过期。
type PresignedUpload struct {
	ObjectKey string
	URL       string
	ExpiresAt time.Time
}

// Uploader 按 dispatch 载荷的 presigned_uploads[] 逐项 PUT 附件。
// 预签名 URL 携带签名,严禁落日志——所有日志只记 object_key(§6)。
type Uploader struct {
	Client *http.Client                     // nil → http.DefaultClient
	Logf   func(format string, args ...any) // nil → 静默

	Timeout time.Duration // 单项上传超时;0 → DefaultUploadTimeout
}

func (u *Uploader) client() *http.Client {
	if u.Client != nil {
		return u.Client
	}
	return http.DefaultClient
}

func (u *Uploader) timeout() time.Duration {
	if u.Timeout > 0 {
		return u.Timeout
	}
	return DefaultUploadTimeout
}

func (u *Uploader) logf(format string, args ...any) {
	if u.Logf != nil {
		u.Logf(format, args...)
	}
}

// Upload 按 presigned 输入顺序逐项上传 files[object_key] 指向的本地文件,
// 返回成功项的附件清单(顺序与输入一致,确定性)。
//
// 降级语义(设计 §3.4,附件缺失不阻断结果回流):
//   - 本地文件不存在(测试可能不产出全部固定键集)→ 跳过,非错误;
//   - ExpiresAt 已过 → 跳过,不发 HTTP;
//   - 单项 4xx/5xx/网络错误 → 记日志后跳过,不影响其余项。
//
// 本函数不返回错误:任何单项失败都已降级处理。
func (u *Uploader) Upload(ctx context.Context, presigned []PresignedUpload, files map[string]string) []reporter.Attachment {
	attachments := make([]reporter.Attachment, 0, len(presigned))
	for _, p := range presigned {
		att, ok := u.uploadOne(ctx, p, files)
		if ok {
			attachments = append(attachments, att)
		}
	}
	return attachments
}

// uploadOne 上传单项;ok=false 表示已按降级语义跳过。
func (u *Uploader) uploadOne(ctx context.Context, p PresignedUpload, files map[string]string) (reporter.Attachment, bool) {
	path, ok := files[p.ObjectKey]
	if !ok {
		u.logf("uploader: %s 无本地文件映射,跳过", p.ObjectKey)
		return reporter.Attachment{}, false
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 收集清单是固定键集,任务未必产出每一项——正常,非错误
			u.logf("uploader: %s 本地文件不存在(%s),跳过", p.ObjectKey, path)
		} else {
			u.logf("uploader: %s 无法读取文件状态: %v", p.ObjectKey, err)
		}
		return reporter.Attachment{}, false
	}
	if !p.ExpiresAt.IsZero() && time.Now().After(p.ExpiresAt) {
		u.logf("uploader: %s 预签名已过期(%s),跳过上传,附件本地保留", p.ObjectKey, p.ExpiresAt.UTC().Format(time.RFC3339))
		return reporter.Attachment{}, false
	}

	if err := u.put(ctx, p, path, info.Size()); err != nil {
		u.logf("uploader: %s 上传失败,附件本地保留,不阻断结果回流: %v", p.ObjectKey, err)
		return reporter.Attachment{}, false
	}

	sum, err := artifact.SHA256File(path)
	if err != nil {
		u.logf("uploader: %s 上传成功但 sha256 计算失败,按未上传处理: %v", p.ObjectKey, err)
		return reporter.Attachment{}, false
	}
	return reporter.Attachment{
		Name:      filepath.Base(path),
		ObjectKey: p.ObjectKey,
		SHA256:    sum,
		Size:      info.Size(),
	}, true
}

// put 以精确 Content-Length PUT 文件;除 Content-Length 外不加任何请求头
// (预签名已固化签名所需头)。非 2xx 视为失败。
func (u *Uploader) put(ctx context.Context, p PresignedUpload, path string, size int64) error {
	ctx, cancel := context.WithTimeout(ctx, u.timeout())
	defer cancel()

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, p.URL, f)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.ContentLength = size
	if size == 0 {
		// Go 对"非 nil 但长度为 0 的 body"按未知长度处理,改用 chunked
		// 编码;S3/MinIO 对 chunked PUT 一律 411。换成 NoBody 后客户端
		// 显式发送 Content-Length: 0。
		req.Body = http.NoBody
	}

	resp, err := u.client().Do(req)
	if err != nil {
		// url.Error 的 Error() 内嵌完整请求 URL(含 X-Amz-Signature),
		// 预签名 URL 永不落日志(设计 §6),解包后只带底层错误。
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("put: %w", ue.Err)
		}
		return fmt.Errorf("put: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("put: HTTP %d", resp.StatusCode)
	}
	return nil
}
