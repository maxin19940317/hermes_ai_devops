package reporter

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"hermes-devops/agent/internal/adb"
	"hermes-devops/agent/internal/store"
)

// Prober 是设备发现与探测逻辑,由 Heartbeat 与 server 的
// GET /api/v1/devices 共用(设计 §3.3/§3.5):adb devices 发现 →
// 逐台 getprop 属性 + df 空间;busy 集合命中的记 BUSY,
// getprop 不可达的记 OFFLINE 并跳过后续探测。
type Prober struct {
	Runner adb.Runner                       // 可注入 fake
	Logf   func(format string, args ...any) // nil → 静默

	DeviceWorkdir string // df 探测路径;空 → DefaultDeviceWorkdir

	// SOCAliases 把 getprop 探测到的平台代号映射为调度约束使用的 SoC 名
	// (如 trinket → QCM6125)。设备固件通常只暴露平台代号,而 variants.yaml
	// 的调度约束用 SoC 型号;没有别名时 SNPE 变体永远匹配不到设备。
	SOCAliases map[string]string

	// Capabilities 声明本 Client 全部设备的能力(如 hexagon/rknpu),
	// 用于调度约束子集匹配。adb 没有可靠的通用能力探测手段,
	// 由运维按设备实际配置显式声明(同 SOCAliases 的显式原则)。
	Capabilities []string
}

func (p *Prober) deviceWorkdir() string {
	if p.DeviceWorkdir != "" {
		return p.DeviceWorkdir
	}
	return DefaultDeviceWorkdir
}

func (p *Prober) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
	}
}

// ProbeDevices 发现设备并逐台探测属性与空间。
func (p *Prober) ProbeDevices(ctx context.Context, busy map[string]bool) []DeviceInfo {
	devices := []DeviceInfo{}
	res, err := p.Runner.Run(ctx, adb.Devices())
	if err != nil {
		p.logf("probe: adb devices: %v", err)
		return devices
	}
	for _, serial := range adb.ParseDevices(res.Stdout) {
		devices = append(devices, p.probeDevice(ctx, serial, busy[serial]))
	}
	return devices
}

// probeDevice 探测单台设备。getprop 属性集与 executor 预检一致
// (ro.product.cpu.abi / ro.build.version.release / ro.board.platform,
// platform 取不到时回退 ro.product.board)。
func (p *Prober) probeDevice(ctx context.Context, serial string, isBusy bool) DeviceInfo {
	state := DeviceIdle
	if isBusy {
		state = DeviceBusy
	}
	dev := DeviceInfo{Serial: serial, State: state}

	abi, err := p.getprop(ctx, serial, "ro.product.cpu.abi")
	if err != nil {
		dev.State = DeviceOffline
		p.logf("probe: %s unreachable: %v", serial, err)
		return dev
	}
	props := &DeviceProps{ABI: abi}
	if release, err := p.getprop(ctx, serial, "ro.build.version.release"); err == nil {
		props.Android = release
	}
	soc, _ := p.getprop(ctx, serial, "ro.board.platform")
	if soc == "" {
		soc, _ = p.getprop(ctx, serial, "ro.product.board")
	}
	if alias, ok := p.SOCAliases[soc]; ok {
		p.logf("probe: %s soc %s -> %s (alias)", serial, soc, alias)
		soc = alias
	}
	props.SOC = soc
	if len(p.Capabilities) > 0 {
		props.Capabilities = append([]string(nil), p.Capabilities...)
	}
	dev.Props = props

	if res, err := p.Runner.Run(ctx, adb.DiskFreeKB(serial, p.deviceWorkdir())); err == nil && res.ExitCode == 0 {
		if kb, err := ParseDFAvailableKB(res.Stdout); err == nil && kb >= 0 {
			mb := kb / 1024
			dev.WorkdirFreeMB = &mb
		}
	}
	return dev
}

// getprop 取单个属性;非零退出码(设备掉线/unauthorized)视为不可达。
func (p *Prober) getprop(ctx context.Context, serial, prop string) (string, error) {
	res, err := p.Runner.Run(ctx, adb.GetProp(serial, prop))
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("getprop %s: exit=%d: %s", prop, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return strings.TrimSpace(res.Stdout), nil
}

// BusySerials 从 store 取非终态任务占用的设备 serial 集合
// (由 dispatch_json 的 device_serial 解析;解析失败只丢 BUSY 判定)。
// LoadInflight 失败时返回空集合(降级为全部 IDLE,不阻断探测)。
func BusySerials(ctx context.Context, st *store.Store, logf func(format string, args ...any)) map[string]bool {
	busy := map[string]bool{}
	inf, err := st.LoadInflight(ctx)
	if err != nil {
		if logf != nil {
			logf("busy serials: load inflight: %v", err)
		}
		return busy
	}
	for _, t := range inf.Tasks {
		var d struct {
			DeviceSerial string `json:"device_serial"`
		}
		if err := json.Unmarshal([]byte(t.DispatchJSON), &d); err == nil && d.DeviceSerial != "" {
			busy[d.DeviceSerial] = true
		}
	}
	return busy
}

// ParseDFAvailableKB 解析 `df -k` 输出的 Available 列(取最后一行数据;
// 与 executor 预检的解析规则一致)。
func ParseDFAvailableKB(out string) (int64, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("unexpected df output: %q", out)
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return 0, fmt.Errorf("unexpected df line: %q", lines[len(lines)-1])
	}
	kb, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse df available: %w", err)
	}
	return kb, nil
}
