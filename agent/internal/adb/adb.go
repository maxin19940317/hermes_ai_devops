// Package adb 提供模板化白名单 ADB 命令构造与执行。
// 红线(CLAUDE.md §14):不提供任意 shell 接口;一律 adb -s <serial>;
// 永不使用系统全局 adb server(5037),私有端口固定 5137。
package adb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// DefaultServerPort 私有 adb server 端口(CLAUDE.md §10)。
const DefaultServerPort = 5137

// Result 为一次 adb 调用的输出。
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Runner 执行以 argv(不含 adb 本体)描述的白名单命令。
type Runner interface {
	Run(ctx context.Context, args []string) (Result, error)
}

// Quote 单引号 shell 转义(' → '\”)。
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func withSerial(serial string, rest ...string) []string {
	return append([]string{"-s", serial}, rest...)
}

func withTransportID(id int, rest ...string) []string {
	return append([]string{"-t", strconv.Itoa(id)}, rest...)
}

// Target 描述一次 adb 调用要寻址的设备。
//
// 二选一:Serial 非空 → `adb -s <serial>`;TransportID 非零 → `adb -t <id>`。
// TransportID 用于 USB serial 丢失的设备(adb devices -l 显示 "?")——adb 对
// 这类设备无法用 -s 区分多台,但 -t <transport_id> 可精确寻址
// (adb -t 是官方寻址,commandline.cpp "-t ID use device with given transport id")。
// transport_id 在 adb server 会话内稳定;agent 重启/换 server 后可能变化,
// 因此 TransportID 只作为寻址手段,不构成设备身份。
type Target struct {
	Serial      string
	TransportID int
}

// TargetFor 构造寻址目标:serial 为 "?" 时优先用 transport_id(-t <id>,
// 可区分多台 ? 设备);transport_id 为 0(adb 未提供或单台 ? 场景)退化为
// `-s "?"`(adb 对唯一 ? 设备可直接寻址,ConfigFS 修复即依赖此行为)。
func TargetFor(serial string, transportID int) Target {
	if serial == "?" || serial == "" {
		if transportID != 0 {
			return Target{TransportID: transportID}
		}
		return Target{Serial: "?"}
	}
	return Target{Serial: serial}
}

// Argv 返回 adb 的寻址参数前缀([-s serial] 或 [-t id])。
func (t Target) Argv() []string {
	if t.TransportID != 0 {
		return withTransportID(t.TransportID)
	}
	return withSerial(t.Serial)
}

// String 是 Target 的可读形式(日志/报错用),不带敏感含义。
func (t Target) String() string {
	if t.TransportID != 0 {
		return fmt.Sprintf("transport#%d", t.TransportID)
	}
	return t.Serial
}

// ---- 白名单命令构造器(纯函数,唯一合法的命令来源) ----

func GetProp(t Target, prop string) []string {
	// 绝对路径:部分设备(Nexus 4 等)的 adb shell 走 /bin/bash,
	// /system/bin 不在 PATH 里,裸命令名 getprop 报 command not found。
	return append(t.Argv(), "shell", "/system/bin/getprop", prop)
}

// DeviceTreeCompatible 读取 Linux 设备的只读硬件兼容串。该命令无外部参数,
// 仅用于识别非 Android ADB 设备,不构成任意 shell 接口。
func DeviceTreeCompatible(t Target) []string {
	return append(t.Argv(), "shell", "/bin/cat", "/proc/device-tree/compatible")
}

// UnameM 读取 `uname -m` 获取 CPU 架构(Linux 设备通过 adb shell 执行)。
// 仅用于非 Android 设备的 ABI 识别,不构成任意 shell 接口。
func UnameM(t Target) []string {
	return append(t.Argv(), "shell", "uname", "-m")
}

// MapLinuxArchToABI 将 uname -m 输出映射为 Android NDK ABI 名。
func MapLinuxArchToABI(arch string) string {
	switch arch {
	case "aarch64":
		return "arm64-v8a"
	case "armv7l":
		return "armeabi-v7a"
	case "x86_64":
		return "x86_64"
	case "i686", "i386":
		return "x86"
	default:
		return arch
	}
}

// DeviceTreeSerialNumber 读取 Linux 设备树中的序列号。部分 Linux 板
// (如 rk3568) 的 USB iSerial 为空(adb devices 显示 "?"),
// 但 /proc/device-tree/serial-number 中存有唯一标识。
// 仅用于 ? transport 的 serial 解析回退,不构成任意 shell 接口。
func DeviceTreeSerialNumber(t Target) []string {
	return append(t.Argv(), "shell", "/bin/cat", "/proc/device-tree/serial-number")
}

