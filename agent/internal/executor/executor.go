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
	"strconv"
	"strings"
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
}

// Executor 驱动流水线;设备交互全部经 Runner(可注入 fake 测试)。
type Executor struct {
	Runner       adb.Runner
	HTTP         *http.Client
	Logf         func(format string, args ...any)
	OnTransition func(to Status)
}

func (e *Executor) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

func (e *Executor) transition(sum *Summary, to Status) {
	sum.Status = to
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
	fail := func(err error) (*Summary, error) {
		e.transition(sum, StatusFailed)
		e.writeSummary(sum)
		return sum, err
	}

	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return fail(fmt.Errorf("create out dir: %w", err))
	}

	// ---- 获取包(DOWNLOADING 仅在需要下载时出现) ----
	pkgPath := opts.PackagePath
	if pkgPath == "" {
		if opts.PackageURL == "" || opts.SHA256 == "" {
			return fail(errors.New("package-url 模式必须提供 url 与 sha256"))
		}
		e.transition(sum, StatusDownloading)
		pkgPath = filepath.Join(opts.OutDir, "package.tar.gz")
		if err := artifact.Download(ctx, e.HTTP, opts.PackageURL, opts.Auth, pkgPath); err != nil {
			return fail(err)
		}
	}

	// ---- PREPARING: 整包校验 → 解压 → Manifest 校验 → 预检 ----
	e.transition(sum, StatusPreparing)
	if opts.SHA256 != "" {
		if err := artifact.VerifySHA256(pkgPath, opts.SHA256); err != nil {
			return fail(err)
		}
	}
	extractDir := filepath.Join(opts.OutDir, "package")
	if _, err := artifact.ExtractTarGz(pkgPath, extractDir); err != nil {
		return fail(fmt.Errorf("extract package: %w", err))
	}
	m, err := manifest.Load(filepath.Join(extractDir, "manifest.yaml"))
	if err != nil {
		return fail(err)
	}
	// 逐文件完整性:manifest 声明的 sha256 必须与解出内容一致
	for _, df := range m.Deploy.Files {
		if err := artifact.VerifySHA256(filepath.Join(extractDir, filepath.FromSlash(df.Src)), df.SHA256); err != nil {
			return fail(fmt.Errorf("deploy file integrity: %w", err))
		}
	}
	if err := e.precheck(ctx, opts.Serial, m, sum); err != nil {
		return fail(fmt.Errorf("device precheck: %w", err))
	}

	// ---- DEPLOYING: 清理旧现场 → push → chmod ----
	e.transition(sum, StatusDeploying)
	if err := e.deploy(ctx, opts.Serial, m, extractDir); err != nil {
		return fail(fmt.Errorf("deploy: %w", err))
	}

	// ---- RUNNING: 超时控制,超时 kill 但仍收集 ----
	e.transition(sum, StatusRunning)
	timedOut, res, duration, err := e.run(ctx, opts.Serial, m, opts.OutDir)
	sum.DurationSec = duration.Seconds()
	if err != nil {
		return fail(fmt.Errorf("run entry: %w", err))
	}
	sum.ExitCode = res.ExitCode

	// ---- COLLECTING ----
	e.transition(sum, StatusCollecting)
	deviceDir := filepath.Join(opts.OutDir, "device")
	sum.Collected = e.collect(ctx, opts.Serial, m, deviceDir)
	e.dumpLogcat(ctx, opts.Serial, opts.OutDir)
	sum.SuccessCriteriaMet = !timedOut &&
		res.ExitCode == m.Test.Success.ExitCode &&
		requireFilesPresent(deviceDir, m.Test.Success.RequireFiles)

	// ---- 设备清理(keep_on_failure 语义) ----
	e.cleanupDevice(ctx, opts.Serial, m, opts.KeepWorkdirOverride, timedOut || !sum.SuccessCriteriaMet)

	final := StatusCompleted
	if timedOut {
		final = StatusTimeout
	}
	e.transition(sum, final)
	e.writeSummary(sum)
	return sum, nil
}

