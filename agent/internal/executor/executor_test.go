package executor

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"hermes-devops/agent/internal/adb"
	"hermes-devops/agent/internal/artifact"
)

const (
	serial  = "R5CT10XXXXX"
	workdir = "/data/local/tmp/tst"
	runSh   = "#!/system/bin/sh\nexit 0\n"
)

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// buildPackage 构造含 manifest.yaml 的合法测试包。
func buildPackage(t *testing.T, timeoutSec int) string {
	t.Helper()
	manifest := fmt.Sprintf(`manifest_version: 1
artifact: {project: p, commit: deadbee1, pipeline_id: 1, platform: aarch64_Android_SNPE_2.21, build_type: Release}
requirements: {os: android, abi: arm64-v8a, soc: [QCM6125], min_free_storage_mb: 100}
deploy:
  workdir: %s
  files:
    - {src: run.sh, dst: run.sh, mode: "0755", sha256: %s}
  env: {LD_LIBRARY_PATH: "{workdir}/lib"}
test:
  entry: ./run.sh
  args: ["--suite", "s"]
  timeout_sec: %d
  success: {exit_code: 0, require_files: [results/result.json]}
collect: [results/result.json, results/*.json, logs/*.log]
cleanup: {remove_workdir: true, keep_on_failure: true}
`, workdir, sha256hex(runSh), timeoutSec)

	path := filepath.Join(t.TempDir(), "pkg.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, e := range map[string]struct {
		data string
		mode int64
	}{
		"run.sh":        {runSh, 0o755},
		"manifest.yaml": {manifest, 0o644},
	} {
		hdr := &tar.Header{Name: name, Size: int64(len(e.data)), Mode: e.mode}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.data)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return path
}

// fakeADB 以 argv 模式匹配模拟设备行为,记录全部调用。
type fakeADB struct {
	mu            sync.Mutex
	calls         [][]string
	props         map[string]string
	dfAvailKB     int
	runExit       int
	runBlocks     bool
	deviceMissing bool // 模拟 -s 寻址不到设备:所有命令 exit=1 + stderr
}

func defaultProps() map[string]string {
	return map[string]string{
		"ro.product.cpu.abi":       "arm64-v8a",
		"ro.board.platform":        "qcm6125",
		"ro.build.version.release": "12",
	}
}

func (f *fakeADB) Run(ctx context.Context, args []string) (adb.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, args)
	f.mu.Unlock()

	if f.deviceMissing {
		return adb.Result{ExitCode: 1, Stderr: "adb: device '" + serial + "' not found\n"}, nil
	}

	cmd := args[2]
	switch {
	case cmd == "shell" && len(args) == 5 && args[3] == "getprop":
		return adb.Result{Stdout: f.props[args[4]] + "\n"}, nil
	case cmd == "shell" && len(args) == 6 && args[3] == "df":
		out := fmt.Sprintf("Filesystem 1K-blocks Used Available Use%% Mounted on\n/dev/block/dm-0 10000000 100 %d 1%% /data\n", f.dfAvailKB)
		return adb.Result{Stdout: out}, nil
	case cmd == "push" || cmd == "logcat":
		if cmd == "logcat" && args[3] == "-d" {
			return adb.Result{Stdout: "fake logcat content\n"}, nil
		}
		return adb.Result{}, nil
	case cmd == "pull":
		dest := args[4]
		os.MkdirAll(filepath.Dir(dest), 0o755)
		os.WriteFile(dest, []byte(`{"result_version":1}`), 0o644)
		return adb.Result{}, nil
	case cmd == "shell":
		s := args[3]
		switch {
		case strings.Contains(s, "ls -1d"):
			if strings.Contains(s, "logs/*.log") {
				return adb.Result{ExitCode: 1, Stderr: "no such file or directory"}, nil
			}
			return adb.Result{Stdout: "results/result.json\n"}, nil
		case strings.Contains(s, "'./run.sh'"):
			if f.runBlocks {
				<-ctx.Done()
				return adb.Result{ExitCode: -1}, ctx.Err()
			}
			return adb.Result{Stdout: "suite ok\n", ExitCode: f.runExit}, nil
		default: // mkdir/rm/chmod/pkill
			return adb.Result{}, nil
		}
	}
	return adb.Result{}, fmt.Errorf("fakeADB: unexpected argv %v", args)
}

