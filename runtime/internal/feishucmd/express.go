package feishucmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"hermes-devops/runtime/internal/activity"
	"hermes-devops/runtime/internal/hermesclient"
	wf "hermes-devops/runtime/internal/workflow"
	"hermes-devops/runtime/internal/store"
)

// expressDevices 计算 DeviceFacts → 调 LLM 表述 → 降级规则文本;审计落
// command_translations(outcome=express_ok / express_fallback)。
// 只读路径:LLM 不可用/超时/不合 Schema 一律降级规则文本,不阻塞回答。
func (e *Executor) expressDevices(ctx context.Context, matched []store.DeviceStatus) (string, error) {
	fleet, err := e.Store.ListFleet(ctx)
	if err != nil {
		return "", err
	}
	// 2026-08-12 解耦:变体清单来自已登记产物(业务仓库权威),不再用
	// SpecCfg.VariantNames。selectorFor 从 artifact 的 requirements 派生。
	arts, err := e.Store.AllVariants(ctx)
	if err != nil {
		return "", err
	}
	variants := make([]string, 0, len(arts))
	selectorFor := func(variant string) wf.DeviceSelector {
		for _, a := range arts {
			if a.Variant == variant && a.VariantRequirements != nil {
				r := a.VariantRequirements
				return wf.DeviceSelector{
					OS: r.OS, SOC: r.SOC, Capabilities: r.Capabilities,
				}
			}
		}
		return wf.DeviceSelector{}
	}
	for _, a := range arts {
		variants = append(variants, a.Variant)
	}
	facts := activity.ComputeDeviceFacts(e.nowFn(), fleet, variants, selectorFor)
	factsJSON, err := json.Marshal(facts)
	if err != nil {
		return "", err
	}

	fallback := renderDevicesRuleText(matched, facts)

	resp, err := e.Express.Express(ctx, hermesclient.ExpressRequest{
		RawText: "查询当前在线设备",
		Scene:   "devices",
		Facts:   factsJSON,
		Model:   e.ExpressModel,
	})
	if err != nil || resp == nil {
		e.saveExpressAudit(ctx, activity.FactsSummary(facts), factsJSON, nil, store.OutcomeExpressFallback)
		return fallback, nil // 表述层挂 → 规则文本
	}
	out, err := json.Marshal(resp)
	if err != nil {
		out = nil
	}
	e.saveExpressAudit(ctx, activity.FactsSummary(facts), factsJSON, out, store.OutcomeExpressOK)
	return renderExpressReply(resp), nil
}

// renderExpressReply 把 Express 结构化输出渲染为纯文本(本轮 emoji 放行)。
func renderExpressReply(r *hermesclient.ExpressResponse) string {
	var b strings.Builder
	if r.Summary != "" {
		b.WriteString(r.Summary)
	}
	for _, s := range r.Sections {
		b.WriteString("\n" + s)
	}
	if r.Footer != "" {
		b.WriteString("\n\n" + r.Footer)
	}
	return b.String()
}

// renderDevicesRuleText 是表述层降级文本:只列在线设备 + 缺口提示(设计文档 §4.6)。
// 降级文本由规则生成,不是 LLM 输出——保证任何时刻回答都可用。
func renderDevicesRuleText(matched []store.DeviceStatus, facts activity.DeviceFacts) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📱 在线设备(%d 台)", len(matched))
	for _, d := range matched {
		b.WriteString("\n" + formatDeviceLine(d))
	}
	if len(facts.Gaps) > 0 {
		for _, g := range facts.Gaps {
			fmt.Fprintf(&b, "\n⚠️ %d 个变体待测: %s", len(g.Variants), g.Reason)
		}
	}
	return b.String()
}

// saveExpressAudit 落表述审计行(express_ok / express_fallback)。
// context_digest 存 Facts 摘要 sha256(可回放"当时看到了什么"),output 存
// LLM 表述输出,raw_text 存用户原话——一张表串起完整证据链(设计文档 §4.3)。
func (e *Executor) saveExpressAudit(
	ctx context.Context, summary string, factsJSON []byte, output []byte, outcome string,
) {
	if e.Store == nil {
		return
	}
	digest := sha256Hex(append([]byte(summary), factsJSON...))
	row := store.CommandTranslation{
		RawText:       "devices(表述层)",
		PromptVersion: hermesclient.PromptVersionExpress,
		Model:         e.ExpressModel,
		ContextDigest: digest,
		Outcome:       outcome,
	}
	if len(output) > 0 {
		row.Output = append([]byte(nil), output...)
	}
	if err := e.Store.SaveCommandTranslation(ctx, row); err != nil {
		log := e.log()
		log.Error().Err(err).Str("outcome", outcome).Msg("save express audit failed")
	}
}

// sha256Hex 计算字节的 sha256 hex(审计 context_digest)。
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
