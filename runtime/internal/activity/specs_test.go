package activity

import (
	"context"
	"testing"

	"hermes-devops/runtime/internal/rules"
	wf "hermes-devops/runtime/internal/workflow"
)

var ctx = context.Background()

func testActs(t *testing.T) *Acts {
	t.Helper()
	cfg, err := LoadSpecConfig("testdata/variants.yaml", SpecDefaults{
		MaxInfraRetries: 2, LeaseSeconds: 120, HardTimeoutMargin: 1200,
		DeviceWaitRounds: 20, DeviceWaitSeconds: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Acts{SpecCfg: cfg}
}

func TestSelectTestSpecsAndroidOnly(t *testing.T) {
	a := testActs(t)
	in := wf.DeviceTestInput{Project: "algo-super-sdk", Commit: "abc1234", PipelineID: 42,
		Packages: []wf.PackageRef{
			{Variant: "aarch64_Android_SNPE_2.21", URL: "https://gitlab/pkg1", SHA256: "aa", ManifestDigest: "dd"},
			{Variant: "aarch64_Linux_SNPE_2.21", URL: "https://gitlab/pkg2"}, // Linux:不进链路(§6.4)
			{Variant: "unknown_variant", URL: "https://gitlab/pkg3"},         // 未配置:跳过
		}}
	specs, err := a.SelectTestSpecs(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 {
		t.Fatalf("specs = %d, want 1(仅 Android 变体)", len(specs))
	}
	s := specs[0]
	if s.TestID != "aarch64_Android_SNPE_2.21" || s.Variant != s.TestID || s.Package.URL != "https://gitlab/pkg1" {
		t.Errorf("spec = %+v", s)
	}
	if len(s.Selector.SOC) != 1 || s.Selector.SOC[0] != "QCM6125" || s.Selector.Capabilities[0] != "hexagon" {
		t.Errorf("selector = %+v", s.Selector)
	}
	// 签名分类 = 公共 Android 签名 + 变体私有签名
	if s.SignatureCategory["native_crash"] != rules.CategoryCode || s.SignatureCategory["cpu_fallback"] != rules.CategoryModel {
		t.Errorf("signatures = %+v", s.SignatureCategory)
	}
	// §10 缺省 + 硬超时 = timeout_sec + margin
	if s.MaxInfraRetries != 2 || s.LeaseSeconds != 120 || s.HardTimeoutSec != 2100 {
		t.Errorf("knobs = %+v", s)
	}
}

func TestLoadSpecConfigMissingFile(t *testing.T) {
	if _, err := LoadSpecConfig("testdata/nonexistent.yaml", SpecDefaults{}); err == nil {
		t.Error("缺失文件应报错(worker 启动时 fail fast)")
	}
}