func (f *fakeADB) find(substr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), substr) {
			return i
		}
	}
	return -1
}

func (f *fakeADB) count(substr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), substr) {
			n++
		}
	}
	return n
}

func newExecutor(f *fakeADB) (*Executor, *[]Status) {
	var transitions []Status
	e := &Executor{
		Runner:       f,
		Logf:         func(string, ...any) {},
		OnTransition: func(to Status) { transitions = append(transitions, to) },
	}
	return e, &transitions
}

func run(t *testing.T, f *fakeADB, opts Options) (*Summary, error, *[]Status) {
	t.Helper()
	e, tr := newExecutor(f)
	sum, err := e.Execute(context.Background(), opts)
	return sum, err, tr
}

func TestHappyPathLocalPackage(t *testing.T) {
	f := &fakeADB{props: defaultProps(), dfAvailKB: 1 << 20}
	out := t.TempDir()
	sum, err, tr := run(t, f, Options{PackagePath: buildPackage(t, 900), Serial: serial, OutDir: out})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := []Status{StatusPreparing, StatusDeploying, StatusRunning, StatusCollecting, StatusCompleted}
	if fmt.Sprint(*tr) != fmt.Sprint(want) {
		t.Errorf("transitions = %v, want %v", *tr, want)
	}
	if sum.Status != StatusCompleted || sum.ExitCode != 0 || !sum.SuccessCriteriaMet {
		t.Errorf("summary = %+v", sum)
	}
	// 两个 pattern 命中同一文件也只收集/记录一次
	if len(sum.Collected) != 1 || sum.Collected[0] != "results/result.json" {
		t.Errorf("collected = %v", sum.Collected)
	}
	if sum.DurationSec <= 0 {
		t.Errorf("duration_sec = %v, 必须记录实际执行时长", sum.DurationSec)
	}
	if sum.Environment["android"] != "12" || sum.Environment["soc"] != "qcm6125" {
		t.Errorf("environment = %v", sum.Environment)
	}
	// 顺序:清旧现场 → push → chmod → 执行 → 收集
	rm, push, chmod, runIdx, pull := f.find("rm -rf"), f.find("push"), f.find("chmod"), f.find("'./run.sh'"), f.find("pull")
	if !(rm < push && push < chmod && chmod < runIdx && runIdx < pull) {
		t.Errorf("order wrong: rm=%d push=%d chmod=%d run=%d pull=%d", rm, push, chmod, runIdx, pull)
	}
	// env 占位符已解析进执行命令
	f.mu.Lock()
	runCmd := strings.Join(f.calls[runIdx], " ")
	f.mu.Unlock()
	if !strings.Contains(runCmd, "LD_LIBRARY_PATH='"+workdir+"/lib'") {
		t.Errorf("run cmd missing resolved env: %s", runCmd)
	}
	// 本地产出
	for _, p := range []string{"run-summary.json", "logcat.txt", filepath.Join("device", "results", "result.json")} {
		if _, err := os.Stat(filepath.Join(out, p)); err != nil {
			t.Errorf("missing output %s: %v", p, err)
		}
	}
}

