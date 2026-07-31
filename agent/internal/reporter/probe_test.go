package reporter

import (
	"context"
	"testing"

	"hermes-devops/agent/internal/adb"
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
	if s1.DisplayName != "QCM6125-SERIAL1" {
		t.Errorf("SERIAL1 display_name = %q, want QCM6125-SERIAL1", s1.DisplayName)
	}
	if s2.Props.SOC != "msm8937" {
		t.Errorf("SERIAL2 soc = %q, want unmapped msm8937", s2.Props.SOC)
	}
	if s2.DisplayName != "MSM8937-SERIAL2" {
		t.Errorf("SERIAL2 display_name = %q, want MSM8937-SERIAL2", s2.DisplayName)
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
	with := &Prober{Runner: heartbeatRunner(), DeviceCapabilities: map[string][]string{
		"serial1": {"hexagon"},
	}}
	for _, d := range with.ProbeDevices(context.Background(), map[string]bool{}) {
		if d.Props == nil {
			continue
		}
		if d.Serial == "SERIAL1" && (len(d.Props.Capabilities) != 1 || d.Props.Capabilities[0] != "hexagon") {
			t.Errorf("SERIAL1 capabilities = %v, want [hexagon]", d.Props.Capabilities)
		}
		if d.Serial == "SERIAL2" && len(d.Props.Capabilities) != 0 {
			t.Errorf("SERIAL2 inherited another device's capabilities: %v", d.Props.Capabilities)
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

func TestProbeDevicesIgnoresLegacyCapabilitiesWithMultipleDevices(t *testing.T) {
	p := &Prober{Runner: heartbeatRunner(), Capabilities: []string{"hexagon"}}
	for _, d := range p.ProbeDevices(context.Background(), map[string]bool{}) {
		if d.Props != nil && len(d.Props.Capabilities) != 0 {
			t.Errorf("multi-device legacy capabilities leaked to %s: %v", d.Serial, d.Props.Capabilities)
		}
	}
}

func TestProbeDevicesResolvesQuestionMarkTransport(t *testing.T) {
	runner := &fakeRunner{responses: map[string]adb.Result{
		"devices -l": {Stdout: "List of devices attached\n? device product:trinket transport_id:28\n"},
		"-s ? shell /system/bin/getprop ro.serialno":              {Stdout: "513cd3de\n"},
		"-s ? shell /system/bin/getprop ro.product.cpu.abi":       {Stdout: "arm64-v8a\n"},
		"-s ? shell /system/bin/getprop ro.build.version.release": {Stdout: "12\n"},
		"-s ? shell /system/bin/getprop ro.board.platform":        {Stdout: "trinket\n"},
		"-s ? shell /system/bin/df -k /data/local/tmp": {Stdout: "Filesystem 1K-blocks Used Available Use% Mounted on\n" +
			"/dev/block/dm-0 10000000 100 1000000 1% /data\n"},
	}}
	p := &Prober{Runner: runner, SOCAliases: map[string]string{"trinket": "QCM6125"}}

	devices := p.ProbeDevices(context.Background(), map[string]bool{"513cd3de": true})
	if len(devices) != 1 {
		t.Fatalf("devices = %+v, want one resolved device", devices)
	}
	if devices[0].Serial != "513cd3de" || devices[0].State != DeviceBusy {
		t.Errorf("resolved device = %+v, want serial 513cd3de and BUSY", devices[0])
	}
	if devices[0].DisplayName != "QCM6125-513cd3de" {
		t.Errorf("display_name = %q, want QCM6125-513cd3de", devices[0].DisplayName)
	}
	if devices[0].Props == nil || devices[0].Props.SOC != "QCM6125" {
		t.Errorf("resolved props = %+v, want aliased QCM6125", devices[0].Props)
	}
}

func TestProbeDevicesIdentifiesLinuxSOCButKeepsDeviceOffline(t *testing.T) {
	runner := &fakeRunner{responses: map[string]adb.Result{
		"devices -l": {Stdout: "List of devices attached\nb5bb1018d94b26da device product:mako\n"},
		"-s b5bb1018d94b26da shell /system/bin/getprop ro.product.cpu.abi": {
			ExitCode: 127, Stderr: "/system/bin/getprop: not found",
		},
		"-s b5bb1018d94b26da shell /bin/cat /proc/device-tree/compatible": {
			Stdout: "friendlyelec,nanopc-t6\x00rockchip,rk3588\x00",
		},
	}}
	p := &Prober{Runner: runner}

	devices := p.ProbeDevices(context.Background(), map[string]bool{})
	if len(devices) != 1 {
		t.Fatalf("devices = %+v, want one Linux device", devices)
	}
	got := devices[0]
	if got.State != DeviceOffline || got.DisplayName != "RK3588-b5bb1018d94b26da" {
		t.Errorf("Linux device = %+v, want RK3588 display name and OFFLINE", got)
	}
	if got.Props == nil || got.Props.SOC != "rk3588" {
		t.Errorf("Linux props = %+v, want soc rk3588", got.Props)
	}
}
