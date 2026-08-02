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
