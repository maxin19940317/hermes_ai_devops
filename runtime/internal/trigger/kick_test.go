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
	err    error
	calls  int
	gotURL string
}

func (f *fakeProber) PackageExists(_ context.Context, url string) error {
	f.calls++
	f.gotURL = url
	return f.err
}

const kickGitLabBase = "https://gitlab.example"
const kickMinIOBase = "http://10.88.118.251:9000"

func validKick() map[string]any {
	return map[string]any{
		"variant":            "aarch64_Android_SNPE_2.21",
		"package_file":       "algo-super-sdk-aarch64_Android_SNPE_2.21-g8e981b96-p48.tar.gz",
		"url":                kickGitLabBase + "/api/v4/projects/651/packages/generic/algo-super-sdk/1.0.2/pkg.tar.gz",
		"sha256":             strings.Repeat("a", 64),
		"size":               83188921,
		"manifest_digest":    "sha256:deadbeef",
		"version":            "1.0.2",
		"project":            "aios/algo_super_sdk",
		"commit":             "8e981b96",
		"pipeline_id":        48,
		"pipeline_global_id": 712,
	}
}

func newKickHandler(starter *fakeStarter, prober *fakeProber) (*Handler, *store.MemStore) {
	return newKickHandlerBases(starter, prober, nil)
}

func newKickHandlerBases(starter *fakeStarter, prober *fakeProber, extra []string) (*Handler, *store.MemStore) {
	st := store.NewMemStore()
	h, err := New(Config{
		WebhookSecret:       testSecret,
		Refs:                []string{"master"},
		GitLabBaseURL:       kickGitLabBase,
		AllowedPackageBases: extra,
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

// TestKickExplicitRetry:显式 retry(差距 #11)——retry=true 时同一逻辑键
// (commit,pipeline,variant)原子递增 workflow_attempt,workflow ID 加 -r{N}
// 起新 run;普通 kick(无 retry)Attempt 恒为 0,ID 不变。
func TestKickExplicitRetry(t *testing.T) {
	starter := &fakeStarter{started: true}
	h, _ := newKickHandler(starter, &fakeProber{})

	// 普通触发:Attempt=0,ID 无 -r 后缀
	if rec := postKick(h, testSecret, mustJSON(t, validKick())); rec.Code != http.StatusAccepted {
		t.Fatalf("normal kick: %d", rec.Code)
	}
	if starter.gotInput.Attempt != 0 {
		t.Errorf("普通 kick Attempt = %d, want 0", starter.gotInput.Attempt)
	}
	// 两次显式 retry:N 单调递增 1 → 2,ID 分别为 -r1/-r2
	for want := 1; want <= 2; want++ {
		m := validKick()
		m["retry"] = true
		rec := postKick(h, testSecret, mustJSON(t, m))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("retry #%d: code = %d body = %s", want, rec.Code, rec.Body)
		}
		if starter.gotInput.Attempt != want {
			t.Errorf("retry Attempt = %d, want %d", starter.gotInput.Attempt, want)
		}
		wantSuffix := "-aarch64_Android_SNPE_2.21-r" + string(rune('0'+want))
		if !strings.HasSuffix(starter.gotWFID, wantSuffix) {
			t.Errorf("retry workflow id = %q, want 后缀 %q", starter.gotWFID, wantSuffix)
		}
	}
}

// TestKickRuleVersion:rule_version 透传(差距 #7);缺省补 verdict-rules-v1。
func TestKickRuleVersion(t *testing.T) {
	starter := &fakeStarter{started: true}
	h, _ := newKickHandler(starter, &fakeProber{})
	if rec := postKick(h, testSecret, mustJSON(t, validKick())); rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d", rec.Code)
	}
	if starter.gotInput.RuleVersion != "verdict-rules-v1" {
		t.Errorf("缺省 rule_version = %q", starter.gotInput.RuleVersion)
	}
	m := validKick()
	m["rule_version"] = "verdict-rules-v1"
	if rec := postKick(h, testSecret, mustJSON(t, m)); rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d", rec.Code)
	}
	if starter.gotInput.RuleVersion != "verdict-rules-v1" {
		t.Errorf("显式 rule_version = %q", starter.gotInput.RuleVersion)
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

// TestKickAllowsMinIOBase:AllowedPackageBases 内的 MinIO URL 可派发,
// 且探活按非 GitLab 来源匿名探测(prober 仍收到原 URL)。
func TestKickAllowsMinIOBase(t *testing.T) {
	starter, prober := &fakeStarter{started: true}, &fakeProber{}
	h, st := newKickHandlerBases(starter, prober, []string{kickMinIOBase})

	m := validKick()
	m["url"] = kickMinIOBase + "/bucket/algo-super-sdk-g8e981b96-p48.tar.gz"
	rec := postKick(h, testSecret, mustJSON(t, m))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body)
	}
	if prober.calls != 1 || prober.gotURL != m["url"] {
		t.Errorf("prober = %+v, want url %v", prober, m["url"])
	}
	if len(st.Artifacts()) != 1 || st.Artifacts()[0].URL != m["url"] {
		t.Errorf("artifacts = %+v", st.Artifacts())
	}
}