// MachineID 读取 Linux 系统的持久唯一机器 ID(/etc/machine-id,由 systemd
// 在首次启动生成,32 位 hex)。用于无 ro.serialno 且无 device-tree serial 的
// Linux 板(2026-08-11 QCS6490 实机:uname qcs6490-odk,machine-id 6cfa3377...)
// 的 serial 解析回退。仅用于 ? transport 的身份解析,不构成任意 shell 接口。
func MachineID(t Target) []string {
	return append(t.Argv(), "shell", "/bin/cat", "/etc/machine-id")
}

func DiskFreeKB(t Target, path string) []string {
	return append(t.Argv(), "shell", "/system/bin/df", "-k", path)
}

// DiskFreeKBLinux Linux 设备上走 PATH 里的 df(无 /system/bin 前缀)。
// Android 设备仍用 DiskFreeKB(写死 /system/bin/df)。
func DiskFreeKBLinux(t Target, path string) []string {
	return append(t.Argv(), "shell", "df", "-k", path)
}

// MemTotalKB 读取 /proc/meminfo 的 MemTotal 行(Android/Linux 通用)。
// 无外部参数,仅用于设备基本信息上报,不构成任意 shell 接口。
func MemTotalKB(t Target) []string {
	return append(t.Argv(), "shell", "cat", "/proc/meminfo")
}

func Push(t Target, local, remote string) []string {
	return append(t.Argv(), "push", local, remote)
}

func Pull(t Target, remote, local string) []string {
	return append(t.Argv(), "pull", remote, local)
}

func ShellMkdirAll(t Target, dir string) []string {
	return append(t.Argv(), "shell", "mkdir -p "+Quote(dir))
}

func ShellRemoveAll(t Target, dir string) []string {
	return append(t.Argv(), "shell", "rm -rf "+Quote(dir))
}

func ShellChmod(t Target, mode, path string) []string {
	return append(t.Argv(), "shell", "chmod "+mode+" "+Quote(path))
}

func ShellPkill(t Target, pattern string) []string {
	return append(t.Argv(), "shell", "pkill -f "+Quote(pattern))
}

func LogcatClear(t Target) []string { return append(t.Argv(), "logcat", "-c") }
func LogcatDump(t Target) []string  { return append(t.Argv(), "logcat", "-d") }

// LogcatTail 拉取最近 lines 行 logcat(-d 立即返回不阻塞,-t 限定行数)。
// lines 按契约(client-agent-api openapi)钳制到 1..1000。
func LogcatTail(t Target, lines int) []string {
	if lines < 1 {
		lines = 1
	}
	if lines > 1000 {
		lines = 1000
	}
	return append(t.Argv(), "logcat", "-d", "-t", strconv.Itoa(lines))
}

// Devices 是白名单中唯一不带 -s <serial> 的命令:设备发现本身就是为了
// 拿到 serial,-s 无从填起。输出必须经 ParseDevices 过滤后才可使用。
func Devices() []string { return []string{"devices", "-l"} }

// Device 是 `adb devices -l` 解析出的单台设备条目。
// Serial 可能为 "?"(USB iSerial 丢失);此时 TransportID 非零,可用于
// `adb -t <id>` 精确寻址——adb 官方寻址方式,支持多台 ? 设备共存。
// Product 来自 -l 的 product: 字段(如 qcm6490-Ubuntu、trinket),可辅助
// 身份识别但不唯一。
type Device struct {
	Serial      string // 原始 serial;"?" = USB iSerial 丢失
	TransportID int    // -l 的 transport_id:N;多台 ? 时用于 -t 寻址
	Product     string // product: 字段;空 = 未提供
	Model       string // model: 字段;空 = 未提供
	State       string // device / offline / unauthorized 等
}

// ParseDeviceList 解析 `adb devices -l` 输出为结构化设备列表(保留表头校验)。
// 与 ParseDeviceStates 的区别:附带 transport_id/product 等寻址与身份信息。
func ParseDeviceList(out string) []Device {
	devices := []Device{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] == "List" {
			continue
		}
		d := Device{Serial: fields[0], State: fields[1]}
		for _, f := range fields[2:] {
			key, val, ok := strings.Cut(f, ":")
			if !ok {
				continue
			}
			switch key {
			case "transport_id":
				if n, err := strconv.Atoi(val); err == nil {
					d.TransportID = n
				}
			case "product":
				d.Product = val
			case "model":
				d.Model = val
			}
		}
		devices = append(devices, d)
	}
	return devices
}

