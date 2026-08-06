package activity

import (
	"context"
	"strings"
	"testing"

	"hermes-devops/runtime/internal/rules"
	"hermes-devops/runtime/internal/store"
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

func TestSelectTestSpecsAndroidAndLinux(t *testing.T) {
	a := testActs(t)
	in := wf.DeviceTestInput{Project: "algo-super-sdk", Commit: "abc1234", PipelineID: 42,
		Packages: []wf.PackageRef{
			{Variant: "aarch64_Android_SNPE_2.21", URL: "https://gitlab/pkg1", SHA256: "aa", ManifestDigest: "dd"},
			{Variant: "aarch64_Linux_SNPE_2.21", URL: "https://gitlab/pkg2"}, // Linux:Phase 4 已接入
			{Variant: "unknown_variant", URL: "https://gitlab/pkg3"},         // 未配置:跳过
		}}
	sel, err := a.SelectTestSpecs(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Specs) != 2 {
		t.Fatalf("specs = %d, want 2(Android + Linux 均已接入)", len(sel.Specs))
	}
	s := sel.Specs[0]
	if s.TestID != "aarch64_Android_SNPE_2.21" || s.Variant != s.TestID || s.Package.URL != "https://gitlab/pkg1" {
		t.Errorf("spec = %+v", s)
	}
	if len(s.Selector.SOC) != 1 || s.Selector.SOC[0] != "QCM6125" || s.Selector.Capabilities[0] != "hexagon" {
		t.Errorf("selector = %+v", s.Selector)
	}
	if s.Selector.OS != "android" {
		t.Errorf("OS = %q, want android", s.Selector.OS)
	}
	// 签名分类 = 公共 Android 签名 + 变体私有签名
	if s.SignatureCategory["native_crash"] != rules.CategoryCode || s.SignatureCategory["cpu_fallback"] != rules.CategoryModel {
		t.Errorf("signatures = %+v", s.SignatureCategory)
	}
	// Linux 变体:Selector.OS=linux, 公共 Linux 签名
	ls := sel.Specs[1]
	if ls.Selector.OS != "linux" {
		t.Errorf("Linux OS = %q, want linux", ls.Selector.OS)
	}
	if ls.SignatureCategory["native_crash"] != rules.CategoryCode {
		t.Errorf("Linux sigs = %+v, want native_crash=CODE", ls.SignatureCategory)
	}
	// §10 缺省 + 硬超时 = timeout_sec + margin
	if s.MaxInfraRetries != 2 || s.LeaseSeconds != 120 || s.HardTimeoutSec != 2100 {
		t.Errorf("knobs = %+v", s)
	}
	// 未配置变体静默跳过;Linux 不再进入 Skipped
	if len(sel.Skipped) != 0 {
		t.Errorf("skipped = %+v, want 0(没有 fleet 感知的跳过)", sel.Skipped)
	}
}

// TestSelectTestSpecsFleetSkip:fleet 无匹配设备的变体秒级跳过;
// 有匹配设备(任意状态)的变体保留(§12 变体级触发)。
func TestSelectTestSpecsFleetSkip(t *testing.T) {
	a := testActs(t)
	st := store.NewMemStore()
	// fleet 只有一台 Android QCM6125:RKNN Linux 变体(要 RK3588+rknpu)应被跳过,SNPE 保留
	if err := st.UpsertClientDevices(ctx, store.Client{ClientID: "c1", BaseURL: "https://c1"},
		[]store.Device{{DeviceID: "d1", Serial: "513cd3de", ClientID: "c1",
			OS: "android", SOC: "QCM6125", Capabilities: []string{"hexagon"}}}); err != nil {
		t.Fatal(err)
	}
	// 离线设备只应折叠计数,不占差异列表(2026-08-06:历史设备是噪声)
	if err := st.UpsertClientDevices(ctx, store.Client{ClientID: "c1", BaseURL: "https://c1"},
		[]store.Device{{DeviceID: "d1", Serial: "513cd3de", ClientID: "c1",
			OS: "android", SOC: "QCM6125", Capabilities: []string{"hexagon"}},
			{DeviceID: "d2-dead", Serial: "deadbeef", ClientID: "c1", ReportedState: "OFFLINE",
				OS: "linux", SOC: "RK3568", Capabilities: []string{"rknpu"}}}); err != nil {
		t.Fatal(err)
	}
	a.Store = st
	in := wf.DeviceTestInput{Project: "p", Commit: "abc1234", PipelineID: 1,
		Packages: []wf.PackageRef{
			{Variant: "aarch64_Android_SNPE_2.21", URL: "https://gitlab/pkg1"},
			{Variant: "aarch64_Android_RKNN_2.3.2", URL: "https://gitlab/pkg2"},
		}}
	sel, err := a.SelectTestSpecs(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Specs) != 1 || sel.Specs[0].Variant != "aarch64_Android_SNPE_2.21" {
		t.Errorf("specs = %+v, want 仅 SNPE", sel.Specs)
	}
	if len(sel.Skipped) != 1 || sel.Skipped[0].Variant != "aarch64_Android_RKNN_2.3.2" {
		t.Fatalf("skipped = %+v, want RKNN 无匹配设备", sel.Skipped)
	}
	// 智能跳过原因:领域知识翻译需求 + 在线设备差异 + 可行动建议;
	// 离线/隔离设备完全不提(2026-08-06 review ×2)
	reason := sel.Skipped[0].Reason
	for _, want := range []string{
		"RKNN 包需要瑞芯微 RK3588/RK3566(Android 系统,RK NPU)",
		"在线设备:d1 是高通 QCM6125(非目标平台、无 RK NPU)",
		"接入瑞芯微 RK3588/RK3566 的 Android 板即可调度",
	} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason = %q, want 含 %q", reason, want)
		}
	}
	for _, unwanted := range []string{"d2-dead", "离线", "隔离"} {
		if strings.Contains(reason, unwanted) {
			t.Errorf("reason = %q, 不应提及离线/隔离设备(%q)", reason, unwanted)
		}
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

// TestVariantNamesSorted:map 遍历序不确定,必须排序后才能给翻译层的上下文快照
// 提供稳定顺序(否则 context_digest 随遍历序抖动,审计回放对不上)。
func TestVariantNamesSorted(t *testing.T) {
	a := testActs(t)
	got := a.SpecCfg.VariantNames()
	want := []string{"aarch64_Android_RKNN_2.3.2", "aarch64_Android_SNPE_2.21", "aarch64_Linux_SNPE_2.21"}
	if len(got) != len(want) {
		t.Fatalf("VariantNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("VariantNames()[%d] = %q, want %q (顺序必须稳定排序)", i, got[i], want[i])
		}
	}
}

func TestLoadSpecConfigMissingFile(t *testing.T) {
	if _, err := LoadSpecConfig("testdata/nonexistent.yaml", SpecDefaults{}); err == nil {
		t.Error("缺失文件应报错(worker 启动时 fail fast)")
	}
}
