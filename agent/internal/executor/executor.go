// Package executor 实现 agent-cli 的确定性执行流水线(CLAUDE.md §12 Phase 1.3):
// 下载 → 整包校验 → 解压 → Manifest 校验 → 设备预检 → 清理旧现场
// → push → chmod/env → 执行(超时 kill 但仍收集) → pull collect → 本地结果目录。
// status 与 verdict 正交:本层只产 status,verdict 由 Runtime 判定。
package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"hermes-devops/agent/internal/adb"
	"hermes-devops/agent/internal/artifact"
	"hermes-devops/agent/internal/manifest"
)

// Status 为任务生命周期状态的 Client 可见子集(CLAUDE.md §9)。
type Status string

const (
	StatusPreparing   Status = "PREPARING"
	StatusDownloading Status = "DOWNLOADING"
	StatusDeploying   Status = "DEPLOYING"
	StatusRunning     Status = "RUNNING"
	StatusCollecting  Status = "COLLECTING"
	StatusCompleted   Status = "COMPLETED"
	StatusFailed      Status = "FAILED"
	StatusTimeout     Status = "TIMEOUT"
	StatusCanceled    Status = "CANCELED"
)

// Options 描述一次运行的输入。PackagePath 与 PackageURL 二选一。
type Options struct {
	PackagePath string // 本地包(跳过下载)
	PackageURL  string
	SHA256      string // PackageURL 时必填;PackagePath 时可选(填了就校验)
	Auth        *artifact.Auth

	Serial string
	OutDir string // 本地结果根目录

	KeepWorkdirOverride *bool // 覆盖 manifest.cleanup(nil = 按 manifest)
}

// Summary 是一次运行的客观记录(不含 verdict)。
type Summary struct {
	Status             Status            `json:"status"`
	ExitCode           int               `json:"exit_code"`
	DurationSec        float64           `json:"duration_sec"`
	SuccessCriteriaMet bool              `json:"success_criteria_met"`
	Collected          []string          `json:"collected"`
	Environment        map[string]string `json:"environment"`
	OutDir             string            `json:"out_dir"`
	// FailureScope/FailureStage 是主失败归因(spec §5/§6),仅在走 fail()
	// 返回路径的站点赋值;best-effort 路径(collect 等)一律不碰,详见
	// classifyFailure 与 Execute 内各 fail 调用点。
	FailureScope string `json:"failure_scope,omitempty"`
	FailureStage string `json:"failure_stage,omitempty"`
}

// Executor 驱动流水线;设备交互全部经 Runner(可注入 fake 测试)。
// 一个 Executor 对应一次运行;Cancel 可从其他 goroutine 并发调用。
type Executor struct {
	Runner       adb.Runner
	HTTP         *http.Client
	Logf         func(format string, args ...any)
	OnTransition func(to Status)

	// SOCAliases 把设备固件上报的平台代号(如 trinket)映射为
	// manifest 调度约束使用的 SoC 型号(如 QCM6125),precheck 的
	// soc 匹配在映射后进行;nil 表示不映射。
	SOCAliases map[string]string

	mu              sync.Mutex
	status          Status             // 当前状态(供 Cancel 判断终态)
	cancelRequested bool               // 取消标志(置位后不可复位)
	runCancel       context.CancelFunc // RUNNING 期间非 nil,Cancel 用它解除执行阻塞
}

// Cancel 请求取消当前运行:幂等,可与 Execute 并发调用。
// RUNNING 中取消会解除 Runner.Run 阻塞,由 run() 按超时同一路径 kill
// 设备端进程,流水线继续走 COLLECTING → cleanup → 终态 CANCELED;
// 更早阶段取消则在下一个阶段边界中止;已终态后调用无副作用。
func (e *Executor) Cancel() {
	e.mu.Lock()
	if e.cancelRequested || isTerminal(e.status) {
		e.mu.Unlock()
		return
	}
	e.cancelRequested = true
	runCancel := e.runCancel
	e.mu.Unlock()
	if runCancel != nil {
		runCancel()
	}
}

func (e *Executor) isCancelRequested() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cancelRequested
}

// isTerminal 报告 status 是否为终态(CLAUDE.md §9)。
func isTerminal(s Status) bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusTimeout, StatusCanceled:
		return true
	}
	return false
}

func (e *Executor) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

