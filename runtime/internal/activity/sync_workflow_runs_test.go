package activity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	wf "hermes-devops/runtime/internal/workflow"
)

// TestSyncWorkflowRunsPostsResults:活动把每个 task 结果 POST 到 bridge。
func TestSyncWorkflowRunsPostsResults(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		got = append(got, body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := &Acts{
		HTTP: srv.Client(),
		Cfg: Config{
			WorkflowBridgeURL:   srv.URL + "/api/workflow-runs",
			WorkflowBridgeToken: "test-token",
		},
	}
	err := a.SyncWorkflowRuns(context.Background(), wf.SyncWorkflowRunsRequest{
		WorkflowID: "device-test-aios/algo_super_sdk-ge543adfd-p106",
		Project:    "aios/algo_super_sdk",
		Tasks: []wf.TaskSummary{
			{Variant: "aarch64_Linux_QCS6490_SNPE_2.21", Verdict: "PASSED",
				DurationSec: 5.9, CasesTotal: 3, CasesFailed: 0},
			{Variant: "aarch64_Android_QCM6125_SNPE_1.68", Verdict: "TEST_FAILED",
				DurationSec: 10.2, CasesTotal: 3, CasesFailed: 2},
		},
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("posts = %d, want 2", len(got))
	}
	if got[0]["variant"] != "aarch64_Linux_QCS6490_SNPE_2.21" || got[0]["verdict"] != "PASSED" {
		t.Errorf("first payload = %v", got[0])
	}
	// run_id 派生自 workflow_id + variant,且去特殊字符(点→下划线)
	rid, _ := got[0]["run_id"].(string)
	if !strings.Contains(rid, "device-test-aios") || !strings.Contains(rid, "QCS6490_SNPE_2_21") {
		t.Errorf("run_id = %q, want derived from workflow+variant", rid)
	}
	if strings.ContainsAny(rid, "/.") {
		t.Errorf("run_id should be sanitized (no slash/dot): %q", rid)
	}
}

// TestSyncWorkflowRunsSkipsWhenUnconfigured:URL 空 → 静默跳过。
func TestSyncWorkflowRunsSkipsWhenUnconfigured(t *testing.T) {
	a := &Acts{HTTP: http.DefaultClient, Cfg: Config{}}
	if err := a.SyncWorkflowRuns(context.Background(), wf.SyncWorkflowRunsRequest{
		Tasks: []wf.TaskSummary{{Variant: "v1"}},
	}); err != nil {
		t.Fatalf("unconfigured should not error: %v", err)
	}
}

// TestSyncWorkflowRunsIgnoresBridgeErrors:bridge 5xx → 不阻断(记录日志)。
func TestSyncWorkflowRunsIgnoresBridgeErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	a := &Acts{
		HTTP: srv.Client(),
		Cfg: Config{
			WorkflowBridgeURL:   srv.URL,
			WorkflowBridgeToken: "t",
		},
	}
	if err := a.SyncWorkflowRuns(context.Background(), wf.SyncWorkflowRunsRequest{
		Tasks: []wf.TaskSummary{{Variant: "v1", Verdict: "PASSED"}},
	}); err != nil {
		t.Fatalf("bridge 500 should not error: %v", err)
	}
}

// TestSyncWorkflowRunsSkipsEmptyVariant:variant 空的任务跳过。
func TestSyncWorkflowRunsSkipsEmptyVariant(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	a := &Acts{
		HTTP: srv.Client(),
		Cfg:  Config{WorkflowBridgeURL: srv.URL, WorkflowBridgeToken: "t"},
	}
	if err := a.SyncWorkflowRuns(context.Background(), wf.SyncWorkflowRunsRequest{
		Tasks: []wf.TaskSummary{{Variant: ""}},
	}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if hit {
		t.Error("empty variant should be skipped")
	}
}
