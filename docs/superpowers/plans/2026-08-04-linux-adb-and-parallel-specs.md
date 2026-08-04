# Phase 4：Linux 变体接入（adb-linux 路径）+ 多设备并发调度

## 目标

1. 4 个 Linux 变体（`aarch64_Linux_*`）经 **adb 通道**（不做 SSH）进入设备测试链路，rk3568/rk3588 板可用
2. `DeviceTestWorkflow` 多变体并发执行，多块板子同时跑

## 现状关键事实（已读码确认）

- Agent 执行器（`agent/internal/executor/executor.go`）的设备交互绝大多数是通用 shell：`mkdir/rm/chmod/pkill/ls`、adb push/pull 在 Linux adbd 上原生可用。**只有 4 个 Android 专属点**：
  - `precheck`：`getprop ro.product.cpu.abi` 等（Linux 无 getprop）
  - `adb.DiskFreeKB` 写死 `/system/bin/df`（Linux 用 PATH 里的 `df`）
  - `LogcatClear/LogcatDump`（Linux 无 logcat，跳过即可）
  - workdir：已由 gen_manifest 按 `requirements.os` 选好（`workdir_linux=/opt/algo-super-sdk`），无需改
- `manifest.Requirements.OS` 字段已存在（manifest.go:57），Agent 执行时能拿到目标 OS
- **调度安全缺口**：`variants.yaml` 里 Linux 变体**没有 soc 约束**，而 `lockOneCandidate`（postgres_devices.go:150）对空 SOC 是通配——现在放开 `os != android` 的跳过逻辑，Linux 变体会抢到 Android 的 QCM6125 板。所以 **OS 维度必须先进设备表和 selector**，否则出错单
- `DeviceSelector`（devicetest.go:53）当前只有 `SOC`/`Capabilities`；`devices` 表无 `os` 列；probe 上报的 props 无 os 字段
- 并发障碍：`awaitResult`（devicetest.go:788）在共享 signal channel 上按 task_id 过滤，但 `selector.AddReceive` 收到不匹配的信号会**消费掉它**——两个 goroutine 直接共享 channel 会互相吃掉对方的结果信号，必须先做分发器
- workflow 结构变更（串行 loop → 并发）**必须 `workflow.GetVersion` 门**（与 notify-card 同一模式，devicetest.go:361）

## 任务分解

### 轮次 A：OS 维度贯通（调度安全前置）

1. **contracts**：`client-agent-api.openapi.yaml` 设备 props 加 `os` 字段（只加不删）；无 schema 破坏性变更
2. **agent probe**（`reporter/probe.go`）：Android 路径设 `os=android`；linuxSOC 路径设 `os=linux`，并用 `uname -m` 补 `abi`（新增白名单命令 `adb.UnameM`）；capabilities 复用 `DeviceCapabilities` 配置（rk3568 配 `rknpu`）
3. **store**：`devices` 表加 `os TEXT NOT NULL DEFAULT 'android'`——新列必须写迁移脚本 `deploy/postgres/migrations/2026-08-04-devices-os.sql`（schema.sql 不管已有表的列）；MemStore 同步；`DeviceSelector` 加 `OS string`（空=不约束）；`lockOneCandidate`/`HasCapableDevice`/`matchSelector` 加 os 匹配
4. **心跳链路**：`UpsertClientDevices` 落 os 列；conformance 补用例

### 轮次 B：Agent adb-linux 执行路径

5. `adb.go` 白名单新增：`UnameM`（`shell uname -m`）、`DiskFreeKBLinux`（`shell df -k`，不带 /system/bin 前缀）
6. `executor.precheck`：`m.Requirements.OS == "linux"` 分支——abi 用 `uname -m`（`aarch64`→`arm64-v8a` 映射），跳过 android release/soc getprop（soc 已由 selector 保证）；df 走 Linux 命令
7. `executor.run`：linux 时跳过 LogcatClear/Dump（`dumpLogcat` 同理）
8. dispatch payload 无需新增字段（manifest 内已有 os）；Agent 旧版收到 linux 派单会因 precheck 失败报 INFRA——**Agent 必须先于变体放开部署**
9. executor 单测：fakeRunner 模拟 Linux 板（getprop 失败、uname/df/push/pull 正常），全流水线走通

### 轮次 C：Runtime 放开 Linux 变体

10. `specs.go SelectTestSpecs`：`os=linux` 不再无条件 Skipped——selector 带 `OS: "linux"`（SOC/Capabilities 从 variants.yaml 读），无匹配设备仍秒级跳过（复用 HasCapableDevice）
11. `variants.yaml`：Linux 变体补 soc 约束（RKNN: `[RK3588, RK3568]`、SNPE/TFLite 按实际板子定），SNPE Linux 需要 hexagon——rk 板没有，这些变体会保持跳过直到有对应板子
12. 规则引擎/evidence：Linux 签名走 `signatures_common_linux`（variants.yaml 已有，gen_manifest 已合并），确认 ExtractEvidence 的 where=stderr 路径工作

### 轮次 D：workflow 并发（带版本门）

13. **signal 分发器**：单 goroutine 消费 `resultCh`，按 task_id fan-out 到每个 spec 的 buffered channel（`workflow.NewBufferedChannel`）；`runTest` 改收私有 channel
14. `workflow.GetVersion(ctx, "parallel-specs", DefaultVersion, 1)`：旧分支保持串行 loop；新分支 `workflow.Go` 每 spec 一个 goroutine，`workflow.NewSelector` 或 channel 按 **spec 顺序**收齐结果填 `out.Tasks`（输出顺序确定性，通知卡片逻辑零改动）
15. 并发语义测试：两个 spec 并发完成、一个失败一个成功、INFRA 重试交错；replay 测试用既有 history fixture（`testdata/history-pre-notify-card.json` 不动）

### 轮次 E：端到端验证（实机）

16. rk3568 板跑 `aarch64_Linux_TFLite_2.21.0` 单变体 workflow（temporal CLI 手动触发，同 r10 验证方式）
17. Android + Linux 变体混合 bundle 触发，验证两板并行 + 卡片聚合正确

## 部署门禁（顺序不能乱）

1. 迁移脚本先跑（devices 加 os 列，非停写窗口型——纯加列，旧代码读不到该列不影响）
2. Agent 先行部署（否则 Runtime 放开 linux 后旧 Agent 收单即 INFRA）
3. Runtime 部署（含 GetVersion 门，进行中 workflow 安全）
4. 每轮独立提交；A/B 可合并一轮，C/D 分开

## 明确不做

- 不做 SSH（用户已决策：设备均 adb 连接）
- 不动 `evidence/` 快照保留策略
- 不做 MR 门禁（等业务包指标埋点）
- rk3568 板的 USB serial 修复（ConfigFS）不是本轮前置——设备树序列号回退已够用

## 验收标准

- `plan`/webhook 触发含 Linux 变体的 bundle 后：Linux 变体在 rk 板上真实执行并出 verdict；Android 变体不受影响
- 双板并行时总耗时 ≈ max(单板耗时) 而非 sum
- 全部既有测试 + conformance 绿；新故障注入用例覆盖"并发中一台板掉线"