func TestDownloadPathEmitsDownloadingStatus(t *testing.T) {
	pkg := buildPackage(t, 900)
	data, _ := os.ReadFile(pkg)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(data)
	}))
	defer srv.Close()
	f := &fakeADB{props: defaultProps(), dfAvailKB: 1 << 20}
	e, tr := newExecutor(f)
	e.HTTP = srv.Client()
	sum, err := e.Execute(context.Background(), Options{
		PackageURL: srv.URL, SHA256: sha256hexBytes(data),
		Auth: &artifact.Auth{Type: "bearer", Token: "t"}, Serial: serial, OutDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if (*tr)[0] != StatusDownloading {
		t.Errorf("first transition = %v, want DOWNLOADING", (*tr)[0])
	}
	if sum.Status != StatusCompleted {
		t.Errorf("status = %v", sum.Status)
	}
}

func sha256hexBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestPrecheckABIMismatchFailsBeforeDeploy(t *testing.T) {
	props := defaultProps()
	props["ro.product.cpu.abi"] = "armeabi-v7a"
	f := &fakeADB{props: props, dfAvailKB: 1 << 20}
	sum, err, tr := run(t, f, Options{PackagePath: buildPackage(t, 900), Serial: serial, OutDir: t.TempDir()})
	if err == nil {
		t.Fatal("expected precheck error")
	}
	if sum.Status != StatusFailed {
		t.Errorf("status = %v", sum.Status)
	}
	if f.find("push") != -1 {
		t.Error("预检失败后不得 push")
	}
	for _, s := range *tr {
		if s == StatusDeploying {
			t.Error("预检失败后不得进入 DEPLOYING")
		}
	}
}

// 实机踩坑回归(2026-07-17):设备不可寻址时 adb 以非零退出码失败,
// 预检必须把 adb 的 stderr 带出来,而不是把空 stdout 误报成 ABI 不匹配。
func TestPrecheckSurfacesADBErrorWhenDeviceUnaddressable(t *testing.T) {
	f := &fakeADB{props: defaultProps(), dfAvailKB: 1 << 20, deviceMissing: true}
	sum, err, _ := run(t, f, Options{PackagePath: buildPackage(t, 900), Serial: serial, OutDir: t.TempDir()})
	if err == nil || sum.Status != StatusFailed {
		t.Fatalf("expected precheck failure, got %+v, err=%v", sum, err)
	}
	if strings.Contains(err.Error(), "abi mismatch") {
		t.Errorf("设备不可寻址不是 ABI 问题,报错误导: %v", err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("报错应包含 adb stderr 原文: %v", err)
	}
}

func TestPrecheckStorageInsufficientFails(t *testing.T) {
	f := &fakeADB{props: defaultProps(), dfAvailKB: 10} // manifest 需要 100MB
	sum, err, _ := run(t, f, Options{PackagePath: buildPackage(t, 900), Serial: serial, OutDir: t.TempDir()})
	if err == nil || sum.Status != StatusFailed {
		t.Fatalf("expected storage precheck failure, got %+v, err=%v", sum, err)
	}
}

func TestTimeoutKillsButStillCollects(t *testing.T) {
	f := &fakeADB{props: defaultProps(), dfAvailKB: 1 << 20, runBlocks: true}
	sum, err, _ := run(t, f, Options{PackagePath: buildPackage(t, 1), Serial: serial, OutDir: t.TempDir()})
	if err != nil {
		t.Fatalf("timeout 是正常结局,不应返回 error: %v", err)
	}
	if sum.Status != StatusTimeout {
		t.Errorf("status = %v, want TIMEOUT", sum.Status)
	}
	runIdx, pkill, pull := f.find("'./run.sh'"), f.find("pkill"), f.find("pull")
	if pkill == -1 {
		t.Error("超时后应 best-effort pkill")
	}
	if pull == -1 || pull < runIdx {
		t.Error("超时后仍须收集(pull)")
	}
}

func TestNonZeroExitIsCompletedButCriteriaNotMet(t *testing.T) {
	f := &fakeADB{props: defaultProps(), dfAvailKB: 1 << 20, runExit: 3}
	sum, err, _ := run(t, f, Options{PackagePath: buildPackage(t, 900), Serial: serial, OutDir: t.TempDir()})
	if err != nil {
		t.Fatalf("非零退出码是客观结果,不应返回 error: %v", err)
	}
	// status 与"成功判据"正交:进程跑完 = COMPLETED,判据是否满足单独记录
	if sum.Status != StatusCompleted || sum.ExitCode != 3 || sum.SuccessCriteriaMet {
		t.Errorf("summary = %+v", sum)
	}
}

func TestLocalPackageSHAMismatchFailsBeforeAnyADB(t *testing.T) {
	f := &fakeADB{props: defaultProps(), dfAvailKB: 1 << 20}
	sum, err, _ := run(t, f, Options{
		PackagePath: buildPackage(t, 900), SHA256: strings.Repeat("0", 64),
		Serial: serial, OutDir: t.TempDir(),
	})
	if err == nil || sum.Status != StatusFailed {
		t.Fatalf("expected sha mismatch failure, got %+v, err=%v", sum, err)
	}
	if len(f.calls) != 0 {
		t.Errorf("校验失败前不得触碰设备: %v", f.calls)
	}
}

// CANCELED 状态机:取消是客观结局(非 error),终态 CANCELED,
// 仍走 COLLECTING 与 cleanup(keep_on_failure 语义同其他异常结局)。
func TestCancelStateMachine(t *testing.T) {
	tests := []struct {
		name       string
		runBlocks  bool   // entry 阻塞直到 ctx 取消(模拟长任务)
		cancelOn   Status // 在该迁移时触发 Cancel;空 = 不中途取消
		cancelLate bool   // 终态后再 Cancel(幂等 no-op)
		wantFinal  Status
		wantPkill  bool
		wantPull   bool
		wantRm     int // rm -rf 次数:deploy 清旧现场 1 次;keep_on_failure 时 cleanup 不再删
	}{
		{"RUNNING 中取消:kill 设备进程后仍收集", true, StatusRunning, false,
			StatusCanceled, true, true, 1},
		{"RUNNING 前取消:下个边界中止并清理现场", false, StatusDeploying, false,
			StatusCanceled, false, false, 1},
		{"终态后取消:幂等 no-op", false, "", true,
			StatusCompleted, false, true, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeADB{props: defaultProps(), dfAvailKB: 1 << 20, runBlocks: tt.runBlocks}
			out := t.TempDir()
			e, tr := newExecutor(f)
			if tt.cancelOn != "" {
				base := e.OnTransition
				e.OnTransition = func(to Status) {
					base(to)
					if to == tt.cancelOn {
						e.Cancel()
						e.Cancel() // 幂等:重复调用无副作用
					}
				}
			}
			sum, err := e.Execute(context.Background(), Options{
				PackagePath: buildPackage(t, 900), Serial: serial, OutDir: out,
			})
			if err != nil {
				t.Fatalf("取消是客观结局,不应返回 error: %v", err)
			}
			if sum.Status != tt.wantFinal {
				t.Errorf("status = %v, want %v", sum.Status, tt.wantFinal)
			}
			if tt.cancelLate {
				callsBefore := f.count("")
				e.Cancel()
				e.Cancel()
				if got := f.count(""); got != callsBefore {
					t.Errorf("终态后 Cancel 不得触碰设备: %d → %d 次调用", callsBefore, got)
				}
				if sum.Status != StatusCompleted {
					t.Errorf("终态后 Cancel 不得改变结局: %v", sum.Status)
				}
				return
			}
			if sum.SuccessCriteriaMet {
				t.Error("取消的运行 success_criteria_met 必须为 false")
			}
			if got := f.find("pkill") != -1; got != tt.wantPkill {
				t.Errorf("pkill 调用 = %v, want %v", got, tt.wantPkill)
			}
			if got := f.find("pull") != -1; got != tt.wantPull {
				t.Errorf("collect(pull) = %v, want %v", got, tt.wantPull)
			}
			if got := f.count("rm -rf"); got != tt.wantRm {
				t.Errorf("rm -rf 次数 = %d, want %d(keep_on_failure 应保留现场)", got, tt.wantRm)
			}
			// 终态必须是最后一个迁移,且 run-summary.json 落盘
			if (*tr)[len(*tr)-1] != tt.wantFinal {
				t.Errorf("last transition = %v, want %v", (*tr)[len(*tr)-1], tt.wantFinal)
			}
			data, rerr := os.ReadFile(filepath.Join(out, "run-summary.json"))
			if rerr != nil || !strings.Contains(string(data), `"status": "`+string(tt.wantFinal)+`"`) {
				t.Errorf("run-summary.json 未记录 %v: %v, %s", tt.wantFinal, rerr, data)
			}
		})
	}
}

// RUNNING 中取消的专项:kill 发生在 run 之后、收集之前,且 logcat 仍落盘。
func TestCancelDuringRunningKillsThenCollects(t *testing.T) {
	f := &fakeADB{props: defaultProps(), dfAvailKB: 1 << 20, runBlocks: true}
	out := t.TempDir()
	e, _ := newExecutor(f)
	base := e.OnTransition
	e.OnTransition = func(to Status) {
		base(to)
		if to == StatusRunning {
			e.Cancel()
		}
	}
	sum, err := e.Execute(context.Background(), Options{
		PackagePath: buildPackage(t, 900), Serial: serial, OutDir: out,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if sum.Status != StatusCanceled {
		t.Fatalf("status = %v, want CANCELED", sum.Status)
	}
	runIdx, pkill, pull := f.find("'./run.sh'"), f.find("pkill"), f.find("pull")
	if !(runIdx != -1 && pkill > runIdx && pull > pkill) {
		t.Errorf("顺序应为 run → pkill → pull: run=%d pkill=%d pull=%d", runIdx, pkill, pull)
	}
	if _, err := os.Stat(filepath.Join(out, "logcat.txt")); err != nil {
		t.Errorf("取消后仍须落盘 logcat: %v", err)
	}
}

// 固件只报平台代号(trinket)时,SOCAliases 使命名约束(QCM6125)匹配成功;
// 无别名则应按 soc mismatch 失败(回归)。
func TestPrecheckSOCAlias(t *testing.T) {
	props := defaultProps()
	props["ro.board.platform"] = "trinket"
	props["ro.product.board"] = "trinket"

	// 无别名:soc mismatch
	f1 := &fakeADB{props: props, dfAvailKB: 1 << 20}
	_, err1, _ := run(t, f1, Options{PackagePath: buildPackage(t, 900), Serial: serial, OutDir: t.TempDir()})
	if err1 == nil || !strings.Contains(err1.Error(), "soc mismatch") {
		t.Fatalf("无别名应 soc mismatch, got %v", err1)
	}

	// 有别名:trinket→QCM6125 匹配通过
	f2 := &fakeADB{props: props, dfAvailKB: 1 << 20}
	e, _ := newExecutor(f2)
	e.SOCAliases = map[string]string{"trinket": "QCM6125"}
	sum, err := e.Execute(context.Background(), Options{PackagePath: buildPackage(t, 900), Serial: serial, OutDir: t.TempDir()})
	if err != nil {
		t.Fatalf("有别名不应失败: %v", err)
	}
	if sum.Environment["soc"] != "QCM6125" {
		t.Errorf("environment soc = %q, want QCM6125", sum.Environment["soc"])
	}
}

// COLLECTING 期间到达的取消同样生效:终态 CANCELED、判据不满足、仍完成收集。
func TestCancelDuringCollectingEndsCanceled(t *testing.T) {
	f := &fakeADB{props: defaultProps(), dfAvailKB: 1 << 20}
	e, _ := newExecutor(f)
	e.OnTransition = func(to Status) {
		if to == StatusCollecting {
			e.Cancel()
		}
	}
	sum, err := e.Execute(context.Background(), Options{PackagePath: buildPackage(t, 900), Serial: serial, OutDir: t.TempDir()})
	if err != nil {
		t.Fatalf("cancel 是客观结局,不应返回 error: %v", err)
	}
	if sum.Status != StatusCanceled {
		t.Errorf("status = %v, want CANCELED", sum.Status)
	}
	if sum.SuccessCriteriaMet {
		t.Error("取消后判据必须不满足")
	}
	if len(sum.Collected) == 0 {
		t.Error("取消不应跳过收集")
	}
}
