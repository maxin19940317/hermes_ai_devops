package activity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hermes-devops/runtime/internal/hermesclient"
	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

// fakeHermes 实现 hermesclient.Client,记录请求并返回预设结果。
type fakeHermes struct {
	req *hermesclient.AnalyzeRequest
	out *hermesclient.Analysis
	err error
}

func (f *fakeHermes) Analyze(_ context.Context, req hermesclient.AnalyzeRequest) (*hermesclient.Analysis, error) {
	f.req = &req
	return f.out, f.err
}

func analyzeReq() wf.AnalyzeRequest {
	return wf.AnalyzeRequest{
		TaskID: "t1", RuleCategory: "MODEL",
		EvidenceJSON: json.RawMessage(`{"evidence_version":1}`),
	}
}

func TestAnalyzeDisabledWhenHermesNil(t *testing.T) {
	a := &Acts{}
	got, err := a.Analyze(ctx, analyzeReq())
	if err != nil || got != nil {
		t.Errorf("Analyzer 未启用应返回 (nil,nil) 由 workflow 跳过, got=%+v err=%v", got, err)
	}
}

func TestAnalyzeDelegatesToHermes(t *testing.T) {
	h := &fakeHermes{out: &hermesclient.Analysis{
		AnalysisVersion: 1, Summary: "s", SuggestedCategory: "MODEL", Confidence: 0.8,
	}}
	a := &Acts{Hermes: h, Cfg: Config{HermesModel: "m1"}}
	got, err := a.Analyze(ctx, analyzeReq())
	if err != nil || got == nil || got.Summary != "s" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if h.req == nil || h.req.TaskID != "t1" || h.req.RuleCategory != "MODEL" ||
		h.req.Model != "m1" || string(h.req.Evidence) != `{"evidence_version":1}` {
		t.Errorf("透传请求 = %+v", h.req)
	}
}

func TestAnalyzeHermesFailurePropagates(t *testing.T) {
	a := &Acts{Hermes: &fakeHermes{err: errors.New("platform down")}}
	if _, err := a.Analyze(ctx, analyzeReq()); err == nil {
		t.Error("平台失败应返回 error,由 workflow 降级到规则引擎保底")
	}
}

