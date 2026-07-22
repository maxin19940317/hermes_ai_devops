// agent — Client Agent 服务模式(设计文档 §3.6,CLAUDE.md §12 Phase 1.7)。
// 在 agent-cli 的执行能力上套 RPC 壳:HTTP Server 接收 Runtime 派单,
// 心跳/事件/结果经 callbacks-api 回流;崩溃重启后从 SQLite 恢复补报。
//
// 用法:
//
//	agent [run] [-config FILE]   前台运行(默认子命令)
//	agent install|uninstall      安装/卸载系统服务(Windows Service / systemd 等)
//	agent start|stop             启动/停止已安装的服务
//
// 配置:环境变量 + 可选 -config 文件(KEY=VALUE 每行一条,# 开头为注释;
// 环境变量优先级高于配置文件)。键:
//
//	AGENT_CLIENT_ID              必填,本 Client 标识
//	AGENT_RUNTIME_CALLBACK_URL   必填,Runtime callbacks 基地址(如 http://host:18091)
//	AGENT_BASE_URL               必填,本 Agent 的 API 基地址(心跳上送,Runtime 派单用)
//	AGENT_ADB_PATH               必填,adb 可执行文件路径
//	AGENT_LISTEN_ADDR            可选,HTTP 监听地址(默认 :8480)
//	AGENT_VERSION                可选,Agent 版本(默认 dev)
//	AGENT_RUNS_ROOT              可选,本地结果根目录(默认 ./agent-runs)
//	AGENT_DB_PATH                可选,SQLite 路径(默认 ./agent.db)
//	AGENT_HEARTBEAT_INTERVAL     可选,心跳周期,Go duration(默认 10s)
//	AGENT_SOC_ALIASES            可选,平台代号→SoC 型号别名(如 trinket:QCM6125,多个用逗号分隔)
//	AGENT_DEVICE_CAPABILITIES    可选,设备能力声明(如 hexagon,多个用逗号分隔;调度子集匹配用)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kardianos/service"

	"hermes-devops/agent/internal/adb"
	"hermes-devops/agent/internal/executor"
	"hermes-devops/agent/internal/reporter"
	"hermes-devops/agent/internal/server"
	"hermes-devops/agent/internal/store"
	"hermes-devops/agent/internal/uploader"
)

// Config 是服务模式配置(键见包注释)。
type Config struct {
	ClientID           string
	RuntimeCallbackURL string
	BaseURL            string
	ADBPath            string
	ListenAddr         string
	Version            string
	RunsRoot           string
	DBPath             string
	HeartbeatInterval  time.Duration
	SOCAliases         map[string]string
	Capabilities       []string
}

