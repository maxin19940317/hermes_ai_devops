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

// 老 adbd 不回传远程退出码且 adb 合并 stderr:getprop 不存在时 adb 仍回
// exit 0,stdout 是 shell 报错文本。必须按内容形态拦截,不能存进 soc/abi。
func TestProbeDevicesRejectsShellErrorTextWithZeroExit(t *testing.T) {
	shellErr := "/bin/bash: line 1: /system/bin/getprop: No such file or directory"
	runner := &fakeRunner{responses: map[string]adb.Result{
		"devices -l": {Stdout: "List of devices attached\nb5bb1018d94b26da device product:occam model:Nexus_4 device:mako\n"},
		"-s b5bb1018d94b26da shell /system/bin/getprop ro.product.cpu.abi": {Stdout: shellErr + "\n"},
		"-s b5bb1018d94b26da shell /bin/cat /proc/device-tree/compatible":  {ExitCode: 1},
	}}
	p := &Prober{Runner: runner}

	devices := p.ProbeDevices(context.Background(), map[string]bool{})
	if len(devices) != 1 {
		t.Fatalf("devices = %+v, want one device", devices)
	}
	got := devices[0]
	if got.State != DeviceOffline {
		t.Errorf("state = %q, want OFFLINE for non-Android shell", got.State)
	}
	if got.Props != nil && (got.Props.ABI == shellErr || got.Props.SOC == shellErr) {
		t.Errorf("shell error text leaked into props: %+v", got.Props)
	}
}

func TestProbeDevicesIdentifiesLinuxSOCAndReportsIdle(t *testing.T) {
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
	if got.State != DeviceIdle || got.DisplayName != "RK3588-b5bb1018d94b26da" {
		t.Errorf("Linux device = %+v, want RK3588 display name and IDLE(reachable Linux ADB device)", got)
	}
	if got.Props == nil || got.Props.SOC != "rk3588" {
		t.Errorf("Linux props = %+v, want soc rk3588", got.Props)
	}
}

// validSOC 单元测试:拒绝所有已知的 shell 报错形态,只接受真实 SoC 型号。
func TestValidSOCRejectsShellErrors(t *testing.T) {
	cases := []struct {
		val string
		ok  bool
	}{
		// 真实 SoC 型号
		{"qcm6125", true},
		{"mt8189", true},
		{"trinket", true},
		{"sdm845", true},
		{"exynos9810", true},
		{"rk3588", true},
		{"msm8953", true},
		{"tegra", true},
		// shell 报错文本
		{"/bin/bash: line 1: /system/bin/getprop: No such file or directory", false},
		{"/system/bin/sh: getprop: not found", false},
		{"-bash: /system/bin/getprop: No such file or directory", false},
		// 空格/大写/特殊字符
		{"QCM6125", false},
		{"qcm 6125", false},
		{"qcm\n6125", false},
		// 空字符串
		{"", false},
	}
	for _, tc := range cases {
		if got := validSOC(tc.val); got != tc.ok {
			t.Errorf("validSOC(%q) = %v, want %v", tc.val, got, tc.ok)
		}
	}
}

