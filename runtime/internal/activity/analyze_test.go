package activity

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"hermes-devops/runtime/internal/hermesclient"
	wf "hermes-devops/runtime/internal/workflow"
)

// fakeHermes 实现 hermesclient.Client,记录请求并返回预设结果。
type fakeHermes struct {
	req  *hermesclient.AnalyzeRequest
	out  *hermesclient.Analysis
	err  error
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
		TaskID  string `json:"task_id"`
		Inputs  struct {
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
}
