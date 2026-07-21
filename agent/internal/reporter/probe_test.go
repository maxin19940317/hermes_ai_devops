package reporter

import (
	"context"
	"testing"
)

// 设备固件只暴露平台代号(trinket),调度约束用 SoC 型号(QCM6125):
// SOCAliases 命中时上报别名,未命中保持原值。
func TestProbeDevicesSOCAlias(t *testing.T) {
	p := &Prober{
		Runner:     heartbeatRunner(),
		SOCAliases: map[string]string{"trinket": "QCM6125"},
	}
	devices := p.ProbeDevices(context.Background(), map[string]bool{})

	var s1, s2 *DeviceInfo
	for i := range devices {
		switch devices[i].Serial {
		case "SERIAL1":
			s1 = &devices[i]
		case "SERIAL2":
			s2 = &devices[i]
		}
	}
	if s1 == nil || s2 == nil {
		t.Fatalf("missing probed devices: %+v", devices)
	}
	if s1.Props.SOC != "QCM6125" {
		t.Errorf("SERIAL1 soc = %q, want aliased QCM6125", s1.Props.SOC)
	}
	if s2.Props.SOC != "msm8937" {
		t.Errorf("SERIAL2 soc = %q, want unmapped msm8937", s2.Props.SOC)
	}
}

// 无别名表时行为不变(回归)。
func TestProbeDevicesWithoutSOCAlias(t *testing.T) {
	p := &Prober{Runner: heartbeatRunner()}
	devices := p.ProbeDevices(context.Background(), map[string]bool{})
	for _, d := range devices {
		if d.Serial == "SERIAL1" && d.Props.SOC != "trinket" {
			t.Errorf("SERIAL1 soc = %q, want probed trinket", d.Props.SOC)
		}
	}
}

// Capabilities 声明透传到设备属性(调度子集匹配的依据)。
func TestProbeDevicesCapabilities(t *testing.T) {
	with := &Prober{Runner: heartbeatRunner(), Capabilities: []string{"hexagon"}}
	for _, d := range with.ProbeDevices(context.Background(), map[string]bool{}) {
		if d.Serial != "SERIAL1" {
			continue
		}
		if len(d.Props.Capabilities) != 1 || d.Props.Capabilities[0] != "hexagon" {
			t.Errorf("capabilities = %v, want [hexagon]", d.Props.Capabilities)
		}
	}
	without := &Prober{Runner: heartbeatRunner()}
	for _, d := range without.ProbeDevices(context.Background(), map[string]bool{}) {
		if d.Props == nil { // OFFLINE 设备无属性
			continue
		}
		if len(d.Props.Capabilities) != 0 {
			t.Errorf("no caps configured, got %v", d.Props.Capabilities)
		}
	}
}
