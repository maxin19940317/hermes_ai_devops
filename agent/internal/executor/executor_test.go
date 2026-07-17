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
