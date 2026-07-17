# Agent dist README Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a tracked, standalone Windows operation and troubleshooting guide to `agent/dist` while continuing to ignore generated binaries and Smoke packages.

**Architecture:** Keep build/development instructions in `agent/README.md` and make `agent/dist/README.md` self-contained for operators receiving the distribution directory. Change Git ignore rules narrowly so only the README is tracked; all generated artifacts remain local.

**Tech Stack:** Markdown, PowerShell examples, Git ignore rules, pytest verification

---

### Task 1: Track only the dist README

**Files:**
- Modify: `.gitignore:1-4`

- [ ] **Step 1: Verify the current rule rejects the future README**

Run:

```bash
git check-ignore -v agent/dist/README.md
```

Expected: exit 0 with the current `agent/dist/` rule.

- [ ] **Step 2: Narrow the ignore rule**

Replace:

```gitignore
agent/dist/
```

with:

```gitignore
agent/dist/*
!agent/dist/README.md
```

- [ ] **Step 3: Verify generated artifacts remain ignored**

Run:

```bash
git check-ignore -v agent/dist/agent-cli agent/dist/agent-cli.exe \
  agent/dist/smoke-pkg-ok.tar.gz
```

Expected: all three paths are ignored by `agent/dist/*`.

- [ ] **Step 4: Commit the tracking rule**

```bash
git add .gitignore
git commit -m "Allow distribution usage guide"
```

Expected: one-line `.gitignore` replacement committed; no dist artifacts staged.

### Task 2: Add the standalone Windows operator guide

**Files:**
- Create: `agent/dist/README.md`

- [ ] **Step 1: Verify the guide does not exist**

Run:

```bash
test -f agent/dist/README.md
```

Expected: non-zero exit before creation.

- [ ] **Step 2: Create the complete guide**

Create `agent/dist/README.md` with exactly this content:

````markdown
# agent-cli 分发包使用说明

本目录用于在原生 Windows 客户端通过 USB/ADB 执行 Android 开发板测试。
首次使用请先完成“ADB 5137 准备”，再运行 Smoke Test。

## 目录内容

| 文件 | 用途 |
|---|---|
| `agent-cli.exe` | Windows x86-64 CLI，实机测试使用此文件 |
| `agent-cli` | Linux x86-64 CLI，仅用于 Linux 本地验证 |
| `smoke-pkg-ok.tar.gz` | 正常完成场景，CLI 预期退出码 0 |
| `smoke-pkg-fail.tar.gz` | 测试脚本主动失败，CLI 预期退出码 2 |
| `smoke-pkg-timeout.tar.gz` | 测试超时场景，CLI 预期退出码 3 |

## 前置条件

- 使用原生 Windows PowerShell，不要用 WSL 代替最终实机验证。
- 开发板已启用 USB debugging，并已确认调试授权。
- 已安装 Android Platform Tools，知道实际 `adb.exe` 的绝对路径。
- `adb devices -l` 能显示非空、非 `?` 的 USB transport serial。
- 同一时间只让 Agent 的 5137 ADB Server 占用设备。

下面示例按实际环境修改两个变量：

```powershell
$adb = "D:\platform-tools\adb.exe"
$serial = "513cd3de"
```

## 1. 准备私有 ADB Server（5137）

Agent 固定使用 5137，不使用系统默认的 5037。先停止 5037，再启动 5137：

```powershell
Remove-Item Env:ANDROID_ADB_SERVER_PORT -ErrorAction SilentlyContinue
& $adb kill-server

$env:ANDROID_ADB_SERVER_PORT = "5137"
& $adb start-server
& $adb devices -l
```

预期设备状态为 `device`。继续检查寻址和 ABI：

```powershell
& $adb -s $serial get-state
& $adb -s $serial shell getprop ro.product.cpu.abi
```

预期输出分别为 `device` 和 `arm64-v8a`。如果这里失败，不要继续运行 Agent。

## 2. 运行 Smoke Test

在本目录打开 PowerShell。每条 Agent 命令结束后立即读取 `$LASTEXITCODE`。

