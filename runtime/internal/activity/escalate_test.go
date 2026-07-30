package activity

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hermes-devops/runtime/internal/hermesclient"
	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

// fakeBridge 记录信封并按脚本应答(created/existing/失败)。
type fakeBridge struct {
	gotAuth   string
	gotBody   map[string]any
	status    int
	reply     string
	callCount int
}

func (f *fakeBridge) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.callCount++
		f.gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &f.gotBody)
		status := f.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		reply := f.reply
		if reply == "" {
			reply = `{"kanban_task_id":"t_01343ec8","result":"created"}`
		}
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func escalationReq() wf.EscalationRequest {
	return wf.EscalationRequest{
		TaskID: "w:t:a1", Project: "grp/p", Commit: "abcd1234", PipelineID: 42,
		Variant: "aarch64_Android_SNPE_2.21", Verdict: "TEST_FAILED",
		Category: "DELEGATE", Reason: "signature hit: dsp_unavailable",
		SignatureOrCategory: "dsp_unavailable",
		Analysis: &hermesclient.Analysis{
			AnalysisVersion: 1, Summary: "dsp 委派失败", SuggestedCategory: "DELEGATE",
			Confidence: 0.9, NextActions: []string{"查 delegate 分区"},
		},
	}
}

// EscalationGate:endpoint 空 → 禁用;启用时判重(decisions actor='escalation')。
func TestEscalationGate(t *testing.T) {
	a := &Acts{Store: store.NewMemStore()}
	g, err := a.EscalationGate(ctx, wf.EscalationGateRequest{TaskID: "w:t:a1"})
	if err != nil || g.Enabled {
		t.Errorf("未配置 endpoint 应禁用: %+v err=%v", g, err)
	}
	if g.MinConfidence != 0.7 {
		t.Errorf("缺省阈值 = %v, want 0.7", g.MinConfidence)
	}

	st := store.NewMemStore()
	a = &Acts{Store: st, Cfg: Config{EscalationEndpoint: "http://bridge", EscalationMinConfidence: 0.8}}
	g, _ = a.EscalationGate(ctx, wf.EscalationGateRequest{TaskID: "w:t:a1"})
	if !g.Enabled || g.AlreadyEscalated || g.MinConfidence != 0.8 {
		t.Errorf("gate = %+v", g)
	}
	// 已升级过的 task:判重 true
	_ = st.SaveDecision(ctx, wf.DecisionRow{TaskID: "w:t:a1", Actor: EscalationActor,
		Output: json.RawMessage(`{"result":"created"}`)})
	g, _ = a.EscalationGate(ctx, wf.EscalationGateRequest{TaskID: "w:t:a1"})
	if !g.AlreadyEscalated {
		t.Error("已有 escalation 决策应判重")
	}
}

