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
			{Variant: "aarch64_Android_QCM6490_SNPE_2.21", URL: "https://gitlab/pkg1", SHA256: "aa", ManifestDigest: "dd"},
			{Variant: "aarch64_Linux_QCS6490_SNPE_2.21", URL: "https://gitlab/pkg2"}, // Linux:Phase 4 已接入
			{Variant: "unknown_variant", URL: "https://gitlab/pkg3"},                 // 未配置:跳过
		}}
	sel, err := a.SelectTestSpecs(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Specs) != 2 {
		t.Fatalf("specs = %d, want 2(Android + Linux 均已接入)", len(sel.Specs))
	}
	s := sel.Specs[0]
	if s.TestID != "aarch64_Android_QCM6490_SNPE_2.21" || s.Variant != s.TestID || s.Package.URL != "https://gitlab/pkg1" {
		t.Errorf("spec = %+v", s)
	}
	if len(s.Selector.SOC) != 1 || s.Selector.SOC[0] != "QCM6490" || s.Selector.Capabilities[0] != "hexagon" {
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
			{Variant: "aarch64_Android_QCM6125_SNPE_1.68", URL: "https://gitlab/pkg1"},
			{Variant: "aarch64_Android_RK3568_RKNN_2.3.2", URL: "https://gitlab/pkg2"},
		}}
	sel, err := a.SelectTestSpecs(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Specs) != 1 || sel.Specs[0].Variant != "aarch64_Android_QCM6125_SNPE_1.68" {
		t.Errorf("specs = %+v, want 仅 SNPE(QCM6125 匹配 fleet 的 QCM6125 板)", sel.Specs)
	}
	if len(sel.Skipped) != 1 || sel.Skipped[0].Variant != "aarch64_Android_RK3568_RKNN_2.3.2" {
		t.Fatalf("skipped = %+v, want RKNN 无匹配设备", sel.Skipped)
	}
	// 智能跳过原因:领域知识翻译需求 + 在线设备无序列表 + 可行动建议;
	// 离线/隔离设备完全不提(2026-08-06 review ×3)
	reason := sel.Skipped[0].Reason
	for _, want := range []string{
		"RKNN 包需要瑞芯微 RK3568(Android 系统,RK NPU)",
		"在线设备:\n- d1 是高通 QCM6125(非目标平台、未声明 RK NPU 能力)",
		"\n\n接入瑞芯微 RK3568 的 Android 板即可调度",
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
		"aarch64_Android_QCM6490_SNPE_2.21": {Signatures: []signatureDecl{
			{ID: "cpu_fallback", Where: "logcat", Pattern: "Falling back to CPU", Classify: "MODEL"},
		}},
		"aarch64_Android_RK3568_RKNN_2.3.2": {Signatures: []signatureDecl{
			// 变体覆盖同 id:替换 where/pattern/classify,位置保持声明序
			{ID: "native_crash", Where: "stderr", Pattern: "Segmentation fault", Classify: "DEVICE"},
			{ID: "rknn_init_fail", Where: "logcat", Pattern: "rknn_init.*fail|RKNN_ERR", Classify: "DELEGATE"},
		}},
	}

	// 合并:公共在前,变体私有追加在后
	sigs := cfg.SignaturesForVariant("aarch64_Android_QCM6490_SNPE_2.21")
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
	sigs = cfg.SignaturesForVariant("aarch64_Android_RK3568_RKNN_2.3.2")
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
	want := []string{
		"aarch64_Android_QCM6125_SNPE_1.68",
		"aarch64_Android_QCM6490_SNPE_2.21",
		"aarch64_Android_Qualcomm_TFLite_2.21.0",
		"aarch64_Android_RK3568_RKNN_2.3.2",
		"aarch64_Linux_QCS6490_SNPE_2.21",
	}
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

