package trigger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hermes-devops/runtime/internal/store"
)

// fakeProber 记录探活调用并按预设失败。
type fakeProber struct {
	err   error
	calls int
	gotURL string
}

func (f *fakeProber) PackageExists(_ context.Context, url string) error {
	f.calls++
	f.gotURL = url
	return f.err
}

const kickGitLabBase = "https://gitlab.example"

func validKick() map[string]any {
	return map[string]any{
		"variant":           "aarch64_Android_SNPE_2.21",
		"package_file":      "algo-super-sdk-aarch64_Android_SNPE_2.21-g8e981b96-p48.tar.gz",
		"url":               kickGitLabBase + "/api/v4/projects/651/packages/generic/algo-super-sdk/1.0.2/pkg.tar.gz",
		"sha256":            strings.Repeat("a", 64),
		"size":              83188921,
		"manifest_digest":   "sha256:deadbeef",
		"version":           "1.0.2",
		"project":           "aios/algo_super_sdk",
		"commit":            "8e981b96",
		"pipeline_id":       48,
		"pipeline_global_id": 712,
	}
}

func newKickHandler(starter *fakeStarter, prober *fakeProber) (*Handler, *store.MemStore) {
	st := store.NewMemStore()
	h, err := New(Config{
		WebhookSecret: testSecret,
		Refs:          []string{"master"},
		GitLabBaseURL: kickGitLabBase,
	}, &fakeFetcher{}, st, starter)
	if err != nil {
		panic(err)
	}
	h.Prober = prober
	return h, st
}

func postKick(h *Handler, token string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/kick", bytes.NewReader(body))
	if token != "" {
		req.Header.Set("X-Gitlab-Token", token)
	}
	rec := httptest.NewRecorder()
	h.HandleKick(rec, req)
	return rec
}

func TestKickHappyPath(t *testing.T) {
	starter, prober := &fakeStarter{started: true}, &fakeProber{}
	h, st := newKickHandler(starter, prober)
	rec := postKick(h, testSecret, mustJSON(t, validKick()))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body)
	}
	// 产物登记
	arts := st.Artifacts()
	if len(arts) != 1 || arts[0].Variant != "aarch64_Android_SNPE_2.21" || arts[0].PipelineID != 48 {
		t.Errorf("artifacts = %+v", arts)
	}
	// workflow 输入:单包 + Scope=variant,ID 带变体后缀(与 bundle 路径不撞)
	in := starter.gotInput
	if in.Scope != "aarch64_Android_SNPE_2.21" || len(in.Packages) != 1 ||
		in.Packages[0].URL != validKick()["url"] {
		t.Errorf("input = %+v", in)
	}
	if !strings.HasSuffix(starter.gotWFID, "-aarch64_Android_SNPE_2.21") {
		t.Errorf("workflow id = %q, want 变体后缀", starter.gotWFID)
	}
	if prober.calls != 1 || prober.gotURL != validKick()["url"] {
		t.Errorf("prober = %+v", prober)
	}
}

func TestKickDuplicateDeliveryIdempotent(t *testing.T) {
	starter := &fakeStarter{started: false} // 同 ID 已存在 → 幂等成功
	h, _ := newKickHandler(starter, &fakeProber{})
	rec := postKick(h, testSecret, mustJSON(t, validKick()))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("重复 kick 应幂等 202, got %d", rec.Code)
	}
	var resp map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if resp["started"] != false {
		t.Errorf("started = %v, want false", resp["started"])
	}
}

func TestKickAuthRequired(t *testing.T) {
	starter := &fakeStarter{}
	h, _ := newKickHandler(starter, &fakeProber{})
	if rec := postKick(h, "", mustJSON(t, validKick())); rec.Code != http.StatusUnauthorized {
		t.Errorf("无 token: code = %d, want 401", rec.Code)
	}
	if rec := postKick(h, "wrong", mustJSON(t, validKick())); rec.Code != http.StatusUnauthorized {
		t.Errorf("错 token: code = %d, want 401", rec.Code)
	}
	if starter.calls != 0 {
		t.Error("鉴权失败不得启动 workflow")
	}
}

func TestKickValidation(t *testing.T) {
	cases := map[string]func(map[string]any){
		"bad variant":    func(m map[string]any) { m["variant"] = "bad/variant" },
		"bad commit":     func(m map[string]any) { m["commit"] = "xyz" },
		"bad sha256":     func(m map[string]any) { m["sha256"] = "aa" },
		"zero size":      func(m map[string]any) { m["size"] = 0 },
		"missing digest": func(m map[string]any) { m["manifest_digest"] = "" },
		"bad pipeline":   func(m map[string]any) { m["pipeline_id"] = 0 },
		"foreign url":    func(m map[string]any) { m["url"] = "https://evil.example/api/v4/projects/1/x" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			starter, prober := &fakeStarter{}, &fakeProber{}
			h, _ := newKickHandler(starter, prober)
			m := validKick()
			mutate(m)
			rec := postKick(h, testSecret, mustJSON(t, m))
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("code = %d, want 422", rec.Code)
			}
			if starter.calls != 0 || prober.calls != 0 {
				t.Error("校验失败不得探活/启动")
			}
		})
	}
}

func TestKickProbeFailure(t *testing.T) {
	starter := &fakeStarter{}
	h, st := newKickHandler(starter, &fakeProber{err: errors.New("status 404")})
	rec := postKick(h, testSecret, mustJSON(t, validKick()))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code = %d, want 502(产物不存在/不可达)", rec.Code)
	}
	if starter.calls != 0 || len(st.Artifacts()) != 0 {
		t.Error("探活失败不得登记/启动")
	}
}

func TestPipelineWebhookDisabledInKickMode(t *testing.T) {
	fetcher, starter := &fakeFetcher{}, &fakeStarter{}
	st := store.NewMemStore()
	h, err := New(Config{
		WebhookSecret:           testSecret,
		Refs:                    []string{"master"},
		PipelineWebhookDisabled: true,
	}, fetcher, st, starter)
	if err != nil {
		t.Fatal(err)
	}
	rec := post(h, testSecret, pipelinePayloadWithIDs("success", "master", fullSHA, "712", "651"))
	if rec.Code != http.StatusNoContent {
		t.Errorf("code = %d, want 204(kick 模式下 webhook 仅记录)", rec.Code)
	}
	if fetcher.calls != 0 || starter.calls != 0 {
		t.Error("kick 模式下 webhook 不得拉 bundle/起 workflow")
	}
}
