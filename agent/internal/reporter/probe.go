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
	// 2026-08-11:USB serial 丢失的设备(adb devices -l 显示 "?")不再因"多台
	// 无法区分"被跳过——用 transport_id 独立寻址(-t <id>),每台都能探测。
	// transport_id 是 adb server 会话内的稳定寻址,多台 ? 互不干扰。
	list := adb.ParseDeviceList(res.Stdout)
	n := len(list)
	for _, d := range list {
		if d.State != "device" {
			continue
		}
		target := adb.TargetFor(d.Serial, d.TransportID)
		serial := d.Serial
		if d.Serial == "?" {
			// 解析真实 serial(ro.serialno / 设备树)。2026-08-11 起用
			// transport_id 逐台寻址,多台 ? 也能独立解析;解析失败则跳过
			// (设备身份无法确定,避免用易变的 transport#N 污染设备表)。
			serial, err = p.resolveUnknownSerial(ctx, target)
			if err != nil || serial == "" {
				p.logf("probe: cannot resolve serial for transport %s: %v; skipping", target, err)
				continue
			}
			p.logf("probe: transport %s resolved to serial %s", target, serial)
		}
		devices = append(devices, p.probeDevice(ctx, target, serial, busy[serial], n == 1))
	}
	return devices
}

// probeDevice 探测单台设备。getprop 属性集与 executor 预检一致
// (ro.product.cpu.abi / ro.build.version.release / ro.board.platform,
// platform 取不到时回退 ro.product.board)。
func (p *Prober) probeDevice(ctx context.Context, t adb.Target, serial string, isBusy, allowLegacyCaps bool) DeviceInfo {
	state := DeviceIdle
	if isBusy {
		state = DeviceBusy
	}
	dev := DeviceInfo{Serial: serial, DisplayName: "UNKNOWN-" + serial, State: state}

	abi, err := p.getprop(ctx, t, "ro.product.cpu.abi")
	if err == nil && !androidABI(abi) {
		// 老 adbd 不回传远程退出码且 adb 合并 stderr:getprop 不存在时
		// adb 仍回 exit 0,stdout 是 shell 报错文本(实测 Nexus 4)。
		// 靠退出码的防护会被绕过,按 abi 内容形态再拦一道。
		err = fmt.Errorf("abi %q is not an Android ABI (non-Android shell?)", abi)
	}
	if err != nil {
		// 非 Android 但 ADB 可达的设备(如 rk3568 Linux 板):getprop 失败
		// 但设备树可读,不应标 OFFLINE——设备在线且 adb shell 连通,只是没有
		// Android getprop。标 IDLE 使其可被调度。
		if soc := p.linuxSOC(ctx, t); soc != "" {
			abi := p.linuxABI(ctx, t)
			dev.State = DeviceIdle
			// Linux 板 soc 同样走别名表(2026-08-11:此前 Linux 分支直用设备树
			// 代号如 qcm6490,不进 SOCAliases——与 Android 分支不一致,调度约束
			// 用型号(QCS6490)时永远匹配不到)。与 probe.go Android 路径同源。
			if alias, ok := adb.ResolveSOCAlias([]string{soc}, p.SOCAliases); ok {
				p.logf("probe: %s linux soc %s -> %s (alias)", serial, soc, alias)
				soc = alias
			}
			// Capabilities 与 Android 路径同源(probe.go:138):能力是配置声明
			// 而非探测,Linux 板同样需要(rknpu 调度约束,2026-08-05 实机遗漏)
			dev.Props = &DeviceProps{OS: "linux", SOC: soc, ABI: abi, Capabilities: p.capabilitiesFor(serial, soc, allowLegacyCaps)}
			dev.DisplayName = strings.ToUpper(soc) + "-" + serial
			p.logf("probe: %s is non-Android Linux (%s/%s); reporting IDLE", serial, soc, abi)
			p.probeMemTotal(ctx, t, dev.Props)
			return dev
		}
		dev.State = DeviceOffline
		p.logf("probe: %s unreachable or unsupported: %v", serial, err)
		return dev
	}
	props := &DeviceProps{OS: "android", ABI: abi}
	if release, err := p.getprop(ctx, t, "ro.build.version.release"); err == nil {
		props.Android = release
	}
	// 别名解析必须遍历**整条**探测链(2026-08-08 A1):别名表按平台代号为键
	// (trinket→QCM6125),而链的第一跳 ro.soc.model 给的是型号串。只拿首个值
	// 查别名时别名永不命中,设备会以型号串注册 → SelectTestSpecs 找不到匹配
	// 设备 → 变体被静默判 SKIPPED(无错误、无告警,最难排查的那种)。
	chain, err := adb.ProbeAndroidSOCChain(ctx, p.Runner, t)
	if err != nil {
		// 心跳探测是尽力而为(设计 §3.3):传输层失败按空链降级,不让
		// 单次探测失败拖垮整轮心跳,只留一条日志供排障。
		p.logf("probe: %s soc chain probe failed: %v", serial, err)
		chain = nil
	}
	soc := p.probeAndroidSOC(ctx, t)
	if alias, ok := adb.ResolveSOCAlias(chain, p.SOCAliases); ok {
		p.logf("probe: %s soc chain %v -> %s (alias)", serial, chain, alias)
		soc = alias
	}
	props.SOC = soc
	if soc != "" {
		dev.DisplayName = strings.ToUpper(soc) + "-" + serial
	}
	props.Capabilities = p.capabilitiesFor(serial, soc, allowLegacyCaps)
	dev.Props = props
	p.probeMemTotal(ctx, t, props)

	if res, err := p.Runner.Run(ctx, adb.DiskFreeKB(t, p.deviceWorkdir())); err == nil && res.ExitCode == 0 {
		if kb, err := ParseDFAvailableKB(res.Stdout); err == nil && kb >= 0 {
			mb := kb / 1024
			dev.WorkdirFreeMB = &mb
		}
	}
	return dev
}