// TestValidateVariantsRejectsMissingRequirements:方案 B(2026-08-11)——
// 业务仓库 variants.yaml 是唯一权威,Runtime 启动时校验配置完整性:
// 任何变体缺 os/abi/soc 必须 fail fast,不能等派单才暴露。
func TestValidateVariantsRejectsMissingRequirements(t *testing.T) {
	ok := variantsFile{Variants: map[string]variantDecl{
		"v1": {Requirements: struct {
			OS           string   `yaml:"os"`
			ABI          string   `yaml:"abi"`
			SOC          []string `yaml:"soc"`
			Capabilities []string `yaml:"capabilities"`
		}{OS: "linux", ABI: "arm64-v8a", SOC: []string{"QCS6490"}}},
	}}
	if err := validateVariants(ok); err != nil {
		t.Fatalf("合法配置不应报错: %v", err)
	}
	cases := []struct {
		name string
		req  struct {
			OS           string   `yaml:"os"`
			ABI          string   `yaml:"abi"`
			SOC          []string `yaml:"soc"`
			Capabilities []string `yaml:"capabilities"`
		}
	}{
		{"缺 os", struct {
			OS           string   `yaml:"os"`
			ABI          string   `yaml:"abi"`
			SOC          []string `yaml:"soc"`
			Capabilities []string `yaml:"capabilities"`
		}{ABI: "arm64-v8a", SOC: []string{"QCS6490"}}},
		{"缺 abi", struct {
			OS           string   `yaml:"os"`
			ABI          string   `yaml:"abi"`
			SOC          []string `yaml:"soc"`
			Capabilities []string `yaml:"capabilities"`
		}{OS: "linux", SOC: []string{"QCS6490"}}},
		{"缺 soc", struct {
			OS           string   `yaml:"os"`
			ABI          string   `yaml:"abi"`
			SOC          []string `yaml:"soc"`
			Capabilities []string `yaml:"capabilities"`
		}{OS: "linux", ABI: "arm64-v8a"}},
	}
	for _, tc := range cases {
		f := variantsFile{Variants: map[string]variantDecl{
			"bad-variant": {Requirements: tc.req},
		}}
		if err := validateVariants(f); err == nil {
			t.Errorf("%s: 应拒绝(必须 os+abi+soc)", tc.name)
		}
	}
}

// TestValidateVariantsRejectsSoCNameDrift:变体名含 SoC 编码但 soc 约束不含它
// (或留空)必须拒绝——正是 2026-08-11 实机 bug(QCS6125 变体无 soc 约束被派到
// QCS6490)的防漂移门。
func TestValidateVariantsRejectsSoCNameDrift(t *testing.T) {
	req := struct {
		OS           string   `yaml:"os"`
		ABI          string   `yaml:"abi"`
		SOC          []string `yaml:"soc"`
		Capabilities []string `yaml:"capabilities"`
	}{OS: "linux", ABI: "arm64-v8a", SOC: []string{"QCS6490"}}
	// 名字含 QCS6125,约束却是 QCS6490 → 漂移,必须拒绝
	f := variantsFile{Variants: map[string]variantDecl{
		"aarch64_Linux_QCS6125_SNPE_1.68": {Requirements: req},
	}}
	if err := validateVariants(f); err == nil {
		t.Error("名字含 QCS6125 但约束 QCS6490 必须拒绝")
	}
	// 名字含 QCS6125,约束也含 QCS6125 → 合法
	req2 := req
	req2.SOC = []string{"QCS6125"}
	f2 := variantsFile{Variants: map[string]variantDecl{
		"aarch64_Linux_QCS6125_SNPE_1.68": {Requirements: req2},
	}}
	if err := validateVariants(f2); err != nil {
		t.Errorf("匹配的约束不应拒绝: %v", err)
	}
}

