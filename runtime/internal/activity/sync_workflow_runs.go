package activity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	wf "hermes-devops/runtime/internal/workflow"
)

// SyncWorkflowRuns 把真实测试结果回填 hermes workflow_runtime.db。
// 调用 hermes-rocklin 容器内的 workflow_bridge(POST /api/workflow-runs)。
// 失败只记日志,不阻断主链路(fire-and-forget 旁路,与 escalation 同语义)。
// 幂等:bridge 按 run_id 去重,重复投递无副作用。
func (a *Acts) SyncWorkflowRuns(ctx context.Context, req wf.SyncWorkflowRunsRequest) error {
	url := a.Cfg.WorkflowBridgeURL
	token := a.Cfg.WorkflowBridgeToken
	if url == "" || token == "" {
		return nil // 未配置:静默跳过(开发模式)
	}
	for _, tk := range req.Tasks {
		if tk.Variant == "" {
			continue
		}
		// run_id 用 workflow_id + variant 派生,同 workflow 同 variant 幂等去重。
		payload := map[string]any{
			"run_id":      fmt.Sprintf("wr-devops-%s-%s", sanitizeID(req.WorkflowID), sanitizeID(tk.Variant)),
			"variant":     tk.Variant,
			"status":      "COMPLETED",
			"verdict":     tk.Verdict,
			"duration_sec": tk.DurationSec,
			"cases_total": tk.CasesTotal,
			"cases_failed": tk.CasesFailed,
			"metrics":     tk.Metrics,
			"project":     req.Project,
			"workflow_ref": req.WorkflowID,
		}
		body, err := json.Marshal(payload)
		if err != nil {
			a.warnf("sync workflow run marshal: %v", err)
			continue
		}
		reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			cancel()
			a.warnf("sync workflow run new request: %v", err)
			continue
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+token)
		resp, err := a.HTTP.Do(httpReq)
		cancel()
		if err != nil {
			a.warnf("sync workflow run %s: %v", tk.Variant, err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			a.warnf("sync workflow run %s: status %d", tk.Variant, resp.StatusCode)
			continue
		}
	}
	return nil
}

// sanitizeID 把 workflow/variant 转成 URL/DB 安全字符(run_id 用)。
func sanitizeID(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out = append(out, r)
		case r == '_' || r == '-':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
