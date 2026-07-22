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

// ---- 白名单命令构造器(纯函数,唯一合法的命令来源) ----

func GetProp(serial, prop string) []string {
	return withSerial(serial, "shell", "getprop", prop)
}

func DiskFreeKB(serial, path string) []string {
	return withSerial(serial, "shell", "df", "-k", path)
}

func Push(serial, local, remote string) []string {
	return withSerial(serial, "push", local, remote)
}

func Pull(serial, remote, local string) []string {
	return withSerial(serial, "pull", remote, local)
}

func ShellMkdirAll(serial, dir string) []string {
	return withSerial(serial, "shell", "mkdir -p "+Quote(dir))
}

func ShellRemoveAll(serial, dir string) []string {
	return withSerial(serial, "shell", "rm -rf "+Quote(dir))
}

func ShellChmod(serial, mode, path string) []string {
	return withSerial(serial, "shell", "chmod "+mode+" "+Quote(path))
}

func ShellPkill(serial, pattern string) []string {
	return withSerial(serial, "shell", "pkill -f "+Quote(pattern))
}

func LogcatClear(serial string) []string { return withSerial(serial, "logcat", "-c") }
func LogcatDump(serial string) []string  { return withSerial(serial, "logcat", "-d") }

// LogcatTail 拉取最近 lines 行 logcat(-d 立即返回不阻塞,-t 限定行数)。
// lines 按契约(client-agent-api openapi)钳制到 1..1000。
func LogcatTail(serial string, lines int) []string {
	if lines < 1 {
		lines = 1
	}
	if lines > 1000 {
		lines = 1000
	}
	return withSerial(serial, "logcat", "-d", "-t", strconv.Itoa(lines))
}

// Devices 是白名单中唯一不带 -s <serial> 的命令:设备发现本身就是为了
// 拿到 serial,-s 无从填起。输出必须经 ParseDevices 过滤后才可使用。
func Devices() []string { return []string{"devices", "-l"} }

// ParseDevices 解析 `adb devices -l` 输出,返回 state 为 "device" 的 serial。
// 跳过表头、空行与 unauthorized/offline 等不可用状态;serial 为 "?" 的
// 条目无法被 -s 寻址(USB 缺 iSerial,见 agent/README.md 踩坑记录),一并跳过。
func ParseDevices(out string) []string {
	serials := []string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != "device" || fields[0] == "?" {
			continue
		}
		serials = append(serials, fields[0])
	}
	return serials
}

// ShellListGlob 在 workdir 内展开 glob。pattern 来自 Manifest collect 字段,
// 已由 Schema 限定字符集([A-Za-z0-9._*/-],无 ..),不加引号以保留 glob 展开。
func ShellListGlob(serial, workdir, pattern string) []string {
	return withSerial(serial, "shell", "cd "+Quote(workdir)+" && ls -1d "+pattern)
}

// ShellRunEntry 在 workdir 内以指定 env 执行 Manifest 声明的 entry。
// env 按 key 排序保证命令确定性。
func ShellRunEntry(serial, workdir string, env map[string]string, entry string, args []string) []string {
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
	return withSerial(serial, "shell", b.String())
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
		return res, fmt.Errorf("adb %s: %w", strings.Join(args, " "), err)
	}
}