func (e *Executor) transition(sum *Summary, to Status) {
	sum.Status = to
	e.mu.Lock()
	e.status = to
	e.mu.Unlock()
	e.logf("→ %s", to)
	if e.OnTransition != nil {
		e.OnTransition(to)
	}
}

// Execute 运行完整流水线。返回的 Summary 总是非 nil,出错时也尽量填充。
// TIMEOUT 与非零退出码是客观结局,不作为 error 返回;FAILED 伴随 error。
func (e *Executor) Execute(ctx context.Context, opts Options) (*Summary, error) {
	sum := &Summary{
		Status:      StatusFailed,
		ExitCode:    -1,
		Environment: map[string]string{"serial": opts.Serial},
		OutDir:      opts.OutDir,
	}
	// fail 是唯一给 FailureScope/FailureStage 赋值的路径(spec §6 防线 1):
	// best-effort 站点(collect 等)不经过它,一律不碰这两个字段。
	// stage 必须落在契约枚举内:resolve|precheck|download|unpack|deploy|run|collect。
	fail := func(stage string, err error) (*Summary, error) {
		sum.FailureStage = stage
		sum.FailureScope = e.classifyFailure(ctx, adb.TargetFor(opts.Serial, 0), stage, err)
		e.transition(sum, StatusFailed)
		e.writeSummary(sum)
		return sum, err
	}

	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return fail("download", fmt.Errorf("create out dir: %w", err))
	}

	// ---- 获取包(DOWNLOADING 仅在需要下载时出现) ----
	pkgPath := opts.PackagePath
	if pkgPath == "" {
		if opts.PackageURL == "" || opts.SHA256 == "" {
			return fail("download", errors.New("package-url 模式必须提供 url 与 sha256"))
		}
		e.transition(sum, StatusDownloading)
		pkgPath = filepath.Join(opts.OutDir, "package.tar.gz")
		if err := artifact.Download(ctx, e.HTTP, opts.PackageURL, opts.Auth, pkgPath); err != nil {
			return fail("download", err)
		}
	}

	// ---- PREPARING: 整包校验 → 解压 → Manifest 校验 → 预检 ----
	e.transition(sum, StatusPreparing)
	if opts.SHA256 != "" {
		if err := artifact.VerifySHA256(pkgPath, opts.SHA256); err != nil {
			return fail("download", err)
		}
	}
	extractDir := filepath.Join(opts.OutDir, "package")
	if _, err := artifact.ExtractTarGz(pkgPath, extractDir); err != nil {
		return fail("unpack", fmt.Errorf("extract package: %w", err))
	}
	m, err := manifest.Load(filepath.Join(extractDir, "manifest.yaml"))
	if err != nil {
		return fail("unpack", err)
	}
	// 逐文件完整性:manifest 声明的 sha256 必须与解出内容一致
	for _, df := range m.Deploy.Files {
		if err := artifact.VerifySHA256(filepath.Join(extractDir, filepath.FromSlash(df.Src)), df.SHA256); err != nil {
			return fail("unpack", fmt.Errorf("deploy file integrity: %w", err))
		}
	}
	// 寻址解析(放在包校验之后:校验不过不得触碰设备):USB gadget serial 丢失时
	// adb 只显示 "?",`adb -s <ro.serialno>` 全部 not found(2026-07-30 实机踩坑)。
	// 此时逐个 transport(含多台 ? 的 transport_id)探测 ro.serialno,返回可寻址 Target。
	target, err := e.resolveTransport(ctx, opts.Serial)
	if err != nil {
		return fail("resolve", err)
	}
	if target.String() != opts.Serial {
		e.logf("serial resolve: %s -> transport %s", opts.Serial, target)
	}
	if err := e.precheck(ctx, target, m, sum); err != nil {
		return fail("precheck", fmt.Errorf("device precheck: %w", err))
	}

	// ---- DEPLOYING: 清理旧现场 → push → chmod ----
	// 阶段边界:取消在设备改动前到达,直接终态 CANCELED(无设备现场可清)
	if e.isCancelRequested() {
		return e.finishCanceled(sum)
	}
	e.transition(sum, StatusDeploying)
	if err := e.deploy(ctx, target, m, extractDir); err != nil {
		return fail("deploy", fmt.Errorf("deploy: %w", err))
	}

	// ---- RUNNING: 超时控制,超时 kill 但仍收集 ----
	// 阶段边界:设备现场已建,取消仍须按 keep_on_failure 语义清理
	if e.isCancelRequested() {
		e.cleanupDevice(ctx, target, m, opts.KeepWorkdirOverride, true)
		return e.finishCanceled(sum)
	}
	e.transition(sum, StatusRunning)
	canceled, timedOut, res, duration, err := e.run(ctx, target, m, opts.OutDir)
	sum.DurationSec = duration.Seconds()
	if err != nil {
		return fail("run", fmt.Errorf("run entry: %w", err))
	}
	sum.ExitCode = res.ExitCode

	// ---- COLLECTING ----
	e.transition(sum, StatusCollecting)
	deviceDir := filepath.Join(opts.OutDir, "device")
	sum.Collected = e.collect(ctx, target, m, deviceDir)
	e.dumpLogcat(ctx, target, opts.OutDir)
	sum.SuccessCriteriaMet = !canceled && !timedOut &&
		res.ExitCode == m.Test.Success.ExitCode &&
		requireFilesPresent(deviceDir, m.Test.Success.RequireFiles)

	// ---- 设备清理(keep_on_failure 语义;取消同其他异常结局) ----
	e.cleanupDevice(ctx, target, m, opts.KeepWorkdirOverride, canceled || timedOut || !sum.SuccessCriteriaMet)

	// 收集/清理期间到达的取消同样生效:设备进程已自然结束(无需 kill),
	// 终态记 CANCELED,判据置不满足。
	canceled = canceled || e.isCancelRequested()
	if canceled {
		sum.SuccessCriteriaMet = false
	}

	final := StatusCompleted
	switch {
	case canceled:
		final = StatusCanceled
	case timedOut:
		final = StatusTimeout
	}
	e.transition(sum, final)
	e.writeSummary(sum)
	return sum, nil
}

