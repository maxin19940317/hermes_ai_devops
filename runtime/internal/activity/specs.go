// Package activity 实现 DeviceTestWorkflow 引用的全部活动(CLAUDE.md §12.6)。
// 活动是薄胶水:store 型直调 store 方法,HTTP 型按 contracts/ 契约调外部服务。
package activity

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"hermes-devops/runtime/internal/evidence"
	"hermes-devops/runtime/internal/rules"
	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

// SpecDefaults 是 TestSpec 调度参数缺省值(§10)。
type SpecDefaults struct {
	MaxInfraRetries   int // 缺省 2(仅 INFRA)
	LeaseSeconds      int // 缺省 120
	HardTimeoutMargin int // 叠加在 test.timeout_sec 上,容纳下载/部署/收集
	DeviceWaitRounds  int
	DeviceWaitSeconds int
}

type signatureDecl struct {
	ID       string `yaml:"id"`
	Where    string `yaml:"where"`
	Pattern  string `yaml:"pattern"`
	Classify string `yaml:"classify"`
}

// variantsFile 是 ci/variants.yaml 的运行时视图,只解析调度所需字段。
type variantsFile struct {
	Defaults struct {
		Test struct {
			TimeoutSec int `yaml:"timeout_sec"`
		} `yaml:"test"`
		SignaturesCommonAndroid []signatureDecl `yaml:"signatures_common_android"`
		SignaturesCommonLinux   []signatureDecl `yaml:"signatures_common_linux"`
	} `yaml:"defaults"`
	Variants map[string]variantDecl `yaml:"variants"`
}

type variantDecl struct {
	Requirements struct {
		OS           string   `yaml:"os"`
		SOC          []string `yaml:"soc"`
		Capabilities []string `yaml:"capabilities"`
	} `yaml:"requirements"`
	Test struct {
		TimeoutSec int `yaml:"timeout_sec"`
	} `yaml:"test"`
	Signatures []signatureDecl `yaml:"signatures"`
}

// SpecConfig 是 worker 启动时加载的变体配置(加载失败 fail fast)。
type SpecConfig struct {
	file     variantsFile
	defaults SpecDefaults
}

func LoadSpecConfig(path string, d SpecDefaults) (*SpecConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read variants config: %w", err)
	}
	var f variantsFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse variants config: %w", err)
	}
	return &SpecConfig{file: f, defaults: d}, nil
}

