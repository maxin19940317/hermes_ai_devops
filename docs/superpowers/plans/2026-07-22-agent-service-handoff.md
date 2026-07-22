# Client Agent 服务化交接（Phase 1.7 收官）

日期：2026-07-22

## 状态

**Phase 1 全部完成，DoD 达成。** 分支 `feature/agent-service`（26 提交）已随
[PR #4](https://github.com/maxin19940317/hermes_ai_devops/pull/4) 合入 master
（merge `181f836`），分支已删除。

## DoD 证据（2026-07-22, workflow `device-test-aios/algo_super_sdk-g108e0d72-p46`）

push(merge MR !2) → CI 8 变体 → bundle → **webhook 自动触发** → 派单到
windows-client-01 → QCM6125(513cd3de) 实测：

| 变体 | verdict | exit | 时长 | 附件 |
|---|---|---|---|---|
| SNPE 1.68 | PASSED | 0 | 6.9s | 4 |
| SNPE 2.21 | PASSED | 0 | 5.7s | 4 |
| TFLite 2.21.0 | PASSED | 0 | 9.2s | 4 |

MinIO `hermes-evidence` 中 12 个附件对象齐（result.json/junit 缺失项按降级语义跳过/
logcat/stdout/stderr），飞书通知已送达。事件序列经 task_events 核验零重复执行。

## 验收中发现并修复的 11 个真实问题（全部有提交与回归测试）

1. SoC 调度不匹配：固件报平台代号 trinket，约束用型号 QCM6125 → `AGENT_SOC_ALIASES` 别名（executor 预检共用同一映射）
2. 设备能力未声明（hexagon）→ `AGENT_DEVICE_CAPABILITIES` 显式声明
3. task_id 含项目路径 `/` 被误拒（400 烧光 INFRA 重试 → 设备被隔离）→ ID 原样接受，仅净化目录名
4. `job_token` 头发 PAT 下载 401 → 默认 `bearer`（13.8 验证接受 PAT）
5. executor 预检未应用别名（派单成功但预检失败）
6. 预签名代码不在运行镜像（`up -d` 只换 env 不重建）→ 升级路径必须先 `build` 再 `up`
7. 0 字节附件 chunked 上传被 S3 拒（411）→ 空文件显式 `Content-Length: 0`
8. 启动脚本从调用者目录解析 exe，静默跑了旧二进制 → `$PSScriptRoot`
9. Windows PS5.1 按 GBK 解析 UTF-8 脚本、stderr 变 NativeCommandError → 脚本纯 ASCII + ToString 扁平化
10. Docker 自动分配网段 172.22 撞内网真实设备 → `RUNTIME_SUBNET` 显式固定 172.31.240.0/24
11. SDK 测试入口 run.sh 非 POSIX（`#!/usr/bin/env bash` + `[[ =~ ]]`）→ SDK 仓库改 POSIX（业务侧修复，MR !2）

独立审查另修 4 项：预签名 URL 经 `url.Error` 泄露进日志、事件即发越过积压致
Runtime 状态回退、cmd/agent 致命错误静默、COLLECTING 期间取消被丢弃。

## 运维要点（接手必读）

- q-uat 栈：`deploy/README.md`（configure/start/verify/upgrade/rollback）。
  **升级必须 `build` 后 `up`**（见问题 6）；`down` 不带 `-v`（卷含全部状态）。
- Windows agent：`agent/dist/start-agent.ps1` 一键启动；`collect-diagnostics.ps1` 收集现场。
- 秘密只存 `deploy/.env`；GitLab PAT 建议轮换一次（早期调试泄露过一次）。
- 设备被 QUARANTINED：先查 fail_streak 来源（tasks 表 INFRA 记录），人工恢复
  `UPDATE devices SET status='IDLE', fail_streak=0`。
- 快速复测（不重新构建）：用历史 bundle 输入直接在 Temporal 起新 workflow
  （tctl workflow start,换 workflow_id；本次验收 rerun1-6 均用此法）。

## 后续阶段

- **Phase 2 — Hermes 接入**：复用 q-uat 现有 hermes-agent 平台（2026-07-21 决策变更，
  见 CLAUDE.md §4）。先做 Evidence Extractor 完整化与 Analyzer；glob 附件全量上传
  需"按需申请预签名"端点（CONTRACT-ISSUE，契约只加字段）。
- **Phase 3 — 硬化**：mTLS（回调与 MinIO 目前是测试网段明文）、clients 离线判定、
  min_agent_version 门禁、MinIO 预签专用账户（现为 root 凭据）、飞书签名校验。
- 已知小尾巴：metrics 基线（PERF verdict 未启用）、Linux 变体 SSH Adapter（Phase 4）。