// ABI 有效但 SOC getprop 返回 shell 错误文本: SOC 应清空但不影响设备上线。
func TestProbeDevicesRejectsShellErrorSOCWithValidABI(t *testing.T) {
	shellErr := "/bin/bash: line 1: /system/bin/getprop: No such file or directory"
	runner := &fakeRunner{responses: map[string]adb.Result{
		"devices -l": {Stdout: "List of devices attached\nb5bb1018d94b26da device\n"},
		"-s b5bb1018d94b26da shell /system/bin/getprop ro.product.cpu.abi":       {Stdout: "arm64-v8a\n"},
		"-s b5bb1018d94b26da shell /system/bin/getprop ro.build.version.release": {Stdout: "12\n"},
		"-s b5bb1018d94b26da shell /system/bin/getprop ro.board.platform":        {Stdout: shellErr + "\n"},
		"-s b5bb1018d94b26da shell /system/bin/getprop ro.product.board":         {Stdout: shellErr + "\n"},
		"-s b5bb1018d94b26da shell /system/bin/df -k /data/local/tmp": {Stdout: "Filesystem 1K-blocks Used Available Use% Mounted on\n" +
			"/dev/block/dm-0 10000000 100 1000000 1% /data\n"},
	}}
	p := &Prober{Runner: runner}

	devices := p.ProbeDevices(context.Background(), map[string]bool{})
	if len(devices) != 1 {
		t.Fatalf("devices = %+v, want one device", devices)
	}
	got := devices[0]
	if got.State != DeviceIdle {
		t.Errorf("state = %q, want IDLE(ABI valid, SOC shell error only clears SOC)", got.State)
	}
	if got.Props == nil {
		t.Fatal("props is nil, want valid ABI")
	}
	if got.Props.SOC != "" {
		t.Errorf("SOC = %q, want empty(shell error rejected)", got.Props.SOC)
	}
	if got.Props.ABI != "arm64-v8a" {
		t.Errorf("ABI = %q, want arm64-v8a", got.Props.ABI)
	}
	// DisplayName fallback: UNKNOWN-{serial} when SOC is empty.
	if got.DisplayName != "UNKNOWN-b5bb1018d94b26da" {
		t.Errorf("display_name = %q, want UNKNOWN-b5bb1018d94b26da", got.DisplayName)
	}
}

// '?' transport 设备:ro.serialno 失败但设备树序列号存在时,应被成功解析为独立设备。
func TestProbeDevicesResolvesLinuxSerialViaDeviceTree(t *testing.T) {
	runner := &fakeRunner{responses: map[string]adb.Result{
		"devices -l": {Stdout: "List of devices attached\n? device product:rk3568-linux model:Nexus_4\n"},
		"-s ? shell /system/bin/getprop ro.serialno":                     {Stdout: "/bin/sh: line 1: /system/bin/getprop: No such file or directory\n"},
		"-s ? shell /bin/cat /proc/device-tree/serial-number":            {Stdout: "rk3568-evb-1\n", ExitCode: 0},
		"-s ? shell /system/bin/getprop ro.product.cpu.abi":              {Stdout: "/bin/sh: line 1: /system/bin/getprop: No such file or directory\n"},
		"-s ? shell /bin/cat /proc/device-tree/compatible":               {Stdout: "rockchip,rk3568\n", ExitCode: 0},
	}}
	p := &Prober{Runner: runner}

	devices := p.ProbeDevices(context.Background(), map[string]bool{})
	if len(devices) != 1 {
		t.Fatalf("devices = %+v, want one device", devices)
	}
	got := devices[0]
	if got.Serial != "rk3568-evb-1" {
		t.Errorf("serial = %q, want rk3568-evb-1", got.Serial)
	}
	if got.State != DeviceIdle {
		t.Errorf("state = %q, want IDLE(Linux ADB device, reachable)", got.State)
	}
	if got.Props == nil || got.Props.SOC != "rk3568" {
		t.Errorf("SOC = %q, want rk3568", got.Props.SOC)
	}
	if got.DisplayName != "RK3568-rk3568-evb-1" {
		t.Errorf("display_name = %q, want RK3568-rk3568-evb-1", got.DisplayName)
	}
}

// '?' transport 设备:ro.serialno 与设备树序列号均失败时,设备应被跳过。
func TestProbeDevicesRejectsShellErrorSerialResolution(t *testing.T) {
	shellErr := "/bin/sh: line 1: /system/bin/getprop: No such file or directory"
	runner := &fakeRunner{responses: map[string]adb.Result{
		"devices -l": {Stdout: "List of devices attached\n? device product:rk3568-linux model:Nexus_4\n"},
		"-s ? shell /system/bin/getprop ro.serialno": {Stdout: shellErr + "\n"},
		// 设备树序列号也不存在(未注册 → Run 报错)
	}}
	p := &Prober{Runner: runner}

	devices := p.ProbeDevices(context.Background(), map[string]bool{})
	if len(devices) != 0 {
		t.Fatalf("devices = %+v, want shell-error serial device skipped", devices)
	}
}