// finishCanceled 以终态 CANCELED 收尾;取消是客观结局(同 TIMEOUT),不作为 error。
func (e *Executor) finishCanceled(sum *Summary) (*Summary, error) {
	e.transition(sum, StatusCanceled)
	e.writeSummary(sum)
	return sum, nil
}

// errAdbServer 标识 adb server / 宿主机级故障(非设备故障)。
// 归因为 client(spec §5.1);包裹后用 errors.Is 判别。
var errAdbServer = errors.New("adb server failure")

// 归因判定用的 sentinel(spec §5.1/§5.3.1)。各失败站点用 %w 包裹对应
// sentinel,classifyFailure 才能用 errors.Is 识别失败的语义类别而不必解析
// 错误文本(红线:不解析 stderr 文本做判断)。
var (
	// errNoMatch 标识 resolveTransport 走完全部候选后仍未能把逻辑 serial
	// 绑定到任何 transport(spec §5.3.1)。逻辑 serial 与 transport 本来就
	// 允许不同(USB gadget serial 丢失场景),不能因为没匹配上就判 device。
	errNoMatch = errors.New("no transport matched logical serial")
	// errSOCMismatch/errABIMismatch 标识属性读取成功但比较不符——这是任务
	// 派错了板或服务端配置漂移,不是设备故障(spec §3)。
	errSOCMismatch = errors.New("soc mismatch")
	errABIMismatch = errors.New("abi mismatch")
	// errNoSpace 标识 df 成功执行但可用空间不足:设备当下状态是自足证据,
	// 不需要走存活复核(spec §5.1 表倒数第三行)。
	errNoSpace = errors.New("insufficient storage")
	// errRemoteExit 标识针对已定位 transport 的调用以非零退出结束。
	// `adb shell` 透传远端命令的退出码(spec §5.2),所以它默认不是设备
	// 证据——只有经两级存活复核(livenessScope)确认设备不可达才升级为
	// device;分类交给 classifyFailure 的默认分支处理,这里只负责标记
	// "这是一次已定位 transport 上的远端命令失败"。
	errRemoteExit = errors.New("remote command exited non-zero")
)