// Escalate 成功路径:信封形态(契约必填段 + 幂等键派生)+ Bearer + 审计落行。
func TestEscalateCreated(t *testing.T) {
	fb := &fakeBridge{}
	srv := fb.server(t)
	st := store.NewMemStore()
	_ = st.SaveEvidenceSnapshot(ctx, store.EvidenceSnapshot{
		EvidenceID: "w:t:a1", TaskID: "w:t:a1", Attempt: 1,
		ObjectKey: "evidence/w:t:a1/evidence.json", SHA256: strings.Repeat("a", 64),
		ExtractorVersion: "1",
	})
	a := &Acts{Store: st, HTTP: srv.Client(), Cfg: Config{
		EscalationEndpoint: srv.URL, EscalationToken: "tok"}}

	req := escalationReq()
	req.EvidenceSnapshotID = "w:t:a1"
	resp, err := a.Escalate(ctx, req)
	if err != nil || resp == nil {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if resp.KanbanTaskID != "t_01343ec8" || resp.Result != "created" {
		t.Errorf("resp = %+v", resp)
	}
	if fb.gotAuth != "Bearer tok" {
		t.Errorf("auth = %q", fb.gotAuth)
	}
	env := fb.gotBody
	if env["escalation_version"].(float64) != 1 {
		t.Errorf("envelope = %v", env)
	}
	if env["idempotency_key"] != "devops-escalation:grp/p:abcd1234:aarch64_Android_SNPE_2.21:dsp_unavailable" {
		t.Errorf("idempotency_key = %v", env["idempotency_key"])
	}
	src := env["source"].(map[string]any)
	if src["commit"] != "abcd1234" || src["pipeline_iid"].(float64) != 42 || src["task_id"] != "w:t:a1" {
		t.Errorf("source = %v", src)
	}
	rule := env["rule"].(map[string]any)
	if rule["category"] != "DELEGATE" || rule["verdict"] != "TEST_FAILED" {
		t.Errorf("rule = %v", rule)
	}
	hermes := env["hermes"].(map[string]any)
	if hermes["confidence"].(float64) != 0.9 || hermes["summary"] != "dsp 委派失败" {
		t.Errorf("hermes = %v", hermes)
	}
	ev := env["evidence"].(map[string]any)
	if ev["object_key"] != "evidence/w:t:a1/evidence.json" || ev["extractor_version"] != "1" {
		t.Errorf("evidence = %v(快照登记补齐)", ev)
	}
	// 审计:decisions actor='escalation' 落成功行
	if has, _ := st.HasDecision(ctx, "w:t:a1", EscalationActor); !has {
		t.Error("成功升级应落审计行")
	}
	dec, _ := st.ListDecisions(ctx, "w:t:a1")
	if len(dec) != 1 || !strings.Contains(string(dec[0].Output), "t_01343ec8") ||
		!strings.Contains(string(dec[0].Output), `"created"`) {
		t.Errorf("decisions = %+v", dec)
	}
}

// 快照缺失(降级):evidence 段省略,不阻断升级。
func TestEscalateWithoutSnapshot(t *testing.T) {
	fb := &fakeBridge{reply: `{"kanban_task_id":"t_01343ec8","result":"existing"}`}
	srv := fb.server(t)
	a := &Acts{Store: store.NewMemStore(), HTTP: srv.Client(),
		Cfg: Config{EscalationEndpoint: srv.URL}}
	resp, err := a.Escalate(ctx, escalationReq())
	if err != nil || resp == nil || resp.Result != "existing" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if _, ok := fb.gotBody["evidence"]; ok {
		t.Error("无快照不得带 evidence 段")
	}
}

// bridge 失败(502/超时):返回 (nil,nil) 不作为 activity 错误(防重试产生
// 重复审计),审计落 error 行。
func TestEscalateBridgeFailureIsSilent(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{"bridge 502", http.StatusBadGateway},
		{"bridge 400 schema", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := &fakeBridge{status: tc.status}
			srv := fb.server(t)
			st := store.NewMemStore()
			a := &Acts{Store: st, HTTP: srv.Client(), Cfg: Config{EscalationEndpoint: srv.URL}}
			resp, err := a.Escalate(ctx, escalationReq())
			if err != nil || resp != nil {
				t.Errorf("bridge 失败应返回 (nil,nil): resp=%+v err=%v", resp, err)
			}
			dec, _ := st.ListDecisions(ctx, "w:t:a1")
			if len(dec) != 1 || !strings.Contains(string(dec[0].Output), "error") {
				t.Errorf("失败应落 error 审计行: %+v", dec)
			}
		})
	}
}

// endpoint 空:禁用态,(nil,nil) 且零调用零审计。
func TestEscalateDisabledWhenNoEndpoint(t *testing.T) {
	a := &Acts{Store: store.NewMemStore()}
	resp, err := a.Escalate(ctx, escalationReq())
	if err != nil || resp != nil {
		t.Errorf("resp=%+v err=%v", resp, err)
	}
}