// TestQualcommTFLiteSkippedOnNonQualcommFleet:2026-08-06 实机回归——
// aarch64_Android_Qualcomm_TFLite_2.21.0 曾因无 soc 约束被派到 MTK mt8189 板
// (AcquireDevice 按 device_id 升序选中 10.83.100.13:5555),而包内 GPU delegate
// 是 Adreno。加 soc 约束后,fleet 只有 MTK/RK 板时必须秒级跳过,不得派发。
func TestQualcommTFLiteSkippedOnNonQualcommFleet(t *testing.T) {
	a := testActs(t)
	st := store.NewMemStore()
	if err := st.UpsertClientDevices(ctx, store.Client{ClientID: "c1", BaseURL: "https://c1"},
		[]store.Device{
			{DeviceID: "10.83.100.13:5555", Serial: "10.83.100.13:5555", ClientID: "c1",
				OS: "android", SOC: "mt8189"},
			{DeviceID: "b84aa09110cfc84a", Serial: "b84aa09110cfc84a", ClientID: "c1",
				OS: "android", SOC: "rk3576"},
		}); err != nil {
		t.Fatal(err)
	}
	a.Store = st
	sel, err := a.SelectTestSpecs(ctx, wf.DeviceTestInput{
		Project: "p", Commit: "abc1234", PipelineID: 1,
		Packages: []wf.PackageRef{
			{Variant: "aarch64_Android_Qualcomm_TFLite_2.21.0", URL: "https://gitlab/pkg1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Specs) != 0 {
		t.Errorf("specs = %+v, want 0(非高通 fleet 不得派发 Qualcomm 包)", sel.Specs)
	}
	if len(sel.Skipped) != 1 || sel.Skipped[0].Variant != "aarch64_Android_Qualcomm_TFLite_2.21.0" {
		t.Fatalf("skipped = %+v, want Qualcomm TFLite 秒级跳过", sel.Skipped)
	}
	// 领域语言:高通平台 + 具体缺项
	for _, want := range []string{"高通", "非目标平台"} {
		if !strings.Contains(sel.Skipped[0].Reason, want) {
			t.Errorf("reason = %q, want 含 %q", sel.Skipped[0].Reason, want)
		}
	}
	// 有高通板(任意状态)时必须保留 spec
	st2 := store.NewMemStore()
	if err := st2.UpsertClientDevices(ctx, store.Client{ClientID: "c1", BaseURL: "https://c1"},
		[]store.Device{
			{DeviceID: "513cd3de", Serial: "513cd3de", ClientID: "c1",
				OS: "android", SOC: "QCM6125", Capabilities: []string{"hexagon"}},
		}); err != nil {
		t.Fatal(err)
	}
	a.Store = st2
	sel2, err := a.SelectTestSpecs(ctx, wf.DeviceTestInput{
		Project: "p", Commit: "abc1234", PipelineID: 1,
		Packages: []wf.PackageRef{
			{Variant: "aarch64_Android_Qualcomm_TFLite_2.21.0", URL: "https://gitlab/pkg1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sel2.Specs) != 1 || len(sel2.Skipped) != 0 {
		t.Errorf("specs = %+v skipped=%+v, want 1 spec / 0 skipped(QCM6125 在 fleet 中)",
			sel2.Specs, sel2.Skipped)
	}
}

// TestExplainNoDevice:acquire 有限等待超时后的原因须与 SKIPPED 同等信息量
// (2026-08-06:曾只报 "no device available")。有匹配设备但离线时要明确点出
// 状态;无任何匹配设备时退化为 skipReason 同构的文案。
func TestExplainNoDevice(t *testing.T) {
	t.Run("匹配设备离线", func(t *testing.T) {
		a := testActs(t)
		st := store.NewMemStore()
		if err := st.UpsertClientDevices(ctx, store.Client{ClientID: "c1", BaseURL: "https://c1"},
			[]store.Device{
				{DeviceID: "513cd3de", Serial: "513cd3de", ClientID: "c1", ReportedState: store.DeviceOffline,
					OS: "android", SOC: "QCM6125", Capabilities: []string{"hexagon"}},
				{DeviceID: "10.83.100.13:5555", Serial: "10.83.100.13:5555", ClientID: "c1",
					OS: "android", SOC: "mt8189"},
			}); err != nil {
			t.Fatal(err)
		}
		a.Store = st
		got, err := a.ExplainNoDevice(ctx, wf.ExplainNoDeviceRequest{
			Variant:  "aarch64_Android_Qualcomm_TFLite_2.21.0",
			Selector: wf.DeviceSelector{OS: "android", SOC: []string{"QCM6125", "QCM6490"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"无可用设备:TFLite 包需要高通 Android 板(QCM6125/QCM6490)",
			"匹配设备:\n- 513cd3de(高通 QCM6125)当前离线,接入/唤醒后可调度",
			"在线设备:\n- 10.83.100.13:5555 是联发科 MT8189(非目标平台)",
			"接入高通 Android 板(QCM6125/QCM6490)即可调度",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("reason = %q, want 含 %q", got, want)
			}
		}
		if strings.Contains(got, "fleet 无在线设备") {
			t.Errorf("reason = %q, 有在线设备时不应说 fleet 无在线设备", got)
		}
	})
	t.Run("无任何匹配设备", func(t *testing.T) {
		a := testActs(t)
		st := store.NewMemStore()
		if err := st.UpsertClientDevices(ctx, store.Client{ClientID: "c1", BaseURL: "https://c1"},
			[]store.Device{
				{DeviceID: "b84aa09110cfc84a", Serial: "b84aa09110cfc84a", ClientID: "c1",
					OS: "android", SOC: "rk3576"},
			}); err != nil {
			t.Fatal(err)
		}
		a.Store = st
		got, err := a.ExplainNoDevice(ctx, wf.ExplainNoDeviceRequest{
			Variant:  "aarch64_Android_Qualcomm_TFLite_2.21.0",
			Selector: wf.DeviceSelector{OS: "android", SOC: []string{"QCM6125", "QCM6490"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "无可用设备") || !strings.Contains(got, "在线设备") ||
			strings.Contains(got, "匹配设备:") {
			t.Errorf("reason = %q, 无匹配设备时不应有匹配设备段", got)
		}
	})
	t.Run("fleet 为空", func(t *testing.T) {
		a := testActs(t)
		a.Store = store.NewMemStore()
		got, err := a.ExplainNoDevice(ctx, wf.ExplainNoDeviceRequest{
			Variant: "v", Selector: wf.DeviceSelector{OS: "android"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "fleet 无任何已注册设备") {
			t.Errorf("reason = %q, want 提示 agent 未上线", got)
		}
	})
}