// probeMemTotal 探测设备物理内存总量(/proc/meminfo MemTotal),写入 props。
// 失败静默(内存是展示信息,不是调度必要条件)。
func (p *Prober) probeMemTotal(ctx context.Context, t adb.Target, props *DeviceProps) {
	res, err := p.Runner.Run(ctx, adb.MemTotalKB(t))
	if err != nil || res.ExitCode != 0 {
		return
	}
	kb, err := ParseMemTotalKB(res.Stdout)
	if err != nil || kb <= 0 {
		return
	}
	mb := kb / 1024
	props.MemTotalMB = &mb
}

// probeAndroidSOC 自动探测 Android 设备的 SoC(2026-08-07):
// 探测链(按真实度优先):
//  1. ro.soc.model    真实 SoC 型号(如 SM6225、SM8250、SDM845)
//  2. ro.chipname     部分高通设备提供(如 sdm845、sm8350)
//  3. ro.board.platform  平台代号(如 bengal、trinket、idp)
//  4. ro.product.board   板名(兜底)
//
// 每步经 validSOC 内容形态校验;取到合法值即返回(不继续降级)。
// 目标:新设备接入即自动得到真实型号,无需手动维护 alias 表;
// SOCAliases 仍保留为探测链之后的最后兜底(兼容旧设备代号映射)。
// 实现委托给 adb.ProbeAndroidSOC——心跳与任务预检必须共用同一探测链
// (2026-08-08 Review P1:两套身份判断不得漂移)。
func (p *Prober) probeAndroidSOC(ctx context.Context, t adb.Target) string {
	soc := adb.ProbeAndroidSOC(ctx, p.Runner, t)
	if soc != "" && soc != t.String() {
		// 记录探测来源(仅日志;ro.board.platform 等代号级不额外标注)
		for _, prop := range []string{"ro.soc.model", "ro.chipname"} {
			if v, err := p.getprop(ctx, t, prop); err == nil && v == soc {
				p.logf("probe: %s %s=%q (auto-detected model)", t, prop, soc)
				break
			}
		}
	}
	return soc
}

// validSerial 校验 adb serial 形态:USB serial 为字母数字(可含 - _),
// 网络设备为 host:port。空串、"?"、含空白或路径符的一律非法。
func validSerial(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '-' || r == ':'
		if !ok {
			return false
		}
	}
	return true
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

// validSOC 校验 getprop ro.board.platform / ro.product.board / ro.soc.model 的
// 内容形态,拒绝 shell 错误文本被当做 SoC 型号(老 adbd 合并 stderr 到 stdout 且
// 不回传远程退出码,getprop 不存在时 stdout 是 "/bin/bash: line N: /system/bin/
// getprop: No such file" 等)。
// SoC 型号可含大写(真实型号如 SM6225、SM8250)或小写(平台代号如 bengal、trinket),
// 仅含字母数字与点、下划线、连字符。
func validSOC(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
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

func (p *Prober) linuxSOC(ctx context.Context, t adb.Target) string {
	res, err := p.Runner.Run(ctx, adb.DeviceTreeCompatible(t))
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
func (p *Prober) getprop(ctx context.Context, t adb.Target, prop string) (string, error) {
	res, err := p.Runner.Run(ctx, adb.GetProp(t, prop))
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

// ParseMemTotalKB 解析 /proc/meminfo 的 MemTotal 行("MemTotal:       5150140 kB"),
// 返回 kB;格式不符返回错误。内存总量是设备基本信息(飞书设备列表展示)。
func ParseMemTotalKB(out string) (int64, error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		// "MemTotal:" "5150140" "kB"
		if len(fields) < 2 {
			return 0, fmt.Errorf("unexpected MemTotal line: %q", line)
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse MemTotal: %w", err)
		}
		return kb, nil
	}
	return 0, fmt.Errorf("meminfo 无 MemTotal 行")
}

// resolveUnknownSerial 尝试解析 transport 为 "?" 的设备的真实序列号。
// 优先 ro.serialno(Android),失败则回退 /proc/device-tree/serial-number(Linux)。
func (p *Prober) resolveUnknownSerial(ctx context.Context, t adb.Target) (string, error) {
	// 尝试 Android getprop ro.serialno
	serial, err := p.getprop(ctx, t, "ro.serialno")
	if err == nil && validSerial(serial) {
		return serial, nil
	}
	// 回退 Linux 设备树序列号
	if res, dterr := p.Runner.Run(ctx, adb.DeviceTreeSerialNumber(t)); dterr == nil && res.ExitCode == 0 {
		// 设备树字符串按规范 NUL 结尾(RK3568 实测 ac6dcbcbfc640f3a\0):
		// 先按首个 NUL 截断再去空白,否则 validSerial 拒绝 NUL 字符。
		serial, _, _ := strings.Cut(res.Stdout, "\x00")
		serial = strings.TrimSpace(serial)
		if validSerial(serial) {
			return serial, nil
		}
	}
	return "", fmt.Errorf("cannot resolve serial for transport '?'")
}

func (p *Prober) linuxABI(ctx context.Context, t adb.Target) string {
	res, err := p.Runner.Run(ctx, adb.UnameM(t))
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	return adb.MapLinuxArchToABI(strings.TrimSpace(res.Stdout))
}