// classifyFailure 按 spec §5 判定主失败归因。全程只看错误的调用层级来源
// (sentinel/类型)与阶段名,不解析 stderr 文本——文案一改归因就会失效。
//
// 分支顺序是判定正确性的一部分:ctx 取消/超时必须最先判,否则会被后面更
// 具体的 stage/sentinel 分支错误吞掉(例如下载阶段被取消不该判成 client)。
func (e *Executor) classifyFailure(ctx context.Context, t adb.Target, stage string, err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "none" // 多义:不归任何一方
	case errors.As(err, new(*adb.LaunchError)), errors.Is(err, errAdbServer):
		return "client" // 本地进程/server 层,与具体设备无关
	case errors.Is(err, errNoSpace):
		return "device" // df 已成功执行 → 设备明确活着,数值不足是自足证据
	case errors.Is(err, errSOCMismatch), errors.Is(err, errABIMismatch):
		return "none" // 属性可读但不匹配 = 任务派错了板
	case errors.Is(err, errNoMatch):
		return "none" // resolve 阶段:逻辑 serial 与 transport 允许不同(§5.3.1)
	case stage == "download":
		return "client" // 本地磁盘/网络故障,尚未触碰任何设备
	case stage == "unpack":
		// tar 解压失败 / manifest 不合法 / 逐文件 sha256 不符,全是构建产物
		// (CI/打包)的问题,不是这台 Windows client 的问题——归 client 会让
		// 恰好领到坏包的那台 client 失败计数上涨,把健康 client 显示成故障中
		// (§1 描述的"记错账"同一种病)。无法可靠区分产物问题 vs 本机问题,
		// 按 §5.1 末行保守默认为 none。
		return "none"
	case stage == "resolve":
		return "none" // resolve 未确定 transport,两级复核无从执行,保守(§5.3.1)
	}
	return e.livenessScope(ctx, t)
}

// livenessScope 是 spec §5.3 的两级存活复核,调用失败升级为 device 的唯一
// 一般性途径。全程只看 exit code 与结构化状态字段,不解析 stderr 文本。
func (e *Executor) livenessScope(ctx context.Context, t adb.Target) string {
	// 一级:目标设备自身状态。
	if res, err := e.Runner.Run(ctx, adb.GetState(t)); err == nil &&
		res.ExitCode == 0 && strings.TrimSpace(res.Stdout) == "device" {
		return "none" // 设备活着,失败另有原因
	}
	// 二级:全局列表。它成功本身就证明 adb server 与宿主机是好的,此时
	// 目标 transport 缺席或状态异常才构成设备证据;不能复用 ParseTransports,
	// 它只保留 state=="device" 的行,offline/unauthorized 恰恰是这里最需要
	// 的证据。
	res, err := e.Runner.Run(ctx, adb.Devices())
	if err != nil || res.ExitCode != 0 {
		return "none" // 排除不掉 server/宿主机故障,保守
	}
	// 二级匹配:Target 是 transport#N 时,按 transport_id 定位;否则按 serial。
	// 不能用 ParseDeviceStates(它按 serial 作键,"?" 和 transport#N 都无法直接匹配)。
	found := false
	for _, d := range adb.ParseDeviceList(res.Stdout) {
		if d.TransportID == t.TransportID && d.State == "device" {
			found = true
			break
		}
		if d.Serial != "?" && d.Serial == t.Serial && d.State == "device" {
			found = true
			break
		}
	}
	if !found {
		return "device" // server 好的,设备却缺席或异常 → 设备证据
	}
	return "none" // 矛盾(列表说好、一级说坏),保守
}