// parseCSV 解析逗号分隔列表(AGENT_DEVICE_CAPABILITIES),去空白;空串返回 nil。
func parseCSV(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		if v := strings.TrimSpace(item); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// parseSOCAliases 解析 "trinket:QCM6125,kalama:SM8550" 形式的
// 平台代号→SoC 型号别名表(AGENT_SOC_ALIASES)。空串返回 nil。
func parseSOCAliases(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	aliases := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		from, to, ok := strings.Cut(strings.TrimSpace(pair), ":")
		if !ok || from == "" || strings.TrimSpace(to) == "" {
			return nil, fmt.Errorf("AGENT_SOC_ALIASES 项 %q 非法(要 from:to)", pair)
		}
		aliases[from] = strings.TrimSpace(to)
	}
	return aliases, nil
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	cmd := "run"
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		cmd, argv = argv[0], argv[1:]
	}
	switch cmd {
	case "run", "install", "uninstall", "start", "stop":
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q (run|install|uninstall|start|stop)\n", cmd)
		return 1
	}

	fs := flag.NewFlagSet("agent "+cmd, flag.ContinueOnError)
	configPath := fs.String("config", "", "配置文件路径(KEY=VALUE;环境变量优先)")
	if err := fs.Parse(argv); err != nil {
		return 1
	}

	cfg, err := loadConfig(*configPath, os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	svc, err := service.New(&program{cfg: cfg}, &service.Config{
		Name:        "hermes-devops-agent",
		DisplayName: "Hermes DevOps Agent",
		Description: "Hermes DevOps Client Agent(接收 Runtime 派单,执行设备测试并回流结果)",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	switch cmd {
	case "run": // 前台:kardianos 等待 SIGTERM/SIGINT 后调用 Stop
		err = svc.Run()
	case "install":
		err = svc.Install()
	case "uninstall":
		err = svc.Uninstall()
	case "start":
		err = svc.Start()
	case "stop":
		err = svc.Stop()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		return 1
	}
	return 0
}

// loadConfig 读取可选 KEY=VALUE 配置文件后以环境变量覆盖,校验必填项。
func loadConfig(path string, getenv func(string) string) (Config, error) {
	vals := map[string]string{}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("read config file: %w", err)
		}
		for ln, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok || strings.TrimSpace(k) == "" {
				return Config{}, fmt.Errorf("config file %s:%d: want KEY=VALUE, got %q", path, ln+1, line)
			}
			vals[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	get := func(key string) string {
		if v := getenv(key); v != "" { // 环境变量优先
			return v
		}
		return vals[key]
	}

	cfg := Config{
		ClientID:           get("AGENT_CLIENT_ID"),
		RuntimeCallbackURL: get("AGENT_RUNTIME_CALLBACK_URL"),
		BaseURL:            get("AGENT_BASE_URL"),
		ADBPath:            get("AGENT_ADB_PATH"),
		ListenAddr:         get("AGENT_LISTEN_ADDR"),
		Version:            get("AGENT_VERSION"),
		RunsRoot:           get("AGENT_RUNS_ROOT"),
		DBPath:             get("AGENT_DB_PATH"),
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8480"
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	if cfg.RunsRoot == "" {
		cfg.RunsRoot = "./agent-runs"
	}
	if cfg.DBPath == "" {
		cfg.DBPath = "./agent.db"
	}
	hb := get("AGENT_HEARTBEAT_INTERVAL")
	if hb == "" {
		cfg.HeartbeatInterval = reporter.DefaultHeartbeatInterval
	} else {
		d, err := time.ParseDuration(hb)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("AGENT_HEARTBEAT_INTERVAL %q 不是合法 duration(如 10s)", hb)
		}
		cfg.HeartbeatInterval = d
	}
	aliases, err := parseSOCAliases(get("AGENT_SOC_ALIASES"))
	if err != nil {
		return Config{}, err
	}
	cfg.SOCAliases = aliases
	cfg.Capabilities = parseCSV(get("AGENT_DEVICE_CAPABILITIES"))

	var missing []string
	for _, req := range []struct {
		key, val string
	}{
		{"AGENT_CLIENT_ID", cfg.ClientID},
		{"AGENT_RUNTIME_CALLBACK_URL", cfg.RuntimeCallbackURL},
		{"AGENT_BASE_URL", cfg.BaseURL},
		{"AGENT_ADB_PATH", cfg.ADBPath},
	} {
		if req.val == "" {
			missing = append(missing, req.key)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required config: %s(环境变量或 -config 文件提供)",
			strings.Join(missing, ", "))
	}
	return cfg, nil
}

// program 实现 kardianos/service 的 Interface:Start 非阻塞启动,
// Stop 触发优雅停机并等待收敛。
type program struct {
	cfg Config

	cancel context.CancelFunc
	done   chan error
}

func (p *program) Start(service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan error, 1)
	go func() {
		err := runAgent(ctx, p.cfg)
		if err != nil {
			// kardianos 只在 Stop 时读 p.done 且不输出;致命错误
			// (DB 打不开/端口被占/监听失败)必须此刻可见。
			logf("fatal: %v", err)
		}
		p.done <- err
	}()
	return nil
}

func (p *program) Stop(service.Service) error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.done != nil {
		<-p.done
	}
	return nil
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s "+format+"\n",
		append([]any{time.Now().UTC().Format("15:04:05.000")}, args...)...)
}

