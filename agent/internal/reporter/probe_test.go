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
