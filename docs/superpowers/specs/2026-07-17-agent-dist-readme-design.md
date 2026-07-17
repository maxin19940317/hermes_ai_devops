# Agent dist 使用说明设计

日期：2026-07-17

## 目标与受众

为拿到 `agent/dist` 分发目录后，需要直接在原生 Windows 上连接 Android
开发板并运行 Smoke Test 的使用者提供一份独立操作手册。读者无需阅读 Agent
源码或仓库根文档，即可完成环境检查、三种 Smoke 场景、结果核对和常见故障排查。

本文档不承担 Agent 构建、Smoke 包生成、CI 发布或服务化部署说明；这些开发者内容
继续保留在 `agent/README.md`。

## 交付内容

新增 `agent/dist/README.md`，采用“快速上手 + 排障手册”结构，内容按实际操作顺序
排列：

1. 说明分发目录中的 Windows CLI、Linux CLI 和三个 Smoke 包各自用途。
2. 列出 Windows、USB 调试、Platform Tools 和有效 USB transport serial 等前置条件。
3. 使用 PowerShell 停止默认 5037 ADB Server，在 Agent 固定使用的 5137 端口启动
   Server，并验证设备状态和 ABI。
4. 分别给出 ok、fail、timeout 三种 Smoke Test 命令，要求在每次命令后检查
   `$LASTEXITCODE`，预期依次为 0、2、3。
5. 解释 `run-summary.json`、`stdout.log`、`stderr.log`、`logcat.txt` 和 `device/`
   的用途及关键核对点。
6. 集中说明实机已经遇到的故障和判断方法。
7. 说明凭据处理、ADB Server 生命周期和原生 Windows 验证边界。

## 命令与行为约束

- 所有可复制的客户端命令使用 PowerShell 语法。
- 示例通过变量保存 ADB 绝对路径，避免 PATH 中存在多个 `adb.exe`。
- 在启动 5137 前先停止 5037，避免两个 ADB Server 争用 Windows USB 接口。
- Agent 命令始终传入 `--serial` 和 `--adb`，本地包使用 `--package-file`。
- Registry 下载示例只说明使用环境变量传入 Token，不写入任何真实凭据。
- WSL 只用于解释已知差异，不作为推荐实机运行方式；推荐原生 Windows
  `agent-cli.exe`。
- `collect "results/*": no match` 在 fail 和 timeout Smoke 包中按预期解释，不能
  泛化为所有业务包都可忽略。
- fail 和 timeout 场景因 `keep_on_failure` 保留设备 workdir，ok 场景应清理。

## 故障排查范围

README 覆盖以下已确认问题：

- `adb devices` 显示 `? device`：区分 USB transport serial 与 `ro.serialno`，说明
  ConfigFS 临时修复重启后可能丢失，长期应由设备 init 配置持久化。
- WSL 测试成功、原生 Windows 失败：说明 WSLENV 造成 5137 环境变量未进入
  Win32 `adb.exe`，WSL 测试可能实际使用 5037。
- `abi mismatch: device=`：说明旧二进制会掩盖 ADB 寻址失败；先检查 5137 下设备
  是否可见，新源码/新构建会输出 ADB exit code 和 stderr。
- 5037 可见、5137 不可见：给出停止 5037 后重启 5137 的完整命令。
- 结果文件未匹配与设备 workdir 保留：结合 Smoke 变体解释预期行为。

## 仓库跟踪策略

当前 `.gitignore` 忽略整个 `agent/dist/`。调整为忽略 `agent/dist/*`，再通过
`!agent/dist/README.md` 仅放行使用说明。二进制、Smoke 包和其他构建产物继续保持
未跟踪状态。

## 验证标准

- `git check-ignore` 证明 README 未被忽略，二进制和 Smoke 包仍被忽略。
- README 中的参数名称与 `agent/cmd/agent-cli/main.go` 一致。
- 三种 Smoke 场景的预期 CLI 退出码分别为 0、2、3，并与当前实现一致。
- 文档不包含真实 Token、私有 Registry 地址或其他凭据。
- 现有 Python 契约与 CI 测试继续通过。
- 若当前环境仍无 Go 工具链，明确记录 Go 测试未执行，不以 Python 测试替代
  Agent 编译验证。