// resolveTransport 把逻辑 serial(ro.serialno)解析为可寻址的 Target。
// 快路径:serial 本身就在 devices 列表中,原样返回,零额外调用。
// 慢路径:USB gadget serial 丢失(列表显示 "?")时,逐个 transport 探测
// ro.serialno(多台 ? 用 transport_id 寻址),返回匹配者;Linux 设备无
// getprop,再回退 /proc/device-tree/serial-number;全部不匹配报可见列表
// 便于排查(这不代表设备故障——逻辑 serial 与 transport 允许不同,见 §5.3.1)。
func (e *Executor) resolveTransport(ctx context.Context, logical string) (adb.Target, error) {
	res, err := e.Runner.Run(ctx, adb.Devices())
	if err != nil {
		return adb.Target{}, fmt.Errorf("resolve serial: adb devices: %w: %w", errAdbServer, err)
	}
	// 非零退出同样是 server 级故障:此前未检查,会让残缺 stdout 流向
	// "device not found",把 server 故障伪装成设备不存在(spec §5.4 第 2 处)
	if res.ExitCode != 0 {
		return adb.Target{}, fmt.Errorf("resolve serial: adb devices exit=%d: %s: %w",
			res.ExitCode, strings.TrimSpace(res.Stderr), errAdbServer)
	}
	devices := adb.ParseDeviceList(res.Stdout)
	// 快路径:逻辑 serial 直接命中设备 Serial(非 ?)。
	for _, d := range devices {
		if d.State != "device" {
			continue
		}
		if d.Serial == logical {
			return adb.TargetFor(d.Serial, d.TransportID), nil
		}
	}
	// 慢路径:逐个可寻址 transport 探测,匹配逻辑 serial。
	// "?" 设备用 transport_id 寻址(-t <id>),因此多台 ? 也能独立探测。
	for _, d := range devices {
		if d.State != "device" {
			continue
		}
		t := adb.TargetFor(d.Serial, d.TransportID)
		res, err := e.Runner.Run(ctx, adb.GetProp(t, "ro.serialno"))
		if err == nil && strings.TrimSpace(res.Stdout) == logical {
			return t, nil
		}
		// Linux 设备无 getprop(2026-08-04 RK3568 实机:/system/bin/getprop
		// 不存在,stdout 空不匹配 → "device not found via adb"):
		// 回退设备树序列号;设备树字符串 NUL 结尾,先截断(与 probe 一致)。
		if res, dterr := e.Runner.Run(ctx, adb.DeviceTreeSerialNumber(t)); dterr == nil && res.ExitCode == 0 {
			serial, _, _ := strings.Cut(res.Stdout, "\x00")
			if strings.TrimSpace(serial) == logical {
				return t, nil
			}
		}
		// 回退 /etc/machine-id(2026-08-11 QCS6490 实机:无 device-tree serial,
		// 用持久机器唯一 ID 作身份;与 probe resolveUnknownSerial 同源)。
		if res, mterr := e.Runner.Run(ctx, adb.MachineID(t)); mterr == nil && res.ExitCode == 0 {
			if strings.TrimSpace(res.Stdout) == logical {
				return t, nil
			}
		}
	}
	visible := make([]string, 0, len(devices))
	for _, d := range devices {
		if d.State == "device" {
			visible = append(visible, adb.TargetFor(d.Serial, d.TransportID).String())
		}
	}
	return adb.Target{}, fmt.Errorf("device %q not found via adb (visible transports: %s): %w",
		logical, strings.Join(visible, ", "), errNoMatch)
}

// precheck 校验设备属性与空间(§12: getprop 属性 / df 空间)。
// Linux 设备分支(Phase 4):getprop 不可用,走 uname -m 校验 abi,跳过 android/soc
// (soc 已由 Runtime selector 在派单前保证);df 走原生路径。
func (e *Executor) precheck(ctx context.Context, t adb.Target, m *manifest.Manifest, sum *Summary) error {
	if m.Requirements.OS == "linux" {
		return e.precheckLinux(ctx, t, m, sum)
	}
	return e.precheckAndroid(ctx, t, m, sum)
}

