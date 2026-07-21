# agent — Windows Client Agent (Go)

Phase 1.3:**`agent-cli` 先行**(CLAUDE.md §12)。不做 RPC Server,先用 CLI 在
Windows+USB+ADB 环境把所有坑踩完;`internal/executor` 与后续服务壳共用。

## 构建与测试

```bash
export PATH=$HOME/.local/go/bin:$PATH   # Go 1.26 (本机安装于 ~/.local/go)
cd agent
go test ./...
go build ./cmd/agent-cli                          # Linux
GOOS=windows GOARCH=amd64 go build ./cmd/agent-cli  # Windows 单二进制
```

## 使用

```powershell
agent-cli run `
  --package-url "https://gitlab.../packages/generic/algo-super-sdk/1.2.3/algo-super-sdk-aarch64_Android_SNPE_2.21-gxxxxxxx-p42.tar.gz" `
  --sha256 <bundle 中的整包 sha256> `
  --auth-type job_token --auth-token $env:AGENT_AUTH_TOKEN `
  --serial R5CT10XXXXX `
  --adb C:\agent\platform-tools\adb.exe
# 本地包调试: --package-file pkg.tar.gz(--sha256 可选)
```

退出码:`0` COMPLETED 且成功判据满足;`2` COMPLETED 判据不满足;`3` TIMEOUT;`1` FAILED。

产出目录(`--out`,默认 `agent-runs/<UTC时间戳>/`):

```
run-summary.json   # status/exit_code/duration/criteria/environment(不含 verdict,verdict 归 Runtime)
device/...         # 按 manifest collect 拉回的产物(含 results/result.json)
logcat.txt / stdout.log / stderr.log
package/           # 解压后的包(含 manifest.yaml)
```

## 实机验证 smoke 包(smoke/)

不依赖 GitLab 的最小测试包,用于在 Windows+USB 实机上验证 agent-cli 闭环:

```bash
./smoke/build.sh            # ok 变体:正常通过,期望退出码 0
./smoke/build.sh timeout    # timeout_sec=5 + 长睡眠,期望退出码 3 且仍收集 logcat
./smoke/build.sh fail       # exit 7 + 打印 SMOKE-FAIL 签名,期望退出码 2 且 keep_on_failure
```

产出 `dist/smoke-pkg-<variant>.tar.gz` 并打印整包 sha256;构建后自动跑
`go run ./smoke/check <pkg>` 做无设备校验(解压 + Manifest Schema + 逐文件 sha256,
与 agent-cli PREPARING 阶段同一代码路径)。拷到 Windows 后:

```powershell
agent-cli run --package-file smoke-pkg-ok.tar.gz --sha256 <打印的sha256> `
  --serial <序列号> --adb C:\agent\platform-tools\adb.exe
```

核对点:退出码;`device/results/result.json` 与 `logs/run.log` 被收集;
`logcat.txt` 含 `hermes-smoke` 标记;stdout 中 `SMOKE_WORKDIR` 已被替换为实际 workdir;
成功后设备上 `/data/local/tmp/hermes-smoke` 已清理(fail/timeout 变体应保留现场)。

## 架构要点(对照 CLAUDE.md 红线)

- `internal/adb`:**模板化白名单**命令构造器是唯一命令来源,全部强制 `adb -s <serial>`;
  `ExecRunner` 对每次调用注入 `ANDROID_ADB_SERVER_PORT=5137`(覆盖继承值),永不碰 5037。
- `internal/manifest`:嵌入 `contracts/manifest.schema.json` 副本,加载必过 Schema;
  测试 `TestEmbeddedSchemaMatchesContract` 防契约漂移(改契约后 `cp` 同步再跑测试)。
- `internal/artifact`:下载原子写 + 整包 sha256 校验;解压拒绝绝对路径/`..`/符号链接。
- `internal/executor`:流水线 PREPARING→(DOWNLOADING)→DEPLOYING→RUNNING→COLLECTING→终态;
  超时 kill 后**仍收集**;非零退出码/超时是客观结局不是 error;
  逐文件 sha256 复核后才 push;status 与 verdict 正交,本层不判 verdict。

## 实机踩坑记录(2026-07-17,trinket/QCM6125 板)

- **USB 传输层 serial 可能为 `?`**:板子的 USB gadget 未设置 iSerial 描述符,
  `adb devices` 显示 `?`,`-s` 无法寻址(`ro.serialno` 是系统属性,与此无关)。
  修复:`adb root` 后
  `echo 513cd3de > /config/usb_gadget/g1/strings/0x409/serialnumber`,拔插 USB 生效。
  **重启后丢失**,长期需 init 脚本持久化。启示:设备注册不能假设 USB serial 总是可用。
- **WSL 下跑 Linux 版 agent-cli + adb.exe 时,`ANDROID_ADB_SERVER_PORT` 不会传给
  Windows 进程**(WSL interop 需 WSLENV 显式声明),即私有 5137 端口静默失效,
  实际连的是 5037 —— 违反 §14 红线。实机验证必须用原生 Windows 的 agent-cli.exe。
- precheck 的 getprop 曾忽略 adb 退出码,设备不可寻址时误报为
  `abi mismatch: device=`(已修:ExitCode != 0 时带 stderr 报错,
  回归测试 `TestPrecheckSurfacesADBErrorWhenDeviceUnaddressable`)。

## 服务模式(cmd/agent,Phase 1.7)

`agent` 在 executor 上套 RPC 壳(设计 §3.5/§3.6,契约
`contracts/client-agent-api.openapi.yaml`):HTTP Server 接收 Runtime 派单并异步执行,
心跳/事件/结果经 callbacks-api 回流,附件按预签名 URL 直传 MinIO,
SQLite(`AGENT_DB_PATH`)支撑幂等与崩溃恢复补报。

```bash
go build ./cmd/agent
agent run -config agent.conf     # 前台(默认子命令);Ctrl-C / SIGTERM 优雅停机
agent install|uninstall|start|stop   # Windows Service / systemd(kardianos/service)
```

配置:环境变量 + 可选 `-config` 文件(`KEY=VALUE` 每行一条,`#` 注释;
环境变量优先)。必填 `AGENT_CLIENT_ID`、`AGENT_RUNTIME_CALLBACK_URL`、
`AGENT_BASE_URL`、`AGENT_ADB_PATH`;可选 `AGENT_LISTEN_ADDR`(默认 `:8480`)、
`AGENT_VERSION`(默认 `dev`)、`AGENT_RUNS_ROOT`(默认 `./agent-runs`)、
`AGENT_DB_PATH`(默认 `./agent.db`)、`AGENT_HEARTBEAT_INTERVAL`(默认 `10s`)。

启动恢复(§4):上次进程的非终态任务统一置 FAILED(事件+合成摘要结果回流),
随后补报未上报的终态结果与事件。

## 尚未覆盖(后续阶段)

- ~~RPC 服务壳(§8.1)、心跳/事件/结果回调、MinIO 直传、Windows Service 化~~ → 已交付(cmd/agent,Phase 1.7)
- Agent 自带固定版本 adb 并自管 server 生命周期(当前用 `--adb` / `AGENT_ADB_PATH` 指定)
- 真实设备验证需在 Windows Client 上进行(本仓库单测用 fake Runner 全覆盖流水线)