// TestExtractEvidenceDegradesWithoutMinIO:MinIO 未配置时所有证据文件计入
// missing,提取仍成功并产出合法 evidence.json(降级语义,§3.7)。
func TestExtractEvidenceDegradesWithoutMinIO(t *testing.T) {
	a := &Acts{} // 无 MinIO 配置、无 SpecCfg
	resp, err := a.ExtractEvidence(ctx, wf.ExtractEvidenceRequest{
		TaskID: "t1", Variant: "v",
		Result: wf.TaskResultSignal{
			TaskID: "t1", Status: "COMPLETED", ExitCode: 1, CasesTotal: 3, CasesFailed: 1,
			Attachments: []wf.Attachment{{Name: "logcat.txt", ObjectKey: "runs/t1/logcat.txt"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Digest) != 64 {
		t.Errorf("digest = %q, want sha256 hex", resp.Digest)
	}
	var ev struct {
		TaskID string `json:"task_id"`
		Inputs struct {
			Missing []string `json:"missing"`
		} `json:"inputs"`
	}
	if err := json.Unmarshal(resp.EvidenceJSON, &ev); err != nil {
		t.Fatalf("evidence 不是合法 JSON: %v", err)
	}
	if ev.TaskID != "t1" || len(ev.Inputs.Missing) != 4 {
		t.Errorf("missing = %v, want 4 个证据文件全部缺失", ev.Inputs.Missing)
	}
	if !strings.Contains(string(resp.EvidenceJSON), `"logcat.txt"`) {
		t.Error("missing 应含 logcat.txt")
	}
	// MinIO 未配置:快照降级不落(差距 #6 降级形态)
	if resp.SnapshotID != "" {
		t.Errorf("SnapshotID = %q, want 空(MinIO 未配置降级)", resp.SnapshotID)
	}
}

// fakeMinIO 伺服 S3 子集:bucket location 查询 + PutObject 记录。
type fakeMinIO struct {
	putPath string
	putBody []byte
	putCode int // 0 → 200
}

func (f *fakeMinIO) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.URL.Query()["location"]; ok {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`))
			return
		}
		if r.Method == http.MethodPut {
			f.putPath = r.URL.Path
			f.putBody, _ = io.ReadAll(r.Body)
			code := f.putCode
			if code == 0 {
				code = http.StatusOK
			}
			w.Header().Set("ETag", `"deadbeef"`)
			w.WriteHeader(code)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
}

// TestExtractEvidencePersistsSnapshot:差距 #6——evidence.json 上传 MinIO
// (evidence/{task_id}/evidence.json,与 runs/ 附件并排)并落 evidence_snapshots;
// 快照 id = task_id(含 attempt,幂等)。
func TestExtractEvidencePersistsSnapshot(t *testing.T) {
	fm := &fakeMinIO{}
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()
	st := store.NewMemStore()
	a := &Acts{Store: st, Cfg: Config{
		MinIOEndpoint: srv.URL, MinIOAccessKey: "k", MinIOSecretKey: "s", MinIOBucket: "bucket",
	}}

	resp, err := a.ExtractEvidence(ctx, wf.ExtractEvidenceRequest{
		TaskID: "w:t:a2", Variant: "v",
		Result: wf.TaskResultSignal{TaskID: "w:t:a2", Status: "COMPLETED", ExitCode: 1, CasesFailed: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.SnapshotID != "w:t:a2" {
		t.Errorf("SnapshotID = %q, want w:t:a2", resp.SnapshotID)
	}
	if fm.putPath != "/bucket/evidence/w:t:a2/evidence.json" {
		t.Errorf("put path = %q, want /b/evidence/w:t:a2/evidence.json", fm.putPath)
	}
	// minio-go 以 aws-chunked(STREAMING-…-PAYLOAD)编码发送,证据原文嵌于
	// 数据块内;完整性由快照行 sha256 覆盖,此处断言内容随上传到达即可。
	if !bytes.Contains(fm.putBody, resp.EvidenceJSON) {
		t.Errorf("上传内容未包含 EvidenceJSON(%d 字节)", len(resp.EvidenceJSON))
	}
	snap, err := st.GetEvidenceSnapshot(ctx, "w:t:a2")
	if err != nil || snap == nil {
		t.Fatalf("snapshot = %+v err=%v", snap, err)
	}
	if snap.ObjectKey != "evidence/w:t:a2/evidence.json" || snap.SHA256 != resp.Digest ||
		snap.ExtractorVersion != "3" || snap.Attempt != 2 || snap.TaskID != "w:t:a2" {
		t.Errorf("snapshot = %+v", snap)
	}
}

// TestExtractEvidenceSnapshotUploadFailureDegrades:上传失败降级——不阻断提取,
// SnapshotID 空、快照不落(§3.7:结果回流优先)。
func TestExtractEvidenceSnapshotUploadFailureDegrades(t *testing.T) {
	fm := &fakeMinIO{putCode: http.StatusInternalServerError}
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()
	st := store.NewMemStore()
	a := &Acts{Store: st, Cfg: Config{
		MinIOEndpoint: srv.URL, MinIOAccessKey: "k", MinIOSecretKey: "s", MinIOBucket: "bucket",
	}}
	resp, err := a.ExtractEvidence(ctx, wf.ExtractEvidenceRequest{
		TaskID: "w:t:a1", Variant: "v",
		Result: wf.TaskResultSignal{TaskID: "w:t:a1", Status: "COMPLETED", ExitCode: 1, CasesFailed: 1},
	})
	if err != nil {
		t.Fatalf("上传失败不得使提取报错: %v", err)
	}
	if resp.SnapshotID != "" {
		t.Errorf("SnapshotID = %q, want 空(上传失败降级)", resp.SnapshotID)
	}
	if snap, _ := st.GetEvidenceSnapshot(ctx, "w:t:a1"); snap != nil {
		t.Errorf("上传失败不得落快照: %+v", snap)
	}
}
