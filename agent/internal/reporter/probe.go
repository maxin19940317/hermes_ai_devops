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

	// Capabilities 是旧版单设备 Client 的能力声明;发现多台设备时忽略。
	Capabilities []string
	// DeviceCapabilities 按 serial 或最终 SoC(大小写不敏感)声明设备能力。
	DeviceCapabilities map[string][]string
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
	transports := adb.ParseTransports(res.Stdout)
	unknownCount := 0
	for _, transport := range transports {
		if transport == "?" {
			unknownCount++
		}
	}
	for _, transport := range transports {
		serial := transport
		if transport == "?" {
			if unknownCount != 1 {
				p.logf("probe: %d devices have unusable serial '?'; cannot resolve unambiguously", unknownCount)
				continue
			}
			serial, err = p.getprop(ctx, transport, "ro.serialno")
			if err != nil || serial == "" || serial == "?" {
				p.logf("probe: cannot resolve device serial '?' via ro.serialno: %v", err)
				continue
			}
			p.logf("probe: transport '?' resolved to serial %s", serial)
		}
		devices = append(devices, p.probeDevice(ctx, transport, serial, busy[serial], len(transports) == 1))
	}
	return devices
}

// probeDevice 探测单台设备。getprop 属性集与 executor 预检一致
// (ro.product.cpu.abi / ro.build.version.release / ro.board.platform,
// platform 取不到时回退 ro.product.board)。
func (p *Prober) probeDevice(ctx context.Context, transport, serial string, isBusy, allowLegacyCaps bool) DeviceInfo {
	state := DeviceIdle
	if isBusy {
		state = DeviceBusy
	}
	dev := DeviceInfo{Serial: serial, DisplayName: "UNKNOWN-" + serial, State: state}

	abi, err := p.getprop(ctx, transport, "ro.product.cpu.abi")
	if err == nil && !androidABI(abi) {
		// 老 adbd 不回传远程退出码且 adb 合并 stderr:getprop 不存在时
		// adb 仍回 exit 0,stdout 是 shell 报错文本(实测 Nexus 4)。
		// 靠退出码的防护会被绕过,按 abi 内容形态再拦一道。
		err = fmt.Errorf("abi %q is not an Android ABI (non-Android shell?)", abi)
	}
	if err != nil {
		dev.State = DeviceOffline
		if soc := p.linuxSOC(ctx, transport); soc != "" {
			dev.Props = &DeviceProps{SOC: soc}
			dev.DisplayName = strings.ToUpper(soc) + "-" + serial
			p.logf("probe: %s is non-Android (%s); reporting OFFLINE", serial, soc)
			return dev
		}
		p.logf("probe: %s unreachable or unsupported: %v", serial, err)
		return dev
	}
	props := &DeviceProps{ABI: abi}
	if release, err := p.getprop(ctx, transport, "ro.build.version.release"); err == nil {
		props.Android = release
	}
	soc, _ := p.getprop(ctx, transport, "ro.board.platform")
	if soc == "" {
		soc, _ = p.getprop(ctx, transport, "ro.product.board")
	}
	if alias, ok := p.SOCAliases[soc]; ok {
		p.logf("probe: %s soc %s -> %s (alias)", serial, soc, alias)
		soc = alias
	}
	props.SOC = soc
	if soc != "" {
		dev.DisplayName = strings.ToUpper(soc) + "-" + serial
	}
	props.Capabilities = p.capabilitiesFor(serial, soc, allowLegacyCaps)
	dev.Props = props

	if res, err := p.Runner.Run(ctx, adb.DiskFreeKB(transport, p.deviceWorkdir())); err == nil && res.ExitCode == 0 {
		if kb, err := ParseDFAvailableKB(res.Stdout); err == nil && kb >= 0 {
			mb := kb / 1024
			dev.WorkdirFreeMB = &mb
		}
	}
	return dev
}

// androidABI 校验 ro.product.cpu.abi 的内容形态(arm64-v8a / armeabi-v7a /
// x86 / x86_64 等):小写字母数字开头,仅含小写字母、数字、点、下划线、连字符。
func androidABI(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') ||
			(i > 0 && (r == '.' || r == '_' || r == '-'))
		if !ok {
			return false
		}
	}
	return true
}

func (p *Prober) capabilitiesFor(serial, soc string, allowLegacy bool) []string {
	for _, key := range []string{serial, soc} {
		if caps, ok := p.DeviceCapabilities[strings.ToLower(key)]; ok {
			return append([]string(nil), caps...)
		}
	}
	if allowLegacy {
		return append([]string(nil), p.Capabilities...)
	}
	return nil
}

func (p *Prober) linuxSOC(ctx context.Context, transport string) string {
	res, err := p.Runner.Run(ctx, adb.DeviceTreeCompatible(transport))
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	var soc string
	for _, compatible := range strings.FieldsFunc(res.Stdout, func(r rune) bool {
		return r == '\x00' || r == '\n' || r == '\r'
	}) {
		if _, suffix, ok := strings.Cut(compatible, ","); ok && suffix != "" {
			soc = suffix
		}
	}
	return soc
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