### 正常完成

```powershell
.\agent-cli.exe run `
  --package-file .\smoke-pkg-ok.tar.gz `
  --serial $serial `
  --adb $adb
$LASTEXITCODE
```

预期：终态 `COMPLETED`、`criteria_met=true`、PowerShell 退出码 `0`。

### 测试主动失败

```powershell
.\agent-cli.exe run `
  --package-file .\smoke-pkg-fail.tar.gz `
  --serial $serial `
  --adb $adb
$LASTEXITCODE
```

预期：终态 `COMPLETED`、设备脚本 `exit_code=7`、`criteria_met=false`、
PowerShell 退出码 `2`。测试失败是客观结果，不等于 Agent 执行故障。

### 测试超时

```powershell
.\agent-cli.exe run `
  --package-file .\smoke-pkg-timeout.tar.gz `
  --serial $serial `
  --adb $adb
$LASTEXITCODE
```

预期：约 5 秒后 kill 设备进程，终态 `TIMEOUT`，PowerShell 退出码 `3`。
超时后仍会进入 `COLLECTING` 并尽力拉取日志。

## 3. 检查结果

默认输出到当前目录下的 `agent-runs\<UTC时间戳>\`：

| 路径 | 检查内容 |
|---|---|
| `run-summary.json` | `status`、设备脚本 `exit_code`、`success_criteria_met`、设备环境 |
| `stdout.log` / `stderr.log` | 设备测试入口的标准输出与错误输出 |
| `logcat.txt` | 本次执行结束时导出的设备 logcat |
| `device/` | Manifest `collect` 规则成功拉回的文件 |
| `package/` | 已校验并解压的包，包含实际 `manifest.yaml` |

ok 场景应收集 `device/results/result.json` 和 `device/logs/run.log`，成功后清理
`/data/local/tmp/hermes-smoke`。fail 和 timeout 场景按 `keep_on_failure` 保留设备
workdir，便于继续排查。

## 使用 Registry 包

`--package-url` 模式必须同时提供整包 SHA-256。Token 使用环境变量，不要写入脚本
或提交到仓库：

```powershell
$env:AGENT_AUTH_TOKEN = "<临时下载令牌>"
.\agent-cli.exe run `
  --package-url "https://gitlab.example/api/v4/projects/1/packages/generic/name/1.2.3/package.tar.gz" `
  --sha256 "<64位十六进制SHA-256>" `
  --auth-type job_token `
  --serial $serial `
  --adb $adb
```

执行完成后清除当前 PowerShell 会话中的 Token：

```powershell
Remove-Item Env:AGENT_AUTH_TOKEN
```

## 常见问题

### `adb devices` 显示 `? device`

这表示 USB transport serial 无效，即使 `ro.serialno` 可能正常，也不能稳定用于
`adb -s <serial>`。先读取系统属性：

```powershell
& $adb -s "?" shell getprop ro.serialno
& $adb -s "?" shell getprop ro.boot.serialno
```

在已取得 root 权限且设备使用 ConfigFS 的情况下，可临时写入 USB gadget serial：

```powershell
& $adb -s "?" shell "echo 513cd3de > /config/usb_gadget/g1/strings/0x409/serialnumber"
```

重新枚举 USB 后再检查 `devices -l`。该修改通常会在重启或 USB gadget 重建后
丢失；永久修复应由设备 vendor init 配置写入 `${ro.boot.serialno}`。

### 5037 能看到设备，5137 看不到

Windows USB 接口可能已被 5037 Server 占用。重新执行“ADB 5137 准备”的完整步骤，
确认停止 5037 后再启动 5137。手工排障命令也必须保留：

```powershell
$env:ANDROID_ADB_SERVER_PORT = "5137"
```

### WSL 成功，原生 Windows 失败

WSL 中的 Linux `agent-cli` 通过 interop 启动 `adb.exe` 时，
`ANDROID_ADB_SERVER_PORT` 若未通过 `WSLENV` 传给 Win32，`adb.exe` 会静默使用
5037。因此 WSL 成功不能证明原生 Windows 的 5137 链路有效；最终验证必须运行
`agent-cli.exe`。

