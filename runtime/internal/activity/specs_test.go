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

func TestSignaturesForVariant(t *testing.T) {
	// 结构参照 ci/variants.yaml:公共 Android 签名 + 变体私有签名,变体可覆盖同 id
	cfg := &SpecConfig{file: variantsFile{}}
	cfg.file.Defaults.SignaturesCommonAndroid = []signatureDecl{
		{ID: "native_crash", Where: "logcat", Pattern: "Fatal signal|tombstone", Classify: "CODE"},
	}
	cfg.file.Variants = map[string]variantDecl{
		"aarch64_Android_SNPE_2.21": {Signatures: []signatureDecl{
			{ID: "cpu_fallback", Where: "logcat", Pattern: "Falling back to CPU", Classify: "MODEL"},
		}},
		"aarch64_Android_RKNN_2.3.2": {Signatures: []signatureDecl{
			// 变体覆盖同 id:替换 where/pattern/classify,位置保持声明序
			{ID: "native_crash", Where: "stderr", Pattern: "Segmentation fault", Classify: "DEVICE"},
			{ID: "rknn_init_fail", Where: "logcat", Pattern: "rknn_init.*fail|RKNN_ERR", Classify: "DELEGATE"},
		}},
	}

	// 合并:公共在前,变体私有追加在后
	sigs := cfg.SignaturesForVariant("aarch64_Android_SNPE_2.21")
	if len(sigs) != 2 || sigs[0].ID != "native_crash" || sigs[1].ID != "cpu_fallback" {
		t.Fatalf("sigs = %+v", sigs)
	}
	if sigs[0].Where != "logcat" || sigs[0].Pattern != "Fatal signal|tombstone" || sigs[0].Classify != "CODE" {
		t.Errorf("sigs[0] = %+v", sigs[0])
	}
	if sigs[1].Pattern != "Falling back to CPU" || sigs[1].Classify != "MODEL" {
		t.Errorf("sigs[1] = %+v", sigs[1])
	}

	// 变体覆盖同 id:单条记录取变体值,顺序不变
	sigs = cfg.SignaturesForVariant("aarch64_Android_RKNN_2.3.2")
	if len(sigs) != 2 || sigs[0].ID != "native_crash" || sigs[1].ID != "rknn_init_fail" {
		t.Fatalf("sigs = %+v", sigs)
	}
	if sigs[0].Where != "stderr" || sigs[0].Pattern != "Segmentation fault" || sigs[0].Classify != "DEVICE" {
		t.Errorf("覆盖后的 sigs[0] = %+v", sigs[0])
	}

	// 未知变体:仅公共签名
	if sigs := cfg.SignaturesForVariant("unknown"); len(sigs) != 1 || sigs[0].ID != "native_crash" {
		t.Errorf("unknown variant sigs = %+v", sigs)
	}
}

func TestLoadSpecConfigMissingFile(t *testing.T) {
	if _, err := LoadSpecConfig("testdata/nonexistent.yaml", SpecDefaults{}); err == nil {
		t.Error("缺失文件应报错(worker 启动时 fail fast)")
	}
}
