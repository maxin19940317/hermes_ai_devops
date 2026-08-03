package activity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

// Dispatch 按 §8.1 POST /api/v1/tasks 派单;非 2xx 返回 error,
// 由 workflow 的 on_infra_error 策略处理。凭据与回调地址由 Config 补充。
func (a *Acts) Dispatch(ctx context.Context, req wf.DispatchRequest) error {
	payload := map[string]any{
		"task_id":         req.TaskID,
		"idempotency_key": req.IdempotencyKey,
		"attempt":         req.Attempt,
		"artifact": map[string]any{
			"url":    req.PackageURL,
			"sha256": req.PackageSHA256,
			"auth":   map[string]any{"type": a.Cfg.ArtifactAuthType, "token": a.Cfg.ArtifactAuthToken, "username": a.Cfg.ArtifactAuthUsername},
		},
		"manifest_digest":    req.ManifestDigest,
		"device_serial":      req.DeviceSerial,
		"callback_base_url":  a.Cfg.CallbackBaseURL,
		"lease_id":           req.LeaseID,
		"lease_generation":   req.LeaseGeneration,
		"presigned_uploads":  a.presignedUploads(ctx, req.TaskID),
		"upload_request_url": uploadRequestURL(a.Cfg.CallbackBaseURL),
	}
	if err := a.post(ctx, req.ClientBaseURL+"/api/v1/tasks", payload, http.StatusAccepted); err != nil {
		return err
	}
	// Phase 3 审计:派单成功 → 落 audit_log(失败不阻断,fire-and-forget)
	a.writeAudit(ctx, store.AuditEntry{
		Actor:  "activity:dispatch",
		Action: "dispatched",
		Target: req.TaskID,
	})
	return nil
}

// uploadRequestURL 由回调基址派生按需签发端点地址(差距 #8)。
// 不新增配置项:与 callback_base_url 同一来源,避免两处配置漂移。
func uploadRequestURL(base string) string {
	if base == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/callbacks/v1/upload-requests"
}

// CancelTask尽力而为取消(§8.1);404 表示 Client 已无此任务,视为成功。
func (a *Acts) CancelTask(ctx context.Context, req wf.CancelRequest) error {
	u := req.ClientBaseURL + "/api/v1/tasks/" + url.PathEscape(req.TaskID)
	hr, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	resp, err := a.HTTP.Do(hr)
	if err != nil {
		return fmt.Errorf("cancel task: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || (resp.StatusCode >= 200 && resp.StatusCode < 300) {
		return nil
	}
	return fmt.Errorf("cancel task: unexpected status %d", resp.StatusCode)
}

// post 发 JSON 并校验预期状态码(2xx 且优先匹配 want)。
func (a *Acts) post(ctx context.Context, u string, payload any, want int) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := a.HTTP.Do(hr)
	if err != nil {
		return fmt.Errorf("post %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("post %s: status %d: %s", u, resp.StatusCode, msg)
	}
	return nil
}