// precheckAndroid Android 设备属性校验。
func (e *Executor) precheckAndroid(ctx context.Context, t adb.Target, m *manifest.Manifest, sum *Summary) error {
	getprop := func(prop string) (string, error) {
		res, err := e.Runner.Run(ctx, adb.GetProp(t, prop))
		if err != nil {
			return "", err
		}
		// 非零退出码通常是设备不可寻址(not found/unauthorized/offline),
		// 必须带出 adb stderr,不能让空 stdout 伪装成属性值。这是一次针对
		// 已定位 transport 的调用非零退出,归因默认 none,只有存活复核确认
		// 设备不可达才升级为 device(spec §5.2/§5.3)。
		if res.ExitCode != 0 {
			return "", fmt.Errorf("adb getprop %s: exit=%d: %s: %w",
				prop, res.ExitCode, strings.TrimSpace(res.Stderr), errRemoteExit)
		}
		return strings.TrimSpace(res.Stdout), nil
	}

	abi, err := getprop("ro.product.cpu.abi")
	if err != nil {
		return err
	}
	sum.Environment["abi"] = abi
	if abi != m.Requirements.ABI {
		return fmt.Errorf("abi mismatch: device=%s, required=%s: %w", abi, m.Requirements.ABI, errABIMismatch)
	}

	if release, err := getprop("ro.build.version.release"); err == nil {
		sum.Environment["android"] = release
	}

	if len(m.Requirements.SOC) > 0 {
		// SoC 探测复用与心跳同一链(ro.soc.model → ro.chipname →
		// ro.board.platform → ro.product.board)。
		// 2026-08-08 Review P1:心跳按 ro.soc.model 调度成功、预检却只读
		// ro.board.platform 会造成 soc mismatch——两套身份判断必须同源。
		//
		// 2026-08-08 A1:匹配必须遍历**整条链**,不能只用首个命中值。
		// 别名表按平台代号为键(trinket→QCM6125),而链的第一跳给的是型号串;
		// 只查首个值时别名根本没机会参与,直接匹配又不上 → 误报 soc mismatch。
		chain, err := adb.ProbeAndroidSOCChain(ctx, e.Runner, t)
		if err != nil {
			// 空链 + 传输层失败:设备很可能已经掉线,不能把它当 soc
			// mismatch 报——那会被归因体系误读成"配置问题"而非设备故障
			// (Task 6 据此做归因判断)。原样把错误向上抛出。
			return err
		}
		matched := ""
	matchLoop:
		for _, want := range m.Requirements.SOC {
			// 1) 链上任一候选直接匹配 manifest 要求
			for _, soc := range chain {
				if strings.EqualFold(soc, want) {
					matched = soc
					break matchLoop
				}
			}
			// 2) 链上任一候选经别名表规范化后匹配
			//    (脏配置拆分与服务端 SOC 清洗语义一致,见 ResolveSOCAlias)
			for _, soc := range chain {
				alias, ok := adb.ResolveSOCAlias([]string{soc}, e.SOCAliases)
				if ok && strings.EqualFold(alias, want) {
					matched = alias
					break matchLoop
				}
			}
		}
		if matched == "" {
			return fmt.Errorf("soc mismatch: device soc chain=%v, required one of %v: %w",
				chain, m.Requirements.SOC, errSOCMismatch)
		}
		sum.Environment["soc"] = matched
	}

	if m.Requirements.MinFreeStorageMB > 0 {
		res, err := e.Runner.Run(ctx, adb.DiskFreeKB(t, path.Dir(m.Deploy.Workdir)))
		if err != nil {
			return err
		}
		availKB, err := parseDFAvailableKB(res.Stdout)
		if err != nil {
			return err
		}
		if availKB < int64(m.Requirements.MinFreeStorageMB)*1024 {
			return fmt.Errorf("insufficient storage: %d KB available, need %d MB: %w",
				availKB, m.Requirements.MinFreeStorageMB, errNoSpace)
		}
	}
	return nil
}

// parseDFAvailableKB 解析 `df -k` 输出的 Available 列(取最后一行数据)。
func parseDFAvailableKB(out string) (int64, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("unexpected df output: %q", out)
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return 0, fmt.Errorf("unexpected df line: %q", lines[len(lines)-1])
	}
	return strconv.ParseInt(fields[3], 10, 64)
}

func (e *Executor) deploy(ctx context.Context, t adb.Target, m *manifest.Manifest, extractDir string) error {
	wd := m.Deploy.Workdir
	// remoteRun 统一远端命令的错误语义:非零退出包 errRemoteExit
	// (spec §5.2——退出码是远端命令的,不是设备的,归因交给
	// classifyFailure 的存活复核默认分支);Runner.Run 自身返回的 err
	// (如 adb.LaunchError)原样 %w 透传,保留错误链供 errors.As 识别。
	remoteRun := func(desc string, args []string) error {
		res, err := e.Runner.Run(ctx, args)
		if err != nil {
			return fmt.Errorf("%s: %w", desc, err)
		}
		if res.ExitCode != 0 {
			// stdout 必须带上:adb 的本地侧错误(cannot stat 超长路径文件等)
			// 打在 stdout 而非 stderr,只拼 stderr 会得到 exit=1 + 空 stderr
			// 的诊断黑洞(2026-08-10 实机,TFLite 变体长文件名 push 失败)。
			return fmt.Errorf("%s: exit=%d stderr=%q stdout=%q: %w",
				desc, res.ExitCode, strings.TrimSpace(res.Stderr), truncForErr(res.Stdout), errRemoteExit)
		}
		return nil
	}

	steps := [][]string{
		adb.ShellRemoveAll(t, wd), // 清理旧现场
		adb.ShellMkdirAll(t, wd),
	}
	for _, args := range steps {
		if err := remoteRun(fmt.Sprintf("workdir setup (%v)", args), args); err != nil {
			return err
		}
	}
	for _, df := range m.Deploy.Files {
		remote := path.Join(wd, df.Dst)
		if dir := path.Dir(remote); dir != wd {
			// exit code 必须检查:只查 Go error 时 mkdir 静默失败,真实错误
			// 推迟到 push 才暴露且丢失 stderr(2026-08-04 RK3568 实机)
			if err := remoteRun("mkdir "+dir, adb.ShellMkdirAll(t, dir)); err != nil {
				return err
			}
		}
		local := filepath.Join(extractDir, filepath.FromSlash(df.Src))
		if err := remoteRun("push "+df.Src, adb.Push(t, local, remote)); err != nil {
			return err
		}
		mode := df.Mode
		if mode == "" {
			mode = "0644"
		}
		if err := remoteRun("chmod "+remote, adb.ShellChmod(t, mode, remote)); err != nil {
			return err
		}
	}
	return nil
}

