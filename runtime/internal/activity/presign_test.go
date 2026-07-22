package activity

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	wf "hermes-devops/runtime/internal/workflow"
)

func presignTestConfig() Config {
	return Config{
		MinIOEndpoint:       "minio:9000",
		MinIOPublicEndpoint: "http://10.88.118.251:9000",
		MinIOAccessKey:      "minioadmin",
		MinIOSecretKey:      "minioadmin-secret",
		MinIOBucket:         "hermes-evidence",
		MinIOPresignTTL:     time.Hour,
	}
}

func TestPresignedUploadsFixedKeySet(t *testing.T) {
	a := &Acts{Cfg: presignTestConfig()}

	before := time.Now().UTC()
	uploads := a.presignedUploads(ctx, "w:t:a1")
	after := time.Now().UTC()

	if len(uploads) != len(EvidenceFiles) {
		t.Fatalf("got %d uploads, want %d: %v", len(uploads), len(EvidenceFiles), uploads)
	}
	wantKeys := map[string]bool{}
	for _, f := range EvidenceFiles {
		wantKeys["runs/w:t:a1/"+f] = false
	}
	for _, u := range uploads {
		if _, ok := wantKeys[u.ObjectKey]; !ok {
			t.Errorf("unexpected object_key %q", u.ObjectKey)
			continue
		}
		wantKeys[u.ObjectKey] = true

		// 签名覆盖 Host:URL 必须是 public endpoint,而非集群内 minio:9000。
		pu, err := url.Parse(u.URL)
		if err != nil {
			t.Errorf("key %s: unparseable url: %v", u.ObjectKey, err)
			continue
		}
		if pu.Host != "10.88.118.251:9000" {
			t.Errorf("key %s: url host = %q, want public endpoint", u.ObjectKey, pu.Host)
		}
		if !strings.HasPrefix(pu.Path, "/hermes-evidence/runs/w:t:a1/") {
			t.Errorf("key %s: url path = %q", u.ObjectKey, pu.Path)
		}
		if pu.Query().Get("X-Amz-Signature") == "" {
			t.Errorf("key %s: url missing signature", u.ObjectKey)
		}
		// X-Amz-Expires 应等于 TTL 秒数。
		if exp, _ := strconv.Atoi(pu.Query().Get("X-Amz-Expires")); exp != 3600 {
			t.Errorf("key %s: X-Amz-Expires = %d, want 3600", u.ObjectKey, exp)
		}
		// expires_at 应落在 [before+TTL, after+TTL] 窗口内。
		exp, err := time.Parse(time.RFC3339, u.ExpiresAt)
		if err != nil {
			t.Errorf("key %s: bad expires_at %q: %v", u.ObjectKey, u.ExpiresAt, err)
			continue
		}
		// expires_at 为 RFC3339 秒级截断,窗口放宽 1s 容差。
		if exp.Before(before.Add(time.Hour-time.Second)) || exp.After(after.Add(time.Hour+time.Second)) {
			t.Errorf("key %s: expires_at %v outside TTL window", u.ObjectKey, exp)
		}
	}
	for key, seen := range wantKeys {
		if !seen {
			t.Errorf("missing expected key %s", key)
		}
	}
}

func TestPresignedUploadsDisabledWhenUnconfigured(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no endpoint":    {MinIOAccessKey: "ak", MinIOSecretKey: "sk"},
		"no credentials": {MinIOEndpoint: "minio:9000"},
		"empty":          {},
	} {
		t.Run(name, func(t *testing.T) {
			a := &Acts{Cfg: cfg}
			if got := a.presignedUploads(ctx, "t"); len(got) != 0 {
				t.Errorf("disabled presigning should yield empty set, got %v", got)
			}
		})
	}
}

// 禁用预签名时 dispatch 仍须按契约成功,presigned_uploads 为空数组(优雅降级,§3.7)。
func TestDispatchWorksWithPresigningDisabled(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	a := &Acts{HTTP: srv.Client(), Cfg: Config{ArtifactAuthType: "bearer", ArtifactAuthToken: "t"}}
	if err := a.Dispatch(ctx, wf.DispatchRequest{TaskID: "w:t:a1", ClientBaseURL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	up, ok := got["presigned_uploads"].([]any)
	if !ok {
		t.Fatalf("presigned_uploads missing or not an array: %v", got["presigned_uploads"])
	}
	if len(up) != 0 {
		t.Errorf("presigned_uploads should be empty when disabled, got %v", up)
	}
}

// 启用预签名时 dispatch 载荷携带 5 个固定键的预签名条目。
func TestDispatchCarriesPresignedUploads(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	a := &Acts{HTTP: srv.Client(), Cfg: presignTestConfig()}
	if err := a.Dispatch(ctx, wf.DispatchRequest{TaskID: "w:t:a1", ClientBaseURL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	up, ok := got["presigned_uploads"].([]any)
	if !ok || len(up) != len(EvidenceFiles) {
		t.Fatalf("presigned_uploads = %v", got["presigned_uploads"])
	}
	for _, item := range up {
		m := item.(map[string]any)
		key, _ := m["object_key"].(string)
		if !strings.HasPrefix(key, "runs/w:t:a1/") {
			t.Errorf("object_key = %v", m["object_key"])
		}
		urlStr, _ := m["url"].(string)
		if !strings.Contains(urlStr, "10.88.118.251:9000") {
			t.Errorf("url missing public host: %v", m["url"])
		}
		if _, ok := m["expires_at"].(string); !ok {
			t.Errorf("expires_at missing: %v", m)
		}
	}
}
