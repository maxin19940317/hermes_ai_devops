# Hermes AI DevOps — 开发板自动测试系统

由 LLM Agent(Hermes)驱动的自动编译、部署、开发板测试、分析与通知系统:
用户只描述目标,Hermes 负责理解/规划/分析/反馈;底层由确定性 Runtime 可靠完成
编译、部署、测试、恢复,不因 LLM 上下文、服务重启或网络抖动失去执行一致性。

> **权威上下文是 [CLAUDE.md](CLAUDE.md)**:架构决策已定稿,按此实现,不要重新发明。
> 本 README 只是入口与导航。

## 三层架构

```text
语义层  Hermes          决定"做什么、为什么、下一步"   (LLM,不在执行关键路径)
执行层  Workflow Runtime 保证"可靠执行、状态不丢、不重复" (确定性,Temporal + Go)
设备层  Client Agent     在 Windows/开发板上"真正干活"   (确定性,Go,无 LLM)
```

硬性边界(详见 CLAUDE.md §3 / §14):

- Hermes 与 Client Agent 之间禁止任何直接通信,一切经 Runtime 中转。
- Hermes 对 Runtime 的输入必须是 JSON Schema 约束的结构化数据(Plan DSL),拒绝自由文本。
- 设备上执行什么由构建包内 Manifest 在打包期声明;Client 不提供任意 Shell 接口,ADB 操作走模板化白名单。
- Hermes 不可用时,已开始的确定性任务必须能继续完成。
- status(生命周期)与 verdict(终态判定)正交;verdict 优先由确定性规则引擎判定,LLM 只补充解释。

## 物理环境

- 服务器:Linux,运行 GitLab 13.8 / Runner / Package Registry 及全部服务端组件。
- Client:Windows,与服务器同局域网,USB 连接 Android 开发板(ADB 访问,私有 adb server 端口 5137)。

## 仓库结构

```text
contracts/   契约优先:plan/manifest/result/bundle JSON Schema + 两个 OpenAPI,附正反例测试
ci/          业务仓库(algo-super-sdk)CI 脚本:gen_manifest / write_meta / gen_bundle / variants.yaml
agent/       Windows Client Agent(Go):agent-cli 先行,后套 RPC 壳
runtime/     Temporal Worker + Trigger 服务 + REST API(Go)
docs/        设计 spec、实施 plan、SDK 打包适配评估
```

各组件细节见子目录 README:[ci/](ci/README.md)、[agent/](agent/README.md)、
[runtime/](runtime/README.md)、[agent/dist/](agent/dist/README.md)(Windows 分发包使用说明)。

## 当前进度(Phase 1 — 无 LLM 最小闭环)

| 步骤 | 状态 |
|---|---|
| 1. contracts 契约 + 校验测试 | ✅ |
| 2. ci 四脚本 + 业务仓库 CI 改造 | ✅(SDK 适配门禁见 `docs/assessments/algo-super-sdk-packaging.md`) |
| 3. agent-cli(下载→校验→部署→执行→收集) | ✅ Windows 实机已验证 |
| 4. Temporal spike(signal/重试/杀进程重放) | ✅ 结论 GO |
| 5. Trigger 服务(webhook → bundle → artifacts → workflow) | ✅ |
| 6. DeviceTestWorkflow 主干 + 规则引擎 | 🚧 进行中 |
| 7. agent 套 RPC 壳 + 回调 + MinIO 直传 + 服务化 | 待做 |

完整阶段规划(Phase 0–4)与 DoD 见 CLAUDE.md §12。

## 开发与测试

```bash
# 契约测试(python3 >= 3.9,依赖见 contracts/tests/requirements.txt)
python -m pytest contracts/tests

# ci 脚本测试
python -m pytest ci/tests

# agent(Go 1.22+)
cd agent && go test ./...

# runtime(部分测试需 temporal CLI;Postgres 集成测试由 TEST_DATABASE_URL 门控)
cd runtime && go test ./...
```

## 工程约定(摘要)

- Go 1.22+;wrapped errors;跨网络调用带 context 超时。
- 含状态迁移的模块必须有表驱动状态机单测;恢复路径必须有故障注入测试。
- 契约只加字段不删字段,`*_version` 递增;消费 Plan/Manifest/result.json 前必过 Schema 校验。
- 提交信息用英文;秘钥不落 Git;时间一律 UTC 存储。