// truncForErr 截断进入错误串的命令输出,防爆(adb stdout 偶尔带大段传输统计)。
func truncForErr(s string) string {
	const max = 512
	s = strings.TrimSpace(s)
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// run 执行 entry。返回 canceled/timedOut 标志与实际时长;
// 超时与取消都是客观结局不算 error(仍需收集),取消复用超时 kill 路径。
func (e *Executor) run(ctx context.Context, t adb.Target, m *manifest.Manifest, outDir string) (bool, bool, adb.Result, time.Duration, error) {
	if m.Requirements.OS != "linux" {
		_, _ = e.Runner.Run(ctx, adb.LogcatClear(t)) // best effort, Android only
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(m.Test.TimeoutSec)*time.Second)
	defer cancel()
	// 注册 runCancel 供 Cancel() 解除 Runner.Run 阻塞;
	// 取消若恰好先于注册到达,立即 cancel 让执行快速退出
	e.mu.Lock()
	if e.cancelRequested {
		cancel()
	} else {
		e.runCancel = cancel
	}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.runCancel = nil
		e.mu.Unlock()
	}()

	start := time.Now()
	res, err := e.Runner.Run(runCtx, adb.ShellRunEntry(
		t, m.Deploy.Workdir, m.ResolvedEnv(), m.Test.Entry, m.Test.Args))
	duration := time.Since(start)

	_ = os.WriteFile(filepath.Join(outDir, "stdout.log"), []byte(res.Stdout), 0o644)
	_ = os.WriteFile(filepath.Join(outDir, "stderr.log"), []byte(res.Stderr), 0o644)

	timedOut := errors.Is(err, context.DeadlineExceeded)
	canceled := e.isCancelRequested()
	if timedOut || canceled {
		if timedOut {
			e.logf("entry timed out after %s, killing device process", duration)
		} else {
			e.logf("cancel requested after %s, killing device process", duration)
		}
		killCtx, killCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer killCancel()
		_, _ = e.Runner.Run(killCtx, adb.ShellPkill(t, path.Base(m.Test.Entry)))
		err = nil
	}
	return canceled, timedOut, res, duration, err
}

// collectPatternOK 是 collect pattern 的字符白名单:只允许文件名常用字符
// 与 glob 通配符。Pattern 来自 Manifest collect 字段,虽经 Schema 校验,但
// Schema 无法表达字符级正则——此处是纵深防御,防止恶意 manifest 注入任意
// shell(审查 #3)。
var collectPatternOK = regexp.MustCompile(`^[A-Za-z0-9._*/-]+$`)

// collect 按 Manifest collect 列表拉取产物,单项失败只记日志不中断;
// 多个 pattern 命中同一文件只拉取一次。
func (e *Executor) collect(ctx context.Context, t adb.Target, m *manifest.Manifest, deviceDir string) []string {
	collected := []string{}
	seen := map[string]bool{}
	for _, pattern := range m.Collect {
		// 纵深防御:Schema 已限定字符集,此处再次校验防止 Schema 漂移或绕过
		if !collectPatternOK.MatchString(pattern) {
			e.logf("collect %q: pattern contains disallowed characters, skipping", pattern)
			continue
		}
		res, err := e.Runner.Run(ctx, adb.ShellListGlob(t, m.Deploy.Workdir, pattern))
		if err != nil || res.ExitCode != 0 {
			e.logf("collect %q: no match (exit=%d)", pattern, res.ExitCode)
			continue
		}
		for _, line := range strings.Split(res.Stdout, "\n") {
			rel := strings.TrimSpace(line)
			if rel == "" || seen[rel] {
				continue
			}
			seen[rel] = true
			local := filepath.Join(deviceDir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
				e.logf("collect %q: mkdir: %v", rel, err)
				continue
			}
			remote := path.Join(m.Deploy.Workdir, rel)
			if res, err := e.Runner.Run(ctx, adb.Pull(t, remote, local)); err != nil || res.ExitCode != 0 {
				e.logf("collect %q: pull failed exit=%d err=%v", rel, res.ExitCode, err)
				continue
			}
			collected = append(collected, rel)
		}
	}
	return collected
}

