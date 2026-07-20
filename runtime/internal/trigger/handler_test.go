package trigger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

const testSecret = "s3cret"

// validBundle 构造契约合法的 bundle 文档(短 sha=abcd1234)。
func validBundle() map[string]any {
	pkg := func(variant string) map[string]any {
		return map[string]any{
			"variant":         variant,
			"package_file":    "algo-super-sdk-" + variant + "-gabcd1234-p42.tar.gz",
			"url":             "https://gitlab.example/api/v4/projects/1/packages/generic/algo-super-sdk/1.2.3/x.tar.gz",
			"sha256":          strings.Repeat("a", 64),
			"size":            1024,
			"manifest_digest": strings.Repeat("b", 64),
		}
	}
	return map[string]any{
		"bundle_version":     1,
		"project":            "grp/algo-super-sdk",
		"commit":             "abcd1234",
		"pipeline_id":        42,
		"pipeline_global_id": 42001,
		"version":            "1.2.3",
		"created_at":         "2026-07-17T08:00:00.000Z",
		"packages": []any{
			pkg("aarch64_Android_SNPE_2.21"),
			pkg("aarch64_Linux_SNPE_2.21"),
		},
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// pipelineEvent 构造 GitLab 13.8 Pipeline Hook payload。
func pipelinePayload(status, ref, sha string) []byte {
	return pipelinePayloadWithIDs(status, ref, sha, "42001", "7")
}

func pipelinePayloadWithIDs(status, ref, sha, pipelineID, projectID string) []byte {
	return []byte(fmt.Sprintf(`{
		"object_kind": "pipeline",
		"object_attributes": {"id": %s, "ref": %q, "tag": false, "sha": %q, "status": %q},
		"project": {"id": %s, "path_with_namespace": "grp/algo-super-sdk"}
	}`, pipelineID, ref, sha, status, projectID))
}

const fullSHA = "abcd1234deadbeefabcd1234deadbeefabcd1234"

// ---- fakes ----

type fakeFetcher struct {
	bundle              []byte // nil = 404(未找到)
	err                 error
	calls               int
	gotSHA              string
	gotProj             int64
	gotPipelineGlobalID int64
}

func (f *fakeFetcher) FetchBundle(_ context.Context, projectID int64, shortSHA string, pipelineGlobalID int64) ([]byte, bool, error) {
	f.calls++
	f.gotProj, f.gotSHA, f.gotPipelineGlobalID = projectID, shortSHA, pipelineGlobalID
	if f.err != nil {
		return nil, false, f.err
	}
	if f.bundle == nil {
		return nil, false, nil
	}
	return f.bundle, true, nil
}

type fakeStarter struct {
	started    bool // 返回值:是否新启动(false=已存在,幂等)
	err        error
	calls      int
	gotInput   wf.DeviceTestInput
	gotWFID    string
	failBefore bool
}

func (f *fakeStarter) StartDeviceTest(_ context.Context, in wf.DeviceTestInput) (string, bool, error) {
	f.calls++
	f.gotInput = in
	f.gotWFID = in.WorkflowID()
	if f.err != nil {
		return "", false, f.err
	}
	return in.WorkflowID(), f.started, nil
}

func newTestHandler(fetcher *fakeFetcher, starter *fakeStarter) (*Handler, *store.MemStore) {
	st := store.NewMemStore()
	h, err := New(Config{WebhookSecret: testSecret, Refs: []string{"master"}}, fetcher, st, starter)
	if err != nil {
		panic(err)
	}
	return h, st
}

func post(h http.Handler, token string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab", bytes.NewReader(body))
	if token != "" {
		req.Header.Set("X-Gitlab-Token", token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ---- tests ----

func TestNewRejectsEmptyWebhookSecret(t *testing.T) {
	if _, err := New(Config{}, nil, nil, nil); err == nil {
		t.Fatal("New accepted an empty webhook secret")
	}
}

func TestRejectsBadToken(t *testing.T) {
	fetcher, starter := &fakeFetcher{}, &fakeStarter{}
	h, _ := newTestHandler(fetcher, starter)
	for _, token := range []string{"", "wrong"} {
		rec := post(h, token, pipelinePayload("success", "master", fullSHA))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("token=%q: code=%d, want 401", token, rec.Code)
		}
	}
	if fetcher.calls != 0 || starter.calls != 0 {
		t.Error("验签失败后不得有任何下游动作")
	}
}

func TestIgnoresNonPipelineAndNonSuccess(t *testing.T) {
	cases := map[string][]byte{
		"push event":       []byte(`{"object_kind":"push"}`),
		"running pipeline": pipelinePayload("running", "master", fullSHA),
		"failed pipeline":  pipelinePayload("failed", "master", fullSHA),
		"other ref":        pipelinePayload("success", "feature/x", fullSHA),
	}
	for name, body := range cases {
		fetcher, starter := &fakeFetcher{}, &fakeStarter{}
		h, _ := newTestHandler(fetcher, starter)
		rec := post(h, testSecret, body)
		if rec.Code != http.StatusNoContent {
			t.Errorf("%s: code=%d, want 204", name, rec.Code)
		}
		if fetcher.calls != 0 || starter.calls != 0 {
			t.Errorf("%s: 不得触发下游", name)
		}
	}
}

func TestRejectsInvalidSHAWithoutFetching(t *testing.T) {
	cases := map[string]string{
		"short":     fullSHA[:len(fullSHA)-1],
		"long":      fullSHA + "a",
		"question":  "abcd123?deadbeefabcd1234deadbeefabcd1234",
		"fragment":  "abcd123#deadbeefabcd1234deadbeefabcd1234",
		"slash":     "abcd123/deadbeefabcd1234deadbeefabcd1234",
		"uppercase": "Abcd1234deadbeefabcd1234deadbeefabcd1234",
		"non-hex":   "zbcd1234deadbeefabcd1234deadbeefabcd1234",
	}
	for name, sha := range cases {
		t.Run(name, func(t *testing.T) {
			fetcher, starter := &fakeFetcher{}, &fakeStarter{}
			h, _ := newTestHandler(fetcher, starter)
			rec := post(h, testSecret, pipelinePayload("success", "master", sha))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("code=%d body=%s, want 400", rec.Code, rec.Body)
			}
			if fetcher.calls != 0 {
				t.Errorf("fetch calls=%d, want 0", fetcher.calls)
			}
		})
	}
}

func TestRejectsInvalidIDsWithoutFetching(t *testing.T) {
	valid := pipelinePayload("success", "master", fullSHA)
	cases := map[string][]byte{
		"missing pipeline id": bytes.Replace(valid, []byte(`"id": 42001, `), nil, 1),
		"zero pipeline id":    pipelinePayloadWithIDs("success", "master", fullSHA, "0", "7"),
		"negative pipeline id": pipelinePayloadWithIDs("success", "master", fullSHA,
			"-1", "7"),
		"pipeline id int64 overflow": pipelinePayloadWithIDs("success", "master", fullSHA,
			"9223372036854775808", "7"),
		"missing project id": bytes.Replace(valid, []byte(`"id": 7, `), nil, 1),
		"zero project id":    pipelinePayloadWithIDs("success", "master", fullSHA, "42001", "0"),
		"negative project id": pipelinePayloadWithIDs("success", "master", fullSHA,
			"42001", "-1"),
		"project id int64 overflow": pipelinePayloadWithIDs("success", "master", fullSHA,
			"42001", "9223372036854775808"),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			fetcher, starter := &fakeFetcher{}, &fakeStarter{}
			h, _ := newTestHandler(fetcher, starter)
			rec := post(h, testSecret, body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("code=%d body=%s, want 400", rec.Code, rec.Body)
			}
			if fetcher.calls != 0 {
				t.Errorf("fetch calls=%d, want 0", fetcher.calls)
			}
		})
	}
}

func TestSuccessPipelineStartsWorkflowAndRegistersArtifacts(t *testing.T) {
	fetcher := &fakeFetcher{bundle: mustJSON(t, validBundle())}
	starter := &fakeStarter{started: true}
	h, st := newTestHandler(fetcher, starter)

	rec := post(h, testSecret, pipelinePayload("success", "master", fullSHA))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s, want 202", rec.Code, rec.Body)
	}
	// bundle 用 short sha(前 8 位)定位
	if fetcher.gotSHA != "abcd1234" || fetcher.gotProj != 7 || fetcher.gotPipelineGlobalID != 42001 {
		t.Errorf("fetch args = (%d, %q, %d)", fetcher.gotProj, fetcher.gotSHA, fetcher.gotPipelineGlobalID)
	}
	// artifacts 全量登记
	arts := st.Artifacts()
	if len(arts) != 2 {
		t.Fatalf("registered %d artifacts, want 2", len(arts))
	}
	for _, a := range arts {
		if a.CommitSHA != "abcd1234" || a.PipelineID != 42 || a.BuildType != "Release" ||
			a.ManifestDigest != strings.Repeat("b", 64) {
			t.Errorf("artifact = %+v", a)
		}
	}
	// workflow 输入来自 bundle,ID 确定性
	if starter.gotInput.Commit != "abcd1234" || starter.gotInput.PipelineID != 42 ||
		len(starter.gotInput.Packages) != 2 {
		t.Errorf("workflow input = %+v", starter.gotInput)
	}
	wantID := "device-test-grp/algo-super-sdk-gabcd1234-p42"
	var resp struct {
		WorkflowID string `json:"workflow_id"`
		Started    bool   `json:"started"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not json: %v", err)
	}
	if resp.WorkflowID != wantID || !resp.Started {
		t.Errorf("response = %+v, want id=%s started=true", resp, wantID)
	}
}

func TestDuplicateDeliveryIsIdempotent(t *testing.T) {
	fetcher := &fakeFetcher{bundle: mustJSON(t, validBundle())}
	starter := &fakeStarter{started: false} // Temporal 返回 AlreadyStarted → started=false
	h, st := newTestHandler(fetcher, starter)

	for i := 0; i < 2; i++ {
		rec := post(h, testSecret, pipelinePayload("success", "master", fullSHA))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("delivery %d: code=%d, want 202(重复投递也应答成功)", i, rec.Code)
		}
	}
	if got := len(st.Artifacts()); got != 2 {
		t.Errorf("重复投递后 artifacts=%d, want 2(幂等)", got)
	}
}

func TestNoBundleIsSkippedQuietly(t *testing.T) {
	fetcher := &fakeFetcher{bundle: nil} // 所有版本都 404
	starter := &fakeStarter{}
	h, st := newTestHandler(fetcher, starter)
	rec := post(h, testSecret, pipelinePayload("success", "master", fullSHA))
	if rec.Code != http.StatusOK {
		t.Errorf("code=%d, want 200(无 bundle 不是错误,如 MR 构建)", rec.Code)
	}
	if starter.calls != 0 || len(st.Artifacts()) != 0 {
		t.Error("无 bundle 不得登记/启动")
	}
}

func TestInvalidBundleRejected(t *testing.T) {
	bad := validBundle()
	delete(bad, "packages") // 违反 schema required
	fetcher := &fakeFetcher{bundle: mustJSON(t, bad)}
	starter := &fakeStarter{}
	h, st := newTestHandler(fetcher, starter)
	rec := post(h, testSecret, pipelinePayload("success", "master", fullSHA))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("code=%d, want 422", rec.Code)
	}
	if starter.calls != 0 || len(st.Artifacts()) != 0 {
		t.Error("非法 bundle 不得登记/启动(红线:未经 Schema 校验不消费)")
	}
}

func TestBundleCommitMismatchRejected(t *testing.T) {
	fetcher := &fakeFetcher{bundle: mustJSON(t, validBundle())} // commit=abcd1234
	starter := &fakeStarter{}
	h, _ := newTestHandler(fetcher, starter)
	otherSHA := "ffff0000deadbeefabcd1234deadbeefabcd1234"
	rec := post(h, testSecret, pipelinePayload("success", "master", otherSHA))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("code=%d, want 422(bundle.commit 必须是事件 sha 前缀)", rec.Code)
	}
	if starter.calls != 0 {
		t.Error("commit 不一致不得启动 workflow")
	}
}

func TestBundlePipelineMismatchRejected(t *testing.T) {
	bundle := validBundle() // pipeline_global_id=42001
	fetcher := &fakeFetcher{bundle: mustJSON(t, bundle)}
	starter := &fakeStarter{}
	h, st := newTestHandler(fetcher, starter)
	body := bytes.Replace(pipelinePayload("success", "master", fullSHA), []byte(`"id": 42001`), []byte(`"id": 42002`), 1)

	rec := post(h, testSecret, body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("code=%d body=%s, want 422(bundle.pipeline_global_id 必须等于事件 object_attributes.id)", rec.Code, rec.Body)
	}
	if strings.TrimSpace(rec.Body.String()) != "bundle pipeline mismatch" {
		t.Errorf("body=%q, want bundle pipeline mismatch", rec.Body.String())
	}
	if starter.calls != 0 || len(st.Artifacts()) != 0 {
		t.Error("pipeline global ID 不一致不得登记 artifact 或启动 workflow")
	}
}

func TestBundleProjectMismatchRejected(t *testing.T) {
	bundle := validBundle()
	bundle["project"] = "other/algo-super-sdk"
	fetcher := &fakeFetcher{bundle: mustJSON(t, bundle)}
	starter := &fakeStarter{}
	h, st := newTestHandler(fetcher, starter)

	rec := post(h, testSecret, pipelinePayload("success", "master", fullSHA))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("code=%d body=%s, want 422", rec.Code, rec.Body)
	}
	if strings.TrimSpace(rec.Body.String()) != "bundle project mismatch" {
		t.Errorf("body=%q, want bundle project mismatch", rec.Body.String())
	}
	if starter.calls != 0 || len(st.Artifacts()) != 0 {
		t.Error("project 不一致不得登记 artifact 或启动 workflow")
	}
}

func TestFetchErrorIs502(t *testing.T) {
	fetcher := &fakeFetcher{err: errors.New("gitlab down")}
	starter := &fakeStarter{}
	h, _ := newTestHandler(fetcher, starter)
	rec := post(h, testSecret, pipelinePayload("success", "master", fullSHA))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code=%d, want 502", rec.Code)
	}
}

func TestStarterErrorIs502(t *testing.T) {
	fetcher := &fakeFetcher{bundle: mustJSON(t, validBundle())}
	starter := &fakeStarter{err: errors.New("temporal down")}
	h, _ := newTestHandler(fetcher, starter)
	rec := post(h, testSecret, pipelinePayload("success", "master", fullSHA))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("code=%d, want 502", rec.Code)
	}
}
