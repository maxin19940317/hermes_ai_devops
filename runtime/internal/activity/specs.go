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
					Reason:  a.skipReason(ctx, spec.Selector),
				})
				continue
			}
		}
		sel.Specs = append(sel.Specs, spec)
	}
	return sel, nil
}

// skipReason 生成可读的 fleet-skip 原因:变体需求 + fleet 各设备的具体差异
// (2026-08-06:原始消息只列 selector 字段,看不出"为什么没设备能跑")。
// 例:无匹配设备:需要 os=linux capabilities=[hexagon];fleet:513cd3de(os=android)、
// ac6dcbcbfc640f3a(缺 hexagon)。fleet 查询失败降级为仅列需求。
func (a *Acts) skipReason(ctx context.Context, sel wf.DeviceSelector) string {
	need := "需要 " + selectorDesc(sel)
	fleet, err := a.Store.ListFleet(ctx)
	if err != nil {
		a.warnf("list fleet for skip reason failed: %v", err)
		return "无匹配设备:" + need
	}
	if len(fleet) == 0 {
		return "无匹配设备:" + need + ";fleet 无任何已注册设备(agent 未上线?)"
	}
	// 只详列在线(IDLE/BUSY)设备的差异;OFFLINE/QUARANTINED 折叠为计数——
	// 历史设备对"为什么没有设备能跑"没有信息量(2026-08-06 review)。
	alive := []string{}
	offline, quarantined := 0, 0
	for _, d := range fleet {
		switch d.Status {
		case store.DeviceOffline:
			offline++
		case store.DeviceQuarantined:
			quarantined++
		default:
			miss := strings.Join(store.SelectorMismatch(d.Device, sel), ",")
			alive = append(alive, fmt.Sprintf("%s(%s)", d.DeviceID, miss))
		}
	}
	reason := "无匹配设备:" + need
	if len(alive) == 0 {
		reason += ";fleet 无在线设备"
	} else {
		reason += ";在线设备:" + strings.Join(alive, "、")
	}
	if offline > 0 || quarantined > 0 {
		reason += fmt.Sprintf("(另有 %d 台离线、%d 台隔离未列出)", offline, quarantined)
	}
	return reason
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
