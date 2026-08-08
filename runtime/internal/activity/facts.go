package activity

import (
	"fmt"
	"sort"
	"time"

	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

// canTestCap 是每台在线设备可测变体的列出上限(设计文档 §4.2 评审定稿):
// Facts 进 prompt,无界会让 token 与延迟失控;超出部分折叠进 can_test_count。
const canTestCap = 5

// DeviceFacts 是 devices 指令的事实计算输出(设计文档 §4.2)。
// 事实永远由规则计算,LLM 只负责表述与洞察——这个结构只含确定性信息。
type DeviceFacts struct {
	Now          string       `json:"now"`
	Online       []DeviceFact `json:"online"`
	OfflineCount int          `json:"offline_count"`
	Quarantined  []string     `json:"quarantined"`
	Gaps         []GapFact    `json:"gaps"`
	Suggestions  []string     `json:"suggestions"`
}

// DeviceFact 是单台在线设备的事实(含可测变体)。
type DeviceFact struct {
	ID           string   `json:"id"`
	OS           string   `json:"os"`
	SOC          string   `json:"soc"`
	ABI          string   `json:"abi"`
	MemTotalMB   *int64   `json:"mem_total_mb,omitempty"`
	Capabilities []string `json:"capabilities"`
	CanTest      []string `json:"can_test"`
	CanTestCount int      `json:"can_test_count"`
}

// GapFact 是一个调度缺口:某组变体无任何设备(任意状态)能匹配。
type GapFact struct {
	Variants []string `json:"variants"`
	Reason   string   `json:"reason"`
}

// ComputeDeviceFacts 是纯函数(表驱动可测):输入 fleet + 全部变体 + selector
// 解析函数,输出 DeviceFacts。变体匹配复用 store.SelectorMismatch(与
// SelectTestSpecs 同一事实源,防漂移)。
//
// fleet: 全部已注册设备(含 OFFLINE/QUARANTINED)。
// variants: 全部合法变体名(来自 SpecCfg.VariantNames())。
// selectorFor: 变体 → 调度 selector(来自 SpecCfg.VariantSelector)。
func ComputeDeviceFacts(
	now time.Time,
	fleet []store.FleetDevice,
	variants []string,
	selectorFor func(variant string) wf.DeviceSelector,
) DeviceFacts {
	f := DeviceFacts{
		Now:         now.UTC().Format(time.RFC3339),
		Online:      []DeviceFact{},
		Quarantined: []string{},
		Gaps:        []GapFact{},
		Suggestions: []string{},
	}

	// 按 device_id 排序,输出确定性(与 ListFleet 一致)。
	sorted := append([]store.FleetDevice(nil), fleet...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].DeviceID < sorted[j].DeviceID })

	// 先归类设备。
	for _, d := range sorted {
		switch d.Status {
		case store.DeviceOffline:
			f.OfflineCount++
		case store.DeviceQuarantined:
			f.Quarantined = append(f.Quarantined, d.DeviceID)
		default: // IDLE/BUSY = 在线
			fact := DeviceFact{
				ID: d.DeviceID, OS: d.OS, SOC: d.SOC, ABI: d.ABI,
				MemTotalMB:   d.MemTotalMB,
				Capabilities: append([]string(nil), d.Capabilities...),
				CanTest:      []string{},
			}
			// 该台可测的变体:selector 完全匹配(与 SelectTestSpecs 同语义)。
			for _, v := range variants {
				sel := selectorFor(v)
				if len(store.SelectorMismatch(d.Device, sel)) == 0 {
					fact.CanTestCount++
					if len(fact.CanTest) < canTestCap {
						fact.CanTest = append(fact.CanTest, v)
					}
				}
			}
			sort.Strings(fact.CanTest)
			f.Online = append(f.Online, fact)
		}
	}

	// 调度缺口:整个 fleet(任意状态)无设备匹配的变体。
	// 按缺口原因分组(同因合并,回答更精炼)。
	gapByReason := map[string][]string{}
	for _, v := range variants {
		sel := selectorFor(v)
		matched := false
		for _, d := range sorted {
			if len(store.SelectorMismatch(d.Device, sel)) == 0 {
				matched = true
				break
			}
		}
		if !matched {
			spec := wf.TestSpec{Variant: v, Selector: sel}
			reason := variantNeed(spec)
			gapByReason[reason] = append(gapByReason[reason], v)
		}
	}
	for _, reason := range sortedKeys(gapByReason) {
		vs := gapByReason[reason]
		sort.Strings(vs)
		f.Gaps = append(f.Gaps, GapFact{Variants: vs, Reason: reason})
		// 从缺口反推可行动建议(复用 actionHint 领域语言)。
		f.Suggestions = append(f.Suggestions, actionHint(wf.TestSpec{Variant: vs[0], Selector: selectorFor(vs[0])}))
	}

	return f
}

// sortedKeys 返回 map 键的排序切片(输出确定性)。
func sortedKeys[M ~map[string]V, V any](m M) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// FactsSummary 计算 DeviceFacts 的摘要行(审计 context_digest 用)。
func FactsSummary(f DeviceFacts) string {
	return fmt.Sprintf("online=%d offline=%d quarantined=%d gaps=%d",
		len(f.Online), f.OfflineCount, len(f.Quarantined), len(f.Gaps))
}