### `abi mismatch: device=, required=arm64-v8a`

空 ABI 通常不是设备 ABI 真的为空，而是旧版 CLI 忽略了 ADB 非零退出码。先确认
5137 下 `devices -l` 和 `getprop ro.product.cpu.abi` 正常。新版源码会直接报告 ADB
exit code 和 stderr；若分发包仍显示空 ABI，需要重新构建 `agent-cli.exe`。

### `collect "results/*": no match (exit=1)`

在 fail 和 timeout Smoke 包中没有生成 `results/result.json`，该日志符合预期，
Agent 仍会继续收集其他匹配项。业务包若要求该文件，则应结合
`run-summary.json`、测试退出码和 Manifest 成功判据判断，不能一律忽略。

### fail/timeout 后设备目录仍存在

Smoke Manifest 设置了 `keep_on_failure: true`，失败和超时后保留
`/data/local/tmp/hermes-smoke` 是预期行为。确认不再需要现场后再人工清理。

## CLI 退出码

| 退出码 | 含义 |
|---:|---|
| 0 | `COMPLETED` 且成功判据满足 |
| 1 | Agent 执行失败或参数错误 |
| 2 | `COMPLETED`，但成功判据不满足 |
| 3 | `TIMEOUT` |

注意终端显示的 `exit_code` 是设备测试脚本退出码；PowerShell 的
`$LASTEXITCODE` 是 Agent CLI 退出码，两者含义不同。
````

- [ ] **Step 3: Verify documentation facts against the CLI source**

Run:

```bash
rg -n 'package-file|package-url|sha256|auth-type|auth-token|serial|adb|out|keep-device-workdir' \
  agent/cmd/agent-cli/main.go
rg -n 'return 0|return 1|return 2|return 3' agent/cmd/agent-cli/main.go
rg -n '5137' agent/internal/adb/adb.go agent/dist/README.md
```

Expected: every documented flag, exit code, and private ADB port exists in source.

- [ ] **Step 4: Check the guide for credential leaks and placeholders**

Run:

```bash
rg -n 'glpat-|github_pat_|AKIA|PRIVATE KEY|tobias\.ma|maxin_1994@qq\.com' \
  agent/dist/README.md
```

Expected: no matches. The angle-bracket examples are intentional user-supplied values, not
unfinished documentation placeholders.

- [ ] **Step 5: Commit the operator guide**

```bash
git add agent/dist/README.md
git commit -m "Add agent distribution usage guide"
```

Expected: only `agent/dist/README.md` is committed.

### Task 3: Run repository verification and synchronize

**Files:**
- Verify: `.gitignore`
- Verify: `agent/dist/README.md`
- Test: `contracts/tests/`
- Test: `ci/tests/`

- [ ] **Step 1: Verify tracking boundaries**

Run:

```bash
git ls-files agent/dist
git check-ignore -v agent/dist/agent-cli agent/dist/agent-cli.exe agent/dist/smoke-pkg-*.tar.gz
```

Expected: `git ls-files` lists only `agent/dist/README.md`; every generated artifact is ignored.

- [ ] **Step 2: Run Python repository tests**

Run:

```bash
.venv/bin/python -m pytest -q contracts/tests ci/tests
```

Expected: 48 tests pass.

- [ ] **Step 3: Record Go verification availability**

Run:

```bash
if command -v go >/dev/null; then (cd agent && go test ./...); else echo GO_TOOLCHAIN_UNAVAILABLE; fi
```

Expected in the current environment: `GO_TOOLCHAIN_UNAVAILABLE`. Do not represent Python tests
as Agent compilation coverage.

- [ ] **Step 4: Push and verify remote synchronization**

Run:

```bash
git push origin master
git fetch origin master
test "$(git rev-parse HEAD)" = "$(git rev-parse origin/master)"
git status --short --branch
```

Expected: push succeeds, local and remote hashes match, and the tracked worktree is clean.

