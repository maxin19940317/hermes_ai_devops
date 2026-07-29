package callbacks

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hermes-devops/runtime/internal/presign"
	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

// 端点唯一的门禁是租约凭据;凭据不对必须一个 URL 都签不出来。
func TestUploadRequestsRejectsBadLease(t *testing.T) {
	h, cred := newUploadHandler(t)
	cases := []struct {
		name   string
		mutate func(m map[string]any)
	}{
		{"错 lease_id", func(m map[string]any) { m["lease_id"] = "bogus" }},
		{"错 generation", func(m map[string]any) { m["lease_generation"] = 99 }},
		{"错 client_id", func(m map[string]any) { m["client_id"] = "other" }},
		{"错 task_id", func(m map[string]any) { m["task_id"] = "w:other:a1" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := cred()
			tc.mutate(body)
			rec := postUpload(t, h, body)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("code = %d, want 401", rec.Code)
			}
			if strings.Contains(rec.Body.String(), "http") {
				t.Error("401 响应不得包含任何 URL")
			}
		})
	}
}

// 安全性质:签发出的 key 一律在 runs/{task_id}/ 内,路径逃逸进 rejected。
func TestUploadRequestsKeyConfinement(t *testing.T) {
	h, cred := newUploadHandler(t)
	body := cred()
	body["files"] = []string{
		"results/result.json", "dumps/0001.bin",
		"../../etc/passwd", "/etc/shadow", "a/../../b", "",
	}
	rec := postUpload(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	// 字段须带 json tag:响应是 snake_case(object_key),Go 默认的大小写不敏感
	// 匹配不会跨越下划线,少了 tag 会让 ObjectKey 静默保持零值,断言形同虚设。
	var out struct {
		Uploads []struct {
			Path      string `json:"path"`
			ObjectKey string `json:"object_key"`
			URL       string `json:"url"`
		} `json:"uploads"`
		Rejected []struct {
			Path   string `json:"path"`
			Reason string `json:"reason"`
		} `json:"rejected"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Uploads) != 2 {
		t.Errorf("uploads = %d, want 2(仅两个合法路径)", len(out.Uploads))
	}
	if len(out.Rejected) != 4 {
		t.Errorf("rejected = %d, want 4", len(out.Rejected))
	}
	prefix := "runs/" + body["task_id"].(string) + "/"
	for _, u := range out.Uploads {
		if !strings.HasPrefix(u.ObjectKey, prefix) {
			t.Errorf("object_key %q 越出前缀 %q", u.ObjectKey, prefix)
		}
	}
}

// 超上限整体拒绝,不截断——截断会让 Agent 以为传全了。
func TestUploadRequestsRejectsTooManyFiles(t *testing.T) {
	h, cred := newUploadHandler(t)
	body := cred()
	files := make([]string, 65)
	for i := range files {
		files[i] = "logs/f.log"
	}
	body["files"] = files
	if rec := postUpload(t, h, body); rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
}

// MinIO 未配置 → 503,Agent 据此回退。
func TestUploadRequestsWithoutSignerReturns503(t *testing.T) {
	h, cred := newUploadHandler(t)
	h.Presign = nil
	if rec := postUpload(t, h, cred()); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
}

func postUpload(t *testing.T, h *Handler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/callbacks/v1/upload-requests", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	return rec
}

// newUploadHandler 造一个带真实租约的 Handler:注册 client+device、
// AcquireDevice 拿到租约,装配 Presign(假凭据,签名是纯离线操作,不需要真
// MinIO 可达)与 UploadMaxFiles。返回 handler 与一个生成"合法请求体"的闭包。
func newUploadHandler(t *testing.T) (*Handler, func() map[string]any) {
	t.Helper()
	s := store.NewMemStore()
	if err := s.UpsertClientDevices(ctx, store.Client{ClientID: "c1", BaseURL: "https://client:8443"},
		[]store.Device{{DeviceID: "513cd3de", Serial: "513cd3de", ClientID: "c1",
			SOC: "QCM6125", ABI: "arm64-v8a", Capabilities: []string{"hexagon"}}}); err != nil {
		t.Fatal(err)
	}
	taskID := "device-test-algo-super-sdk-g9da3b9d9-p56-aarch64_Android_SNPE_2.21:aarch64_Android_SNPE_2.21:a1"
	lease, err := s.AcquireDevice(ctx, wf.DeviceSelector{SOC: []string{"QCM6125"}}, taskID, 120)
	if err != nil || lease == nil {
		t.Fatalf("acquire: lease=%v err=%v", lease, err)
	}
	signer, err := presign.NewSigner(presign.Config{
		Endpoint: "minio:9000", AccessKey: "ak", SecretKey: "sk", Bucket: "hermes-evidence",
	})
	if err != nil || signer == nil {
		t.Fatalf("NewSigner: %v (signer=%v)", err, signer)
	}
	h := New(s, &fakeSignaler{}, nil, 120)
	h.Presign = signer
	h.UploadMaxFiles = 64
	cred := func() map[string]any {
		return map[string]any{
			"task_id":          taskID,
			"client_id":        "c1",
			"device_id":        lease.DeviceID,
			"attempt":          1,
			"lease_id":         lease.LeaseID,
			"lease_generation": lease.Generation,
			"files":            []string{"results/result.json"},
		}
	}
	return h, cred
}
