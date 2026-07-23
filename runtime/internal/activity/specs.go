// Package activity 实现 DeviceTestWorkflow 引用的全部活动(CLAUDE.md §12.6)。
// 活动是薄胶水:store 型直调 store 方法,HTTP 型按 contracts/ 契约调外部服务。
package activity

import (
	"context"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"hermes-devops/runtime/internal/evidence"
	"hermes-devops/runtime/internal/rules"
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

// SignaturesForVariant 合并 defaults.signatures_common_android 与
// variants.<name>.signatures(同 id 变体覆盖,与 SelectTestSpecs 的
// SignatureCategory 合并逻辑一致),按声明序返回,供证据提取使用。
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
	merge(c.file.Defaults.SignaturesCommonAndroid)
	merge(c.file.Variants[variant].Signatures)
	out := make([]evidence.Signature, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out
}

// SelectTestSpecs 把 bundle 中的 Android 变体映射为 TestSpec;
// Linux 变体第一阶段不进设备测试链路(§6.4),未配置变体跳过。
// fleet 感知(§12 变体级触发):整个 fleet(含 OFFLINE/BUSY/QUARANTINED)
// 无任何设备满足变体 selector 时,该变体秒级跳过(Skipped),不进
// acquire 等待;"设备在但暂不可用"仍由 acquire 的有限等待处理。
// 输出顺序跟随 in.Packages(workflow 依赖确定性)。
func (a *Acts) SelectTestSpecs(ctx context.Context, in wf.DeviceTestInput) (*wf.SpecSelection, error) {
	sel := &wf.SpecSelection{}
	for _, p := range in.Packages {
		v, ok := a.SpecCfg.file.Variants[p.Variant]
		if !ok {
			continue
		}
		if v.Requirements.OS != "android" {
			sel.Skipped = append(sel.Skipped, wf.SkippedSpec{
				Variant: p.Variant,
				Reason:  "os " + v.Requirements.OS + " 尚未接入设备测试链路(Phase 4)",
			})
			continue
		}
		timeout := v.Test.TimeoutSec
		if timeout == 0 {
			timeout = a.SpecCfg.file.Defaults.Test.TimeoutSec
		}
		sigs := map[string]rules.Category{}
		for _, s := range a.SpecCfg.file.Defaults.SignaturesCommonAndroid {
			sigs[s.ID] = rules.Category(s.Classify)
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
				// fleet 查询失败不阻塞派发:退回 acquire 等待的旧行为
				a.warnf("has capable device check failed for %s: %v; keep spec", p.Variant, err)
			} else if !capable {
				sel.Skipped = append(sel.Skipped, wf.SkippedSpec{
					Variant: p.Variant,
					Reason: fmt.Sprintf("no capable device registered (soc=%v capabilities=%v)",
						spec.Selector.SOC, spec.Selector.Capabilities),
				})
				continue
			}
		}
		sel.Specs = append(sel.Specs, spec)
	}
	return sel, nil
}
