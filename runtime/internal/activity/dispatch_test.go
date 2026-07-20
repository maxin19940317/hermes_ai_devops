package activity

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	wf "hermes-devops/runtime/internal/workflow"
)

func TestDispatchPostsContractPayload(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/tasks" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	a := &Acts{HTTP: srv.Client(), Cfg: Config{
		CallbackBaseURL: "https://runtime:8091", ArtifactAuthType: "bearer", ArtifactAuthToken: "tok"}}
	err := a.Dispatch(ctx, wf.DispatchRequest{
		TaskID: "w:t:a1", IdempotencyKey: "w:t:a1", Attempt: 1,
		PackageURL: "https://gitlab/pkg", PackageSHA256: "ab12", ManifestDigest: "cd34",
		DeviceSerial: "513cd3de", ClientBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	// §8.1 TaskDispatchRequest 必填字段
	if got["task_id"] != "w:t:a1" || got["idempotency_key"] != "w:t:a1" ||
		got["manifest_digest"] != "cd34" || got["device_serial"] != "513cd3de" ||
		got["callback_base_url"] != "https://runtime:8091" {
		t.Errorf("payload = %v", got)
	}
	art := got["artifact"].(map[string]any)
	auth := art["auth"].(map[string]any)
	if art["url"] != "https://gitlab/pkg" || art["sha256"] != "ab12" ||
		auth["type"] != "bearer" || auth["token"] != "tok" {
		t.Errorf("artifact = %v", art)
	}
}

func TestDispatchNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"code":"version_too_low","message":"agent too old"}`, http.StatusUnprocessableEntity)
	}))
	defer srv.Close()
	a := &Acts{HTTP: srv.Client(), Cfg: Config{ArtifactAuthType: "bearer", ArtifactAuthToken: "t"}}
	if err := a.Dispatch(ctx, wf.DispatchRequest{TaskID: "t", ClientBaseURL: srv.URL}); err == nil {
		t.Error("422 应返回 error(触发活动重试/INFRA 处理)")
	}
}

func TestCancelTask404IsOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/tasks/w:t:a1" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	a := &Acts{HTTP: srv.Client()}
	if err := a.CancelTask(ctx, wf.CancelRequest{TaskID: "w:t:a1", ClientBaseURL: srv.URL}); err != nil {
		t.Errorf("404(任务已不存在)应视为取消成功: %v", err)
	}
}
