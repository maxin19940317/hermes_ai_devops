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
		CallbackBaseURL: "https://runtime:8091", ArtifactAuthType: "basic",
		ArtifactAuthToken: "tok", ArtifactAuthUsername: "deploy-user"}}
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
		auth["type"] != "basic" || auth["token"] != "tok" || auth["username"] != "deploy-user" {
		t.Errorf("artifact = %v", art)
	}
}

// TestDispatchAuthScopedToGitLab:配置 ArtifactAuthGitLabBase 后,仅该基址
// 下的 URL 携带凭据;其余来源(MinIO)auth 为空(匿名下载)。
func TestDispatchAuthScopedToGitLab(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	a := &Acts{HTTP: srv.Client(), Cfg: Config{
		CallbackBaseURL: "https://runtime:8091",
		ArtifactAuthType: "basic", ArtifactAuthToken: "tok", ArtifactAuthUsername: "deploy-user",
		ArtifactAuthGitLabBase: "https://gitlab.example",
	}}

	// GitLab URL → 带 basic 凭据
	err := a.Dispatch(ctx, wf.DispatchRequest{
		TaskID: "w:t:a1", IdempotencyKey: "w:t:a1", Attempt: 1,
		PackageURL: "https://gitlab.example/api/v4/projects/1/packages/generic/x/1/y.tar.gz",
		PackageSHA256: "ab12", ManifestDigest: "cd34",
		DeviceSerial: "513cd3de", ClientBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	auth := got["artifact"].(map[string]any)["auth"].(map[string]any)
	if auth["type"] != "basic" || auth["token"] != "tok" || auth["username"] != "deploy-user" {
		t.Errorf("GitLab URL auth = %v, want basic/tok/deploy-user", auth)
	}

	// MinIO URL → 匿名(type=none)
	err = a.Dispatch(ctx, wf.DispatchRequest{
		TaskID: "w:t:a2", IdempotencyKey: "w:t:a2", Attempt: 1,
		PackageURL: "http://10.88.118.251:9000/hermes-packages/x.tar.gz",
		PackageSHA256: "ab12", ManifestDigest: "cd34",
		DeviceSerial: "513cd3de", ClientBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	auth = got["artifact"].(map[string]any)["auth"].(map[string]any)
	if auth["type"] != "none" || auth["token"] != "" || auth["username"] != "" {
		t.Errorf("MinIO URL auth = %v, want type=none(匿名)", auth)
	}
}

// TestDispatchAuthNoGitLabBaseKeepsLegacy:未配置 ArtifactAuthGitLabBase
// 时,所有 URL 都带凭据(旧行为,向后兼容)。
func TestDispatchAuthNoGitLabBaseKeepsLegacy(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	a := &Acts{HTTP: srv.Client(), Cfg: Config{
		ArtifactAuthType: "basic", ArtifactAuthToken: "tok", ArtifactAuthUsername: "deploy-user",
	}}
	err := a.Dispatch(ctx, wf.DispatchRequest{
		TaskID: "w:t:a1", IdempotencyKey: "w:t:a1", Attempt: 1,
		PackageURL: "http://10.88.118.251:9000/hermes-packages/x.tar.gz",
		ClientBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	auth := got["artifact"].(map[string]any)["auth"].(map[string]any)
	if auth["type"] != "basic" {
		t.Errorf("无 GitLabBase: auth = %v, want basic(旧行为)", auth)
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
