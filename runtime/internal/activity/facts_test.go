package activity

import (
	"strings"
	"testing"
	"time"

	"hermes-devops/runtime/internal/store"
)

// TestComputeDeviceFacts 表驱动:输入 fleet + 变体 + selector,输出 DeviceFacts。
// 变体-设备匹配复用 store.SelectorMismatch(与 SelectTestSpecs 同一事实源)。
func TestComputeDeviceFacts(t *testing.T) {
	cfg, err := LoadSpecConfig("testdata/variants.yaml", SpecDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	variants := cfg.VariantNames()
	sel := cfg.VariantSelector

	t.Run("在线设备可测变体 + 缺口", func(t *testing.T) {
		fleet := []store.FleetDevice{
			{Device: store.Device{DeviceID: "825485946", OS: "linux", SOC: "QCS6490",
				Capabilities: []string{"hexagon", "adreno"}}, Status: store.DeviceIdle},
			{Device: store.Device{DeviceID: "ac6dcbcbfc640f3a", OS: "android", SOC: "rk3568",
				Capabilities: []string{"rknpu"}}, Status: store.DeviceIdle},
			{Device: store.Device{DeviceID: "513cd3de", OS: "android", SOC: "QCM6125",
				Capabilities: []string{"hexagon"}}, Status: store.DeviceOffline},
		}
		f := ComputeDeviceFacts(now, fleet, variants, sel)
		if len(f.Online) != 2 {
			t.Fatalf("online = %d, want 2(离线不计)", len(f.Online))
		}
		if f.OfflineCount != 1 {
			t.Errorf("offline_count = %d, want 1", f.OfflineCount)
		}
		// 825485946(Linux QCS6490)应可测 Linux QCS6490 SNPE
		if !containsStr(f.Online[0].CanTest, "aarch64_Linux_QCS6490_SNPE_2.21") {
			t.Errorf("825485946 can_test = %v, want 含 Linux QCS6490 SNPE", f.Online[0].CanTest)
		}
		// 825485946 是 Linux,不能测 Android 变体
		for _, v := range f.Online[0].CanTest {
			if strings.Contains(v, "Android") {
				t.Errorf("Linux 设备误测 Android 变体 %s", v)
			}
		}
		// 缺口:QCM6125 Android SNPE 无任何设备匹配(513cd3de 离线也计入全 fleet 匹配?
		// 513cd3de 是离线但状态不影响匹配——它在 fleet 里,selector 匹配成功即不算缺口)
		matchedQCM := false
		for _, g := range f.Gaps {
			for _, v := range g.Variants {
				if v == "aarch64_Android_QCM6125_SNPE_1.68" {
					matchedQCM = true
				}
			}
		}
		if matchedQCM {
			t.Errorf("QCM6125 SNPE 不应是缺口(513cd3de 离线但存在即可匹配): gaps=%+v", f.Gaps)
		}
	})

	t.Run("can_test 上限折叠", func(t *testing.T) {
		// 一台万能设备 + 大量变体:can_test 列出前 N 个,count 记录总数
		fleet := []store.FleetDevice{
			{Device: store.Device{DeviceID: "all-in-1", OS: "android", SOC: "QCM6490",
				Capabilities: []string{"hexagon"}}, Status: store.DeviceIdle},
		}
		f := ComputeDeviceFacts(now, fleet, variants, sel)
		if len(f.Online) != 1 {
			t.Fatalf("online = %d", len(f.Online))
		}
		fact := f.Online[0]
		if fact.CanTestCount < len(fact.CanTest) {
			t.Errorf("can_test_count=%d < len(can_test)=%d, 应 >= 列出数", fact.CanTestCount, len(fact.CanTest))
		}
		if len(fact.CanTest) > canTestCap {
			t.Errorf("can_test 列出 %d 个, 超上限 %d", len(fact.CanTest), canTestCap)
		}
	})

	t.Run("隔离设备归入 quarantined", func(t *testing.T) {
		fleet := []store.FleetDevice{
			{Device: store.Device{DeviceID: "b5bb1018d94b26da", OS: "android", SOC: "rk3588"},
				Status: store.DeviceQuarantined},
		}
		f := ComputeDeviceFacts(now, fleet, variants, sel)
		if len(f.Online) != 0 || f.OfflineCount != 0 {
			t.Errorf("online=%d offline=%d, want 0/0", len(f.Online), f.OfflineCount)
		}
		if len(f.Quarantined) != 1 || f.Quarantined[0] != "b5bb1018d94b26da" {
			t.Errorf("quarantined = %v", f.Quarantined)
		}
	})
}

func TestFactsSummary(t *testing.T) {
	f := DeviceFacts{
		Online:       []DeviceFact{{ID: "a"}},
		OfflineCount: 3,
		Quarantined:  []string{"q1"},
		Gaps:         []GapFact{{Variants: []string{"v1"}}},
	}
	s := FactsSummary(f)
	if !strings.Contains(s, "online=1") || !strings.Contains(s, "offline=3") ||
		!strings.Contains(s, "quarantined=1") || !strings.Contains(s, "gaps=1") {
		t.Errorf("FactsSummary = %q", s)
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