// runAgent 装配并运行全部组件,阻塞至 ctx 取消后优雅停机。
func runAgent(ctx context.Context, cfg Config) error {
	if err := os.MkdirAll(cfg.RunsRoot, 0o755); err != nil {
		return fmt.Errorf("create runs root: %w", err)
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	client := &reporter.Client{BaseURL: cfg.RuntimeCallbackURL}
	events := &reporter.EventReporter{Store: st, Client: client, Logf: logf}
	results := &reporter.ResultReporter{Store: st, Client: client, Logf: logf}
	runner := &adb.ExecRunner{ADBPath: cfg.ADBPath}

	// 崩溃恢复(§4):上次进程的非终态任务无法复活(executor 不能接管
	// 已死进程的运行),统一置 FAILED,事件经 EventReporter 落盘+即发;
	// 随后补报未上报的终态结果。
	if err := abortInflight(ctx, st, events); err != nil {
		logf("recovery: abort inflight: %v", err)
	}
	if err := results.RecoverPending(ctx); err != nil {
		logf("recovery: report pending results: %v", err)
	}

	srv := server.New(server.Config{
		Store:        st,
		Runner:       runner,
		Events:       events,
		Results:      results,
		Uploader:     &uploader.Uploader{Logf: logf},
		RunsRoot:     cfg.RunsRoot,
		AgentVersion: cfg.Version,
		SOCAliases:   cfg.SOCAliases,
		Capabilities: cfg.Capabilities,
		Logf:         logf,
	})
	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Mux(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	hb := &reporter.Heartbeat{
		Runner: runner, Store: st, Client: client, Logf: logf,
		ClientID: cfg.ClientID, AgentVersion: cfg.Version, BaseURL: cfg.BaseURL,
		Interval: cfg.HeartbeatInterval, SOCAliases: cfg.SOCAliases, Capabilities: cfg.Capabilities,
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = hb.Run(ctx) }()
	// EventReporter.Run 首轮即抽干未上报事件,之后按周期补报
	go func() { defer wg.Done(); _ = events.Run(ctx) }()

	errCh := make(chan error, 1)
	go func() {
		logf("agent %s listening on %s (client_id=%s)", cfg.Version, cfg.ListenAddr, cfg.ClientID)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	}

	logf("shutting down")
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shCtx)
	wg.Wait()
	return nil
}

// abortInflight 把 store 中全部非终态任务迁移到 FAILED(agent 重启即
// 任务中止);并为缺 run-summary.json 的任务写合成摘要,使结果补报
// (RecoverPending)能组装出合法 result.json。
func abortInflight(ctx context.Context, st *store.Store, events *reporter.EventReporter) error {
	inf, err := st.LoadInflight(ctx)
	if err != nil {
		return err
	}
	for _, t := range inf.Tasks {
		events.OnTransition(t.TaskID, executor.Status(t.State), executor.StatusFailed,
			"agent restarted, task aborted")
		writeSyntheticSummary(t)
	}
	return nil
}

// writeSyntheticSummary 为被中止的任务补一份最小 run-summary.json
// (已存在则不覆盖)。退出码 -1 表示未能跑完。
func writeSyntheticSummary(t store.Task) {
	path := filepath.Join(t.OutDir, "run-summary.json")
	if _, err := os.Stat(path); err == nil {
		return
	}
	var d struct {
		DeviceSerial string `json:"device_serial"`
	}
	_ = json.Unmarshal([]byte(t.DispatchJSON), &d)
	sum := executor.Summary{
		Status:      executor.StatusFailed,
		ExitCode:    -1,
		Environment: map[string]string{"serial": d.DeviceSerial},
		OutDir:      t.OutDir,
	}
	data, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(t.OutDir, 0o755); err != nil {
		logf("recovery: mkdir %s: %v", t.OutDir, err)
		return
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		logf("recovery: write synthetic summary %s: %v", path, err)
	}
}