func (e *Executor) dumpLogcat(ctx context.Context, t adb.Target, outDir string) {
	// Linux 设备无 logcat,跳过。
	// 注:m 参数不可靠(disconnect 后的 collect 重试可能拿不到 manifest),
	// 此处用 adb 本身判断:Android shell 有 /system/bin/logcat,Linux 没有。
	res, err := e.Runner.Run(ctx, adb.LogcatDump(t))
	if err != nil {
		// 失败可能来自 Linux 设备(无 logcat)或 Android 设备 logcat 异常;
		// 不做 OS 分类判定——logcat 缺失不构成故障。
		e.logf("logcat dump: %v", err)
		return
	}
	_ = os.WriteFile(filepath.Join(outDir, "logcat.txt"), []byte(res.Stdout), 0o644)
}

// cleanupDevice 按 manifest.cleanup 语义清理设备现场。
func (e *Executor) cleanupDevice(ctx context.Context, t adb.Target, m *manifest.Manifest, override *bool, failed bool) {
	remove := m.Cleanup.RemoveWorkdir
	if failed && m.Cleanup.KeepOnFailure {
		remove = false
	}
	if override != nil {
		remove = !*override // override=keep
	}
	if remove {
		_, _ = e.Runner.Run(ctx, adb.ShellRemoveAll(t, m.Deploy.Workdir))
	} else {
		e.logf("keeping device workdir %s", m.Deploy.Workdir)
	}
}

func requireFilesPresent(deviceDir string, files []string) bool {
	for _, rf := range files {
		if _, err := os.Stat(filepath.Join(deviceDir, filepath.FromSlash(rf))); err != nil {
			return false
		}
	}
	return true
}

func (e *Executor) writeSummary(sum *Summary) {
	data, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(sum.OutDir, "run-summary.json"), append(data, '\n'), 0o644)
}

// precheckLinux Linux 设备属性校验(Phase 4):getprop 不可用,走 uname -m 校验 abi;
// soc 已由 Runtime selector 在派单前保证(devices 表 os/soc 列),
// selector 无匹配设备时该变体在 SelectTestSpecs 即被 SKIPPED,不会走到这里;
// df 走原生 Linux 路径。
func (e *Executor) precheckLinux(ctx context.Context, t adb.Target, m *manifest.Manifest, sum *Summary) error {
	res, err := e.Runner.Run(ctx, adb.UnameM(t))
	if err != nil {
		return fmt.Errorf("linux precheck: uname -m: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("linux precheck: uname -m: exit=%d: %s: %w",
			res.ExitCode, strings.TrimSpace(res.Stderr), errRemoteExit)
	}
	arch := strings.TrimSpace(res.Stdout)
	abi := adb.MapLinuxArchToABI(arch)
	sum.Environment["abi"] = abi
	if abi != m.Requirements.ABI {
		return fmt.Errorf("abi mismatch: device=%s (uname=%s), required=%s: %w", abi, arch, m.Requirements.ABI, errABIMismatch)
	}
	sum.Environment["os"] = "linux"

	if m.Requirements.MinFreeStorageMB > 0 {
		res, err := e.Runner.Run(ctx, adb.DiskFreeKBLinux(t, path.Dir(m.Deploy.Workdir)))
		if err != nil {
			return err
		}
		availKB, err := parseDFAvailableKB(res.Stdout)
		if err != nil {
			return err
		}
		if availKB < int64(m.Requirements.MinFreeStorageMB)*1024 {
			return fmt.Errorf("insufficient storage: %d KB available, need %d MB: %w",
				availKB, m.Requirements.MinFreeStorageMB, errNoSpace)
		}
	}
	return nil
}
