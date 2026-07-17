// agent-cli — Phase 1 先行的命令行执行器(CLAUDE.md §12.3)。
// 不做 RPC Server;完整实现 下载→校验→解压→预检→部署→执行→收集 闭环,
// 用于在 Windows+USB+ADB 环境手动踩坑,后续套服务壳复用同一 executor。
//
// 退出码: 0=COMPLETED 且成功判据满足; 2=COMPLETED 但判据不满足;
//         3=TIMEOUT; 1=FAILED/参数错误。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"hermes-devops/agent/internal/adb"
	"hermes-devops/agent/internal/artifact"
	"hermes-devops/agent/internal/executor"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	fs := flag.NewFlagSet("agent-cli run", flag.ContinueOnError)
	var (
		packageURL  = fs.String("package-url", "", "产物 Registry URL(与 --package-file 二选一)")
		packageFile = fs.String("package-file", "", "本地包路径(与 --package-url 二选一)")
		sha256Hex   = fs.String("sha256", "", "整包 sha256(package-url 时必填)")
		authType    = fs.String("auth-type", "", "bearer | job_token")
		authToken   = fs.String("auth-token", "", "下载凭据(建议用环境变量 AGENT_AUTH_TOKEN)")
		serial      = fs.String("serial", "", "目标设备序列号(必填)")
		adbPath     = fs.String("adb", "adb", "adb 可执行文件路径")
		outDir      = fs.String("out", "", "本地结果目录(默认 ./agent-runs/<UTC时间戳>)")
		keepWorkdir = fs.Bool("keep-device-workdir", false, "保留设备 workdir(覆盖 manifest.cleanup)")
	)
	if len(argv) < 1 || argv[0] != "run" {
		fmt.Fprintln(os.Stderr, "usage: agent-cli run [flags]")
		fs.PrintDefaults()
		return 1
	}
	if err := fs.Parse(argv[1:]); err != nil {
		return 1
	}
	if *serial == "" {
		fmt.Fprintln(os.Stderr, "error: --serial 必填(禁止无 -s 的 adb 操作)")
		return 1
	}
	if (*packageURL == "") == (*packageFile == "") {
		fmt.Fprintln(os.Stderr, "error: --package-url 与 --package-file 必须二选一")
		return 1
	}
	if *packageURL != "" && *sha256Hex == "" {
		fmt.Fprintln(os.Stderr, "error: --package-url 模式必须提供 --sha256")
		return 1
	}
	if *outDir == "" {
		*outDir = filepath.Join("agent-runs", time.Now().UTC().Format("20060102T150405Z"))
	}
	token := *authToken
	if token == "" {
		token = os.Getenv("AGENT_AUTH_TOKEN")
	}
	var auth *artifact.Auth
	if *authType != "" {
		auth = &artifact.Auth{Type: *authType, Token: token}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "%s "+format+"\n",
			append([]any{time.Now().UTC().Format("15:04:05.000")}, args...)...)
	}
	exec := &executor.Executor{
		Runner: &adb.ExecRunner{ADBPath: *adbPath},
		Logf:   logf,
	}
	keep := *keepWorkdir
	sum, err := exec.Execute(ctx, executor.Options{
		PackagePath:         *packageFile,
		PackageURL:          *packageURL,
		SHA256:              *sha256Hex,
		Auth:                auth,
		Serial:              *serial,
		OutDir:              *outDir,
		KeepWorkdirOverride: boolPtrIf(keep),
	})
	if err != nil {
		logf("FAILED: %v", err)
	}
	logf("status=%s exit_code=%d criteria_met=%v out=%s",
		sum.Status, sum.ExitCode, sum.SuccessCriteriaMet, sum.OutDir)

	switch sum.Status {
	case executor.StatusCompleted:
		if sum.SuccessCriteriaMet {
			return 0
		}
		return 2
	case executor.StatusTimeout:
		return 3
	default:
		return 1
	}
}

func boolPtrIf(keep bool) *bool {
	if !keep {
		return nil
	}
	return &keep
}
