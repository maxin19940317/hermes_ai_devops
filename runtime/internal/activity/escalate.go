package activity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	wf "hermes-devops/runtime/internal/workflow"
)

// EscalationActor 是升级裁决在 decisions 表的 actor(设计 §7 审计;
// 判重同键,§5 门槛 3)。
const EscalationActor = "escalation"

// escalationTimeout 是单次 bridge 调用超时(旁路,不得拖住 workflow)。
const escalationTimeout = 15 * time.Second

// EscalationGate 提供升级门槛的配置侧输入(设计 §5):ESCALATION_ENDPOINT
// 空 → Enabled=false(升级禁用,workflow 完全跳过旁路);判重查 decisions
// actor='escalation'(同一 task 只升级一次)。
func (a *Acts) EscalationGate(ctx context.Context, req wf.EscalationGateRequest) (*wf.EscalationGateResponse, error) {
	resp := &wf.EscalationGateResponse{MinConfidence: a.Cfg.minConfidence()}
	if a.Cfg.EscalationEndpoint == "" || a.Store == nil {
		return resp, nil
	}
	resp.Enabled = true
	already, err := a.Store.HasDecision(ctx, req.TaskID, EscalationActor)
	if err != nil {
		return nil, fmt.Errorf("escalation gate %s: %w", req.TaskID, err)
	}
	resp.AlreadyEscalated = already
	return resp, nil
}

// Escalate 组升级信封并 POST 到 kanban_bridge(设计 §2/§3,旁路 fire-and-forget)。
// bridge 不可达/建单失败:落 decisions(actor='escalation',output.error)后返回
// (nil, nil)——失败是既定形态,不作为 activity 错误触发重试(重试会产生重复
// 审计行;§7:升级失败不阻断、不重试)。成功落 output{kanban_task_id,
// idempotency_key, result}。endpoint 空 → (nil, nil) 禁用态。
func (a *Acts) Escalate(ctx context.Context, req wf.EscalationRequest) (*wf.EscalationResponse, error) {
	if a.Cfg.EscalationEndpoint == "" {
		return nil, nil
	}
	env, idemKey := buildEnvelope(a, ctx, req)
	resp, err := a.postEscalation(ctx, env)
	out := map[string]any{"idempotency_key": idemKey}
	if err != nil {
		out["error"] = err.Error()
		a.saveEscalationDecision(ctx, req.TaskID, out)
		a.warnf("escalation %s failed (audit saved): %v", req.TaskID, err)
		return nil, nil
	}
	out["kanban_task_id"] = resp.KanbanTaskID
	out["result"] = resp.Result
	a.saveEscalationDecision(ctx, req.TaskID, out)
	return resp, nil
}

// buildEnvelope 组升级信封(contracts/escalation.schema.json);
// 返回信封与幂等键(设计 §4)。
func buildEnvelope(a *Acts, ctx context.Context, req wf.EscalationRequest) (map[string]any, string) {
	idemKey := fmt.Sprintf("devops-escalation:%s:%s:%s:%s",
		req.Project, req.Commit, req.Variant, req.SignatureOrCategory)
	env := map[string]any{
		"escalation_version": 1,
		"idempotency_key":    idemKey,
		"source": map[string]any{
			"project": req.Project, "commit": req.Commit,
			"pipeline_iid": req.PipelineID, "variant": req.Variant, "task_id": req.TaskID,
		},
		"rule": map[string]any{
			"verdict": req.Verdict, "category": req.Category, "reason": req.Reason,
		},
	}
	if req.Analysis != nil {
		env["hermes"] = map[string]any{
			"summary":            req.Analysis.Summary,
			"root_cause":         req.Analysis.RootCause,
			"suggested_category": req.Analysis.SuggestedCategory,
			"confidence":         req.Analysis.Confidence,
			"next_actions":       req.Analysis.NextActions,
		}
	}
	// evidence 段:从快照登记补齐 object_key/sha256/extractor_version;
	// 快照缺失(降级未持久化或登记丢失)省略该段,不阻断升级(契约可选)。
	if req.EvidenceSnapshotID != "" && a.Store != nil {
		if snap, err := a.Store.GetEvidenceSnapshot(ctx, req.EvidenceSnapshotID); err == nil && snap != nil {
			env["evidence"] = map[string]any{
				"snapshot_id":       snap.EvidenceID,
				"object_key":        snap.ObjectKey,
				"sha256":            snap.SHA256,
				"extractor_version": snap.ExtractorVersion,
			}
		}
	}
	return env, idemKey
}

// postEscalation 把信封 POST 到 kanban_bridge(Bearer,15s 超时)。
func (a *Acts) postEscalation(ctx context.Context, env map[string]any) (*wf.EscalationResponse, error) {
	body, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("escalate: marshal envelope: %w", err)
	}
	url := strings.TrimRight(a.Cfg.EscalationEndpoint, "/") + "/escalations"
	reqCtx, cancel := context.WithTimeout(ctx, escalationTimeout)
	defer cancel()
	hr, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("escalate: build request: %w", err)
	}
	hr.Header.Set("Content-Type", "application/json")
	if a.Cfg.EscalationToken != "" {
		hr.Header.Set("Authorization", "Bearer "+a.Cfg.EscalationToken)
	}
	hc := a.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("escalate: post bridge: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("escalate: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		snip := string(respBody)
		if len(snip) > 256 {
			snip = snip[:256] + "..."
		}
		return nil, fmt.Errorf("escalate: bridge status %d: %s", resp.StatusCode, snip)
	}
	var out wf.EscalationResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("escalate: decode response: %w", err)
	}
	if out.KanbanTaskID == "" {
		return nil, fmt.Errorf("escalate: bridge response missing kanban_task_id")
	}
	return &out, nil
}

// saveEscalationDecision 落升级审计行(actor='escalation',§7);
// 失败只记日志(审计失败不放大为链路失败)。
func (a *Acts) saveEscalationDecision(ctx context.Context, taskID string, output map[string]any) {
	if a.Store == nil {
		return
	}
	raw, err := json.Marshal(output)
	if err != nil {
		a.warnf("escalation decision marshal %s: %v", taskID, err)
		return
	}
	if err := a.Store.SaveDecision(ctx, wf.DecisionRow{
		TaskID: taskID, Actor: EscalationActor, Output: raw,
	}); err != nil {
		a.warnf("escalation decision save %s: %v", taskID, err)
	}
}

// minConfidence 是 hermes 置信度门槛(设计 §5,缺省 0.7)。
func (c Config) minConfidence() float64 {
	if c.EscalationMinConfidence > 0 {
		return c.EscalationMinConfidence
	}
	return 0.7
}