// precheck 校验设备属性与空间(§12: getprop 属性 / df 空间)。
func (e *Executor) precheck(ctx context.Context, serial string, m *manifest.Manifest, sum *Summary) error {
	getprop := func(prop string) (string, error) {
		res, err := e.Runner.Run(ctx, adb.GetProp(serial, prop))
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(res.Stdout), nil
	}

	abi, err := getprop("ro.product.cpu.abi")
	if err != nil {
		return err
	}
	sum.Environment["abi"] = abi
	if abi != m.Requirements.ABI {
		return fmt.Errorf("abi mismatch: device=%s, required=%s", abi, m.Requirements.ABI)
	}

	if release, err := getprop("ro.build.version.release"); err == nil {
		sum.Environment["android"] = release
	}

	if len(m.Requirements.SOC) > 0 {
		platform, _ := getprop("ro.board.platform")
		board, _ := getprop("ro.product.board")
		matched := ""
		for _, want := range m.Requirements.SOC {
			for _, got := range []string{platform, board} {
				if got != "" && strings.EqualFold(got, want) {
					matched = got
				}
			}
		}
		if matched == "" {
			return fmt.Errorf("soc mismatch: device platform=%q board=%q, required one of %v",
				platform, board, m.Requirements.SOC)
		}
		sum.Environment["soc"] = matched
	}

	if m.Requirements.MinFreeStorageMB > 0 {
		res, err := e.Runner.Run(ctx, adb.DiskFreeKB(serial, path.Dir(m.Deploy.Workdir)))
		if err != nil {
			return err
		}
		availKB, err := parseDFAvailableKB(res.Stdout)
		if err != nil {
			return err
		}
		if availKB < int64(m.Requirements.MinFreeStorageMB)*1024 {
			return fmt.Errorf("insufficient storage: %d KB available, need %d MB",
				availKB, m.Requirements.MinFreeStorageMB)
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

func (e *Executor) deploy(ctx context.Context, serial string, m *manifest.Manifest, extractDir string) error {
	wd := m.Deploy.Workdir
	steps := [][]string{
		adb.ShellRemoveAll(serial, wd), // 清理旧现场
		adb.ShellMkdirAll(serial, wd),
	}
	for _, args := range steps {
		if res, err := e.Runner.Run(ctx, args); err != nil || res.ExitCode != 0 {
			return fmt.Errorf("workdir setup (%v): exit=%d err=%w", args, res.ExitCode, err)
		}
	}
	for _, df := range m.Deploy.Files {
		remote := path.Join(wd, df.Dst)
		if dir := path.Dir(remote); dir != wd {
			if _, err := e.Runner.Run(ctx, adb.ShellMkdirAll(serial, dir)); err != nil {
				return err
			}
		}
		local := filepath.Join(extractDir, filepath.FromSlash(df.Src))
		if res, err := e.Runner.Run(ctx, adb.Push(serial, local, remote)); err != nil || res.ExitCode != 0 {
			return fmt.Errorf("push %s: exit=%d err=%w", df.Src, res.ExitCode, err)
		}
		mode := df.Mode
		if mode == "" {
			mode = "0644"
		}
		if res, err := e.Runner.Run(ctx, adb.ShellChmod(serial, mode, remote)); err != nil || res.ExitCode != 0 {
			return fmt.Errorf("chmod %s: exit=%d err=%w", remote, res.ExitCode, err)
		}
	}
	return nil
}

// run 执行 entry。返回 timedOut 标志与实际时长;超时不算 error(仍需收集)。
func (e *Executor) run(ctx context.Context, serial string, m *manifest.Manifest, outDir string) (bool, adb.Result, time.Duration, error) {
	_, _ = e.Runner.Run(ctx, adb.LogcatClear(serial)) // best effort

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(m.Test.TimeoutSec)*time.Second)
	defer cancel()
	start := time.Now()
	res, err := e.Runner.Run(runCtx, adb.ShellRunEntry(
		serial, m.Deploy.Workdir, m.ResolvedEnv(), m.Test.Entry, m.Test.Args))
	duration := time.Since(start)

	_ = os.WriteFile(filepath.Join(outDir, "stdout.log"), []byte(res.Stdout), 0o644)
	_ = os.WriteFile(filepath.Join(outDir, "stderr.log"), []byte(res.Stderr), 0o644)

	timedOut := errors.Is(err, context.DeadlineExceeded)
	if timedOut {
		e.logf("entry timed out after %s, killing device process", duration)
		killCtx, killCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer killCancel()
		_, _ = e.Runner.Run(killCtx, adb.ShellPkill(serial, path.Base(m.Test.Entry)))
		err = nil
	}
	return timedOut, res, duration, err
}

// collect 按 Manifest collect 列表拉取产物,单项失败只记日志不中断;
// 多个 pattern 命中同一文件只拉取一次。
func (e *Executor) collect(ctx context.Context, serial string, m *manifest.Manifest, deviceDir string) []string {
	collected := []string{}
	seen := map[string]bool{}
	for _, pattern := range m.Collect {
		res, err := e.Runner.Run(ctx, adb.ShellListGlob(serial, m.Deploy.Workdir, pattern))
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
			if res, err := e.Runner.Run(ctx, adb.Pull(serial, remote, local)); err != nil || res.ExitCode != 0 {
				e.logf("collect %q: pull failed exit=%d err=%v", rel, res.ExitCode, err)
				continue
			}
			collected = append(collected, rel)
		}
	}
	return collected
}

func (e *Executor) dumpLogcat(ctx context.Context, serial, outDir string) {
	res, err := e.Runner.Run(ctx, adb.LogcatDump(serial))
	if err != nil {
		e.logf("logcat dump: %v", err)
		return
	}
	_ = os.WriteFile(filepath.Join(outDir, "logcat.txt"), []byte(res.Stdout), 0o644)
}

// cleanupDevice 按 manifest.cleanup 语义清理设备现场。
func (e *Executor) cleanupDevice(ctx context.Context, serial string, m *manifest.Manifest, override *bool, failed bool) {
	remove := m.Cleanup.RemoveWorkdir
	if failed && m.Cleanup.KeepOnFailure {
		remove = false
	}
	if override != nil {
		remove = !*override // override=keep
	}
	if remove {
		_, _ = e.Runner.Run(ctx, adb.ShellRemoveAll(serial, m.Deploy.Workdir))
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