// TestKickRejectsURLOutsideBases:URL 不属于 GitLab 且不在额外白名单 → 422。
func TestKickRejectsURLOutsideBases(t *testing.T) {
	cases := []string{
		"https://evil.example/api/v4/projects/1/x",        // 其他主机
		"https://gitlab.example.evil.com/api/v4/projects/1/x", // 前缀绕过
		"http://10.88.118.251:9001/bucket/pkg.tar.gz",     // MinIO 但不在白名单
	}
	for _, u := range cases {
		t.Run(u, func(t *testing.T) {
			starter, prober := &fakeStarter{}, &fakeProber{}
			h, _ := newKickHandler(starter, prober) // 未配置 MinIO 白名单
			m := validKick()
			m["url"] = u
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

// TestKickMinIOWithNoGitLabBase:未配置 GitLab 但配置了 MinIO 白名单,
// GitLab URL 被拒、MinIO URL 放行。
func TestKickMinIOWithNoGitLabBase(t *testing.T) {
	starter, prober := &fakeStarter{started: true}, &fakeProber{}
	st := store.NewMemStore()
	h, err := New(Config{
		WebhookSecret:       testSecret,
		Refs:                []string{"master"},
		AllowedPackageBases: []string{kickMinIOBase},
	}, &fakeFetcher{}, st, starter)
	if err != nil {
		t.Fatal(err)
	}
	h.Prober = prober

	m := validKick()
	m["url"] = kickGitLabBase + "/api/v4/projects/651/packages/generic/algo-super-sdk/1.0.2/pkg.tar.gz"
	if rec := postKick(h, testSecret, mustJSON(t, m)); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("无 GitLabBaseURL: GitLab URL code = %d, want 422", rec.Code)
	}
	m["url"] = kickMinIOBase + "/bucket/pkg.tar.gz"
	if rec := postKick(h, testSecret, mustJSON(t, m)); rec.Code != http.StatusAccepted {
		t.Errorf("MinIO URL code = %d, want 202", rec.Code)
	}
}

func TestKickRejectsUncommandableProject(t *testing.T) {
	cases := []struct {
		project string
		want    int
	}{
		{"grp/bad project", http.StatusUnprocessableEntity},
		{"/absolute", http.StatusUnprocessableEntity},
		{"grp//double", http.StatusUnprocessableEntity},
		{"grp/trailing\n", http.StatusUnprocessableEntity},
		{strings.Repeat("a", 257), http.StatusUnprocessableEntity},
		{"grp/good_project", http.StatusAccepted},
	}
	for _, tc := range cases {
		t.Run(tc.project, func(t *testing.T) {
			starter, prober := &fakeStarter{started: true}, &fakeProber{}
			h, _ := newKickHandler(starter, prober)
			payload := validKick()
			payload["project"] = tc.project
			rec := postKick(h, testSecret, mustJSON(t, payload))
			if rec.Code != tc.want {
				t.Fatalf("project %q: code = %d, want %d", tc.project, rec.Code, tc.want)
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

// TestHasPrefixAnyBoundary:hasPrefixAny 必须按路径边界分割,防止主机名前缀绕过。
func TestHasPrefixAnyBoundary(t *testing.T) {
	bases := []string{"https://gitlab.example", kickMinIOBase}
	cases := []struct {
		url  string
		want bool
	}{
		{"https://gitlab.example/api/v4/projects/1/x", true},
		{"https://gitlab.example", true},
		{"https://gitlab.example/api", true},
		{"https://gitlab.example.evil.com/x", false},
		{"https://gitlab.exampleevil.com/x", false},
		{"https://gitlab.example:8443/x", false},
		{kickMinIOBase + "/bucket/pkg.tar.gz", true},
		{kickMinIOBase + ":9000/bucket/x", false}, // 端口重写绕过
		{"https://evil.example/x", false},
	}
	for _, tc := range cases {
		if got := hasPrefixAny(tc.url, bases); got != tc.want {
			t.Errorf("hasPrefixAny(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

// TestPackageExistsAnonymousProbe:非 GitLab 来源的 URL 不携带 GitLab token。
func TestPackageExistsAnonymousProbe(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("PRIVATE-TOKEN")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	gl := &GitLabClient{BaseURL: srv.URL, Token: "secret-token"}

	// URL 属于 GitLab base → 带 token
	if err := gl.PackageExists(context.Background(), srv.URL+"/api/v4/projects/1/packages/generic/x/1/y.tar.gz"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "secret-token" {
		t.Errorf("GitLab URL: token = %q, want secret-token", gotAuth)
	}

	// 非 GitLab URL(MinIO 起一个独立 server)→ 匿名
	anon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("PRIVATE-TOKEN")
		w.WriteHeader(http.StatusOK)
	}))
	defer anon.Close()
	if err := gl.PackageExists(context.Background(), anon.URL+"/bucket/pkg.tar.gz"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "" {
		t.Errorf("MinIO URL: token = %q, want empty", gotAuth)
	}
}