// ParseDevices 解析 `adb devices -l` 输出,返回 state 为 "device" 的 serial。
// 跳过表头、空行与 unauthorized/offline 等不可用状态。serial 为 "?" 的
// 条目排除(无法用 -s 直接寻址;如需含 ? 的寻址目标请用 ParseTransports)。
func ParseDevices(out string) []string {
	serials := []string{}
	for _, d := range ParseDeviceList(out) {
		if d.State != "device" || d.Serial == "?" {
			continue
		}
		serials = append(serials, d.Serial)
	}
	return serials
}

// ParseTransports 与 ParseDevices 同构,但保留 "?" 条目:解析寻址
// (ResolveTransport)需要把它们作为候选 transport 逐个探测 ro.serialno——
// "?" 不能直接 -s 寻址,但可用 -t <transport_id>(Device.TransportID)。
// 返回 transport 的 Target 列表(替代旧式裸字符串列表)。
func ParseTransports(out string) []Target {
	targets := []Target{}
	for _, d := range ParseDeviceList(out) {
		if d.State != "device" {
			continue
		}
		targets = append(targets, TargetFor(d.Serial, d.TransportID))
	}
	return targets
}

// GetState 查询单台设备的连接状态(存活复核一级,spec §5.3)。
// 输出为 "device" / "offline" / "unauthorized" 之一,或非零退出。
func GetState(t Target) []string { return append(t.Argv(), "get-state") }

// ParseDeviceStates 解析 `adb devices -l`,返回 serial → state 全量映射。
//
// 与 ParseTransports 的区别:后者只保留 state == "device" 的行。
// 存活复核二级(spec §5.3)恰恰需要 offline / unauthorized —— 它们是
// "设备确实不可达"的证据,被丢掉就无法与"adb server 挂了"区分。
func ParseDeviceStates(out string) map[string]string {
	states := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] == "List" {
			continue
		}
		states[fields[0]] = fields[1]
	}
	return states
}

// ShellListGlob 在 workdir 内展开 glob。pattern 来自 Manifest collect 字段,
// 已由 Schema 限定字符集([A-Za-z0-9._*/-],无 ..),不加引号以保留 glob 展开。
func ShellListGlob(t Target, workdir, pattern string) []string {
	return append(t.Argv(), "shell", "cd "+Quote(workdir)+" && ls -1d "+pattern)
}

// ShellRunEntry 在 workdir 内以指定 env 执行 Manifest 声明的 entry。
// env 按 key 排序保证命令确定性。
func ShellRunEntry(t Target, workdir string, env map[string]string, entry string, args []string) []string {
	var b strings.Builder
	b.WriteString("cd " + Quote(workdir) + " &&")
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(" " + k + "=" + Quote(env[k]))
	}
	b.WriteString(" " + Quote(entry))
	for _, a := range args {
		b.WriteString(" " + Quote(a))
	}
	return append(t.Argv(), "shell", b.String())
}

// ExecRunner 是基于 os/exec 的真实 Runner,自带私有 server 端口环境变量。
type ExecRunner struct {
	ADBPath    string // adb 可执行文件路径
	ServerPort int    // 0 → DefaultServerPort
}

// commandEnv 返回子进程环境(含 ANDROID_ADB_SERVER_PORT,覆盖任何继承值)。
func (r *ExecRunner) commandEnv() []string {
	port := r.ServerPort
	if port == 0 {
		port = DefaultServerPort
	}
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "ANDROID_ADB_SERVER_PORT=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, fmt.Sprintf("ANDROID_ADB_SERVER_PORT=%d", port))
}

// LaunchError 表示 adb 二进制本身没能执行(缺失、不可执行、权限不足)。
// 归因意义:这是 Client 侧故障,与任何具体设备无关(spec §5.1)。
//
// 注意它**不**覆盖"私有 adb server 起不来":那种情况 adb 进程正常启动、
// 以非零退出码结束,走 ExitError 分支返回 (res, nil),连 error 都不是,
// 只能由 spec §5.3 的两级复核区分。
type LaunchError struct {
	Args []string
	Err  error
}

func (e *LaunchError) Error() string {
	return fmt.Sprintf("adb %s: %v", strings.Join(e.Args, " "), e.Err)
}

func (e *LaunchError) Unwrap() error { return e.Err }

func (r *ExecRunner) Run(ctx context.Context, args []string) (Result, error) {
	cmd := exec.CommandContext(ctx, r.ADBPath, args...)
	cmd.Env = r.commandEnv()
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return res, nil
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
		if ctx.Err() != nil { // 被超时/取消 kill
			return res, ctx.Err()
		}
		return res, nil // 非零退出码是客观结果,不作为 error
	default:
		return res, &LaunchError{Args: args, Err: err}
	}
}