// VariantNames 返回全部已声明变体名(排序后顺序稳定),供翻译层的上下文快照与
// 变体存在性校验使用。
func (c *SpecConfig) VariantNames() []string {
	if c == nil {
		return []string{}
	}
	out := make([]string, 0, len(c.file.Variants))
	for name := range c.file.Variants {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// SignaturesForVariant 合并 defaults.signatures_common_android 与
// variants.<name>.signatures(同 id 变体覆盖,与 SelectTestSpecs 的
// SignatureCategory 合并逻辑一致),按声明序返回,供证据提取使用。
// Linux 变体额外合并 signatures_common_linux(与 SelectTestSpecs 一致)。
func (c *SpecConfig) SignaturesForVariant(variant string) []evidence.Signature {
	order := []string{}
	byID := map[string]evidence.Signature{}
	merge := func(decls []signatureDecl) {
		for _, d := range decls {
			if _, ok := byID[d.ID]; !ok {
				order = append(order, d.ID)
			}
			byID[d.ID] = evidence.Signature{
				ID: d.ID, Where: d.Where, Pattern: d.Pattern, Classify: d.Classify,
			}
		}
	}
	if vd, ok := c.file.Variants[variant]; ok && vd.Requirements.OS == "linux" {
		merge(c.file.Defaults.SignaturesCommonLinux)
	} else {
		merge(c.file.Defaults.SignaturesCommonAndroid)
	}
	merge(c.file.Variants[variant].Signatures)
	out := make([]evidence.Signature, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out
}

// SelectTestSpecs 把 bundle 中的变体映射为 TestSpec;
// 未在 variants.yaml 中配置的变体跳过。
// fleet 感知(§12 变体级触发):整个 fleet(含 OFFLINE/BUSY/QUARANTINED)
// 无任何设备满足变体 selector 时,该变体秒级跳过(Skipped),不进
// acquire 等待;"设备在但暂不可用"仍由 acquire 的有限等待处理。
// 输出顺序跟随 in.Packages(workflow 依赖确定性)。
// Phase 4:Linux 变体(OS=linux)已接入设备测试链路,selector 带 OS 字段;
// fleet 无匹配设备(如 SNPE Linux 需要 hexagon 但 rk 板无 hexagon)仍秒级跳过。
func (a *Acts) SelectTestSpecs(ctx context.Context, in wf.DeviceTestInput) (*wf.SpecSelection, error) {
	sel := &wf.SpecSelection{}
	for _, p := range in.Packages {
		v, ok := a.SpecCfg.file.Variants[p.Variant]
		if !ok {
			continue
		}
		os := v.Requirements.OS
		if os == "" {
			os = "android" // 兼容:旧 variants.yaml 可能未显式声明 os
		}
		timeout := v.Test.TimeoutSec
		if timeout == 0 {
			timeout = a.SpecCfg.file.Defaults.Test.TimeoutSec
		}
		sigs := map[string]rules.Category{}
		switch os {
		case "linux":
			for _, s := range a.SpecCfg.file.Defaults.SignaturesCommonLinux {
				sigs[s.ID] = rules.Category(s.Classify)
			}
		default:
			for _, s := range a.SpecCfg.file.Defaults.SignaturesCommonAndroid {
				sigs[s.ID] = rules.Category(s.Classify)
			}
		}
		for _, s := range v.Signatures {
			sigs[s.ID] = rules.Category(s.Classify)
		}
		d := a.SpecCfg.defaults
		spec := wf.TestSpec{
			TestID:  p.Variant,
			Variant: p.Variant,
			Package: p,
			Selector: wf.DeviceSelector{
				OS:           os,
				SOC:          v.Requirements.SOC,
				Capabilities: v.Requirements.Capabilities,
			},
			SignatureCategory: sigs,
			MaxInfraRetries:   d.MaxInfraRetries,
			LeaseSeconds:      d.LeaseSeconds,
			HardTimeoutSec:    timeout + d.HardTimeoutMargin,
			DeviceWaitRounds:  d.DeviceWaitRounds,
			DeviceWaitSeconds: d.DeviceWaitSeconds,
		}
		if a.Store != nil {
			capable, err := a.Store.HasCapableDevice(ctx, spec.Selector)
			if err != nil {
				a.warnf("has capable device check failed for %s: %v; keep spec", p.Variant, err)
			} else if !capable {
				sel.Skipped = append(sel.Skipped, wf.SkippedSpec{
					Variant: p.Variant,
					Reason:  a.skipReason(ctx, spec),
				})
				continue
			}
		}
		sel.Specs = append(sel.Specs, spec)
	}
	return sel, nil
}

// skipReason 生成业务语言的 fleet-skip 原因(2026-08-06 review):
// 需求按领域知识翻译(SNPE=高通/Hexagon、RKNN=瑞芯微/NPU),在线设备以
// 无序列表逐台给出厂商和具体缺项,末尾给可行动建议。例:
// 无匹配设备:SNPE 包需要高通 Linux 板(Hexagon DSP);
// 在线设备:
// - 513cd3de 是高通 QCM6125(系统为 Android)
// - ac6dcbcbfc640f3a 是瑞芯微 RK3568(无 Hexagon DSP)
//
// 接入任意高通 Linux 板即可调度
// fleet 查询失败降级为仅列需求;OFFLINE/QUARANTINED 设备不列出。
func (a *Acts) skipReason(ctx context.Context, spec wf.TestSpec) string {
	need := variantNeed(spec)
	fleet, err := a.Store.ListFleet(ctx)
	if err != nil {
		a.warnf("list fleet for skip reason failed: %v", err)
		return "无匹配设备:" + need
	}
	if len(fleet) == 0 {
		return "无匹配设备:" + need + ";fleet 无任何已注册设备(agent 未上线?)。"
	}
	// 只列在线(IDLE/BUSY)设备的差异;OFFLINE/QUARANTINED 直接不提——
	// 历史设备对"为什么没有设备能跑"没有信息量(2026-08-06 review ×2)。
	alive := []string{}
	for _, d := range fleet {
		switch d.Status {
		case store.DeviceOffline, store.DeviceQuarantined:
			continue
		default:
			alive = append(alive, deviceCN(d, spec.Selector))
		}
	}
	reason := "无匹配设备:" + need
	if len(alive) == 0 {
		reason += ";fleet 无在线设备"
	} else {
		// 设备逐个占一行(无序列表),比顿号串排易读(2026-08-06 排版 review)
		reason += ";\n在线设备:\n- " + strings.Join(alive, "\n- ")
	}
	return reason + "\n\n" + actionHint(spec)
}

// variantNeed 把变体的调度需求翻译成业务语言(领域知识:引擎 → 目标平台)。
func variantNeed(spec wf.TestSpec) string {
	os := osCN(spec.Selector.OS)
	switch engineOf(spec.Variant) {
	case "SNPE":
		return fmt.Sprintf("SNPE 包需要高通 %s 板(Hexagon DSP)", os)
	case "RKNN":
		return fmt.Sprintf("RKNN 包需要瑞芯微 %s(%s 系统,RK NPU)", strings.Join(spec.Selector.SOC, "/"), os)
	case "TFLite":
		// 2026-08-06:Qualcomm TFLite 带 soc 约束后,领域语言须区分高通平台
		// (否则 fleet-skip 原因误导为"任意板可跑",见 TestQualcommTFLiteSkippedOnNonQualcommFleet)
		if len(spec.Selector.SOC) > 0 {
			return fmt.Sprintf("TFLite 包需要高通 %s 板(%s)", os, strings.Join(spec.Selector.SOC, "/"))
		}
		return fmt.Sprintf("TFLite 包需要 %s 设备(通用 CPU/GPU 即可)", os)
	}
	return "需要 " + selectorDesc(spec.Selector)
}

// deviceCN 用业务语言描述单台设备与需求的差异(厂商 + 具体缺项)。
func deviceCN(d store.FleetDevice, sel wf.DeviceSelector) string {
	misses := store.SelectorMismatch(d.Device, sel)
	var b strings.Builder
	b.WriteString(d.DeviceID)
	if v := vendorOf(d.SOC); v != "" && d.SOC != "" {
		fmt.Fprintf(&b, " 是%s %s", v, strings.ToUpper(d.SOC))
	}
	parts := []string{}
	for _, m := range misses {
		switch {
		case strings.HasPrefix(m, "os="):
			parts = append(parts, "系统为 "+osCN(strings.TrimPrefix(m, "os=")))
		case strings.HasPrefix(m, "soc="):
			parts = append(parts, "非目标平台")
		case strings.HasPrefix(m, "缺 "):
			parts = append(parts, "无 "+capCN(strings.TrimPrefix(m, "缺 ")))
		default:
			parts = append(parts, m)
		}
	}
	fmt.Fprintf(&b, "(%s)", strings.Join(parts, "、"))
	return b.String()
}

// actionHint 给出可行动的调度建议(领域知识:引擎 → 该接什么板)。
func actionHint(spec wf.TestSpec) string {
	switch engineOf(spec.Variant) {
	case "SNPE":
		return fmt.Sprintf("接入任意高通 %s 板即可调度", osCN(spec.Selector.OS))
	case "RKNN":
		return fmt.Sprintf("接入瑞芯微 %s 的 %s 板即可调度", strings.Join(spec.Selector.SOC, "/"), osCN(spec.Selector.OS))
	case "TFLite":
		// 2026-08-06:Qualcomm TFLite 带 soc 约束,提示接入高通板
		if len(spec.Selector.SOC) > 0 {
			return fmt.Sprintf("接入高通 %s 板(%s)即可调度", osCN(spec.Selector.OS), strings.Join(spec.Selector.SOC, "/"))
		}
		return "接入满足条件的设备后即可调度"
	}
	return "接入满足条件的设备后即可调度"
}

// engineOf 从变体名识别推理引擎(aarch64_Linux_RK3568_RKNN_2.3.2 → RKNN)。
func engineOf(variant string) string {
	v := strings.ToUpper(variant)
	switch {
	case strings.Contains(v, "SNPE"):
		return "SNPE"
	case strings.Contains(v, "RKNN"):
		return "RKNN"
	case strings.Contains(v, "TFLITE"):
		return "TFLite"
	}
	return ""
}

// vendorOf 从 SoC 型号识别厂商(大小写不敏感)。
func vendorOf(soc string) string {
	s := strings.ToUpper(soc)
	switch {
	case strings.HasPrefix(s, "RK"):
		return "瑞芯微"
	case strings.HasPrefix(s, "QCM"), strings.HasPrefix(s, "QCS"),
		strings.HasPrefix(s, "SM"), strings.HasPrefix(s, "SDM"), strings.HasPrefix(s, "SA"):
		return "高通"
	case strings.HasPrefix(s, "MT"):
		return "联发科"
	}
	return ""
}

// capCN 能力标识的中文名(调度约束 → 业务语言)。
func capCN(cap string) string {
	switch strings.ToLower(cap) {
	case "hexagon":
		return "Hexagon DSP"
	case "rknpu":
		return "RK NPU"
	}
	return cap
}

func osCN(os string) string {
	if strings.EqualFold(os, "android") {
		return "Android"
	}
	if strings.EqualFold(os, "linux") {
		return "Linux"
	}
	return os
}

// selectorDesc 只渲染非空约束项,供 fleet-skip 原因展示——selector 只含
// os+capabilities 时不再出现 "soc=[]" 这种噪声(2026-08-05 SNPE Linux 实例)。
func selectorDesc(sel wf.DeviceSelector) string {
	parts := []string{}
	if sel.OS != "" {
		parts = append(parts, "os="+sel.OS)
	}
	if len(sel.SOC) > 0 {
		parts = append(parts, fmt.Sprintf("soc=%v", sel.SOC))
	}
	if len(sel.Capabilities) > 0 {
		parts = append(parts, fmt.Sprintf("capabilities=%v", sel.Capabilities))
	}
	return strings.Join(parts, " ")
}

// ExplainNoDevice 在 acquire 有限等待超时后生成业务语言的原因(2026-08-06:
// 曾只报 "no device available",与 SKIPPED 的丰富原因反差过大)。语义与
// skipReason 互补:skipReason 用于 fleet 无任何匹配设备(秒级);
// 此处 fleet 有匹配设备但均不可用(离线/忙/隔离),要明确点出匹配设备状态。
func (a *Acts) ExplainNoDevice(ctx context.Context, req wf.ExplainNoDeviceRequest) (string, error) {
	spec := wf.TestSpec{Variant: req.Variant, Selector: req.Selector}
	return a.noDeviceReason(ctx, spec), nil
}

// noDeviceReason 生成 acquire 超时后的详细原因:需求翻译 + 匹配设备状态 +
// 在线设备差异 + 可行动建议。格式与 skipReason 对齐,多一层"匹配设备"段。
// 例:
// 无可用设备:TFLite 包需要高通 Android 板(QCM6125/QCM6490);
// 匹配设备:
// - 513cd3de(高通 QCM6125)当前 OFFLINE,接入/唤醒后可调度
// 在线设备:
// - 10.83.100.13:5555 是联发科 MT8189(非目标平台)
//
// 接入高通 Android 板(QCM6125/QCM6490)即可调度
func (a *Acts) noDeviceReason(ctx context.Context, spec wf.TestSpec) string {
	need := variantNeed(spec)
	fleet, err := a.Store.ListFleet(ctx)
	if err != nil {
		a.warnf("list fleet for no-device reason failed: %v", err)
		return "无可用设备:" + need
	}
	if len(fleet) == 0 {
		return "无可用设备:" + need + ";fleet 无任何已注册设备(agent 未上线?)。"
	}
	// 匹配设备(任意状态,带状态说明)与在线设备差异(仅 IDLE/BUSY)分离
	var matched, alive []string
	for _, d := range fleet {
		matches := len(store.SelectorMismatch(d.Device, spec.Selector)) == 0
		if matches {
			label := strings.ToUpper(d.SOC)
			if v := vendorOf(d.SOC); v != "" {
				label = v + " " + label
			}
			matched = append(matched, fmt.Sprintf("%s(%s)当前%s,接入/唤醒后可调度",
				d.DeviceID, label, deviceStatusCN(d.Status)))
			continue
		}
		switch d.Status {
		case store.DeviceOffline, store.DeviceQuarantined:
			continue
		default:
			alive = append(alive, deviceCN(d, spec.Selector))
		}
	}
	reason := "无可用设备:" + need
	if len(matched) > 0 {
		reason += ";\n匹配设备:\n- " + strings.Join(matched, "\n- ")
	}
	if len(alive) > 0 {
		reason += ";\n在线设备:\n- " + strings.Join(alive, "\n- ")
	} else if len(matched) == 0 {
		reason += ";fleet 无在线设备"
	}
	return reason + "\n\n" + actionHint(spec)
}

// deviceStatusCN 设备状态的中文名(业务语言,供 noDeviceReason 展示)。
func deviceStatusCN(status string) string {
	switch status {
	case store.DeviceIdle:
		return "空闲"
	case store.DeviceBusy:
		return "忙"
	case store.DeviceOffline:
		return "离线"
	case store.DeviceQuarantined:
		return "隔离"
	}
	return status
}
