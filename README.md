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
- Client:Windows,与服务器同局域网,USB 连接多块开发板(Android + QCS Ubuntu Linux 板,ADB 访问,私有 adb server 端口 5137)。
- 当前设备:QCM6125(Android)/ QCS6490(Linux)/ RK3576(Linux)等 3+ 块,支持多 Client 多设备。

## 仓库结构

```text
contracts/   契约优先:plan/manifest/result/bundle/command/escalation/evidence JSON Schema + 两个 OpenAPI,附正反例测试
ci/          业务仓库(algo-super-sdk)CI 脚本:gen_manifest / write_meta / gen_bundle / kick / variants.yaml(12 变体)
agent/       Windows Client Agent(Go):agent-cli 先行,后套 RPC 壳(服务模式 + CLI 模式)
runtime/     Temporal Worker + Trigger 服务 + REST API(Go,含 cmdapi 受控命令接口)
hermes/      hermes-agent 平台侧组件(全部为受控适配层,无 LLM 循环):
             analyze_bridge(Analyzer/Planner) / kanban_bridge(升级通道) / mcp_bridge(MCP 工具桥) /
             workflow_bridge(排行榜回填) / workflow_runtime(wf-devops-device-test 资产)
docs/        设计 spec、实施 plan、SDK 打包适配评估、设备测试链路时序图(device-test-sequence.md)
```

各组件细节见子目录 README:[ci/](ci/README.md)、[agent/](agent/README.md)、
[runtime/](runtime/README.md)、[deploy/](deploy/README.md)、[hermes/mcp_bridge/](hermes/mcp_bridge/README.md)、
[agent/dist/](agent/dist/README.md)(Windows 分发包使用说明)。

## 当前进度

### Phase 1 — 无 LLM 最小闭环(已达成)

| 步骤 | 状态 |
|---|---|
| 1. contracts 契约 + 校验测试 | ✅ |
| 2. ci 脚本 + 业务仓库 CI 改造 | ✅(12 变体 + kick 触发;SDK 适配门禁见 `docs/assessments/algo-super-sdk-packaging.md`) |
| 3. agent-cli(下载→校验→部署→执行→收集) | ✅ Windows 实机已验证 |
| 4. Temporal spike(signal/重试/杀进程重放) | ✅ 结论 GO |
| 5. Trigger 服务(webhook → bundle → artifacts → workflow) | ✅ q-uat 容器化部署 |
| 6. DeviceTestWorkflow 主干 + 规则引擎 | ✅ |
| 7. agent 套 RPC 壳 + 回调 + MinIO 直传 + 服务化 | ✅ |

**Phase 1 DoD 已达成**(2026-07-22,workflow `device-test-aios/algo_super_sdk-g108e0d72-p46`):
push → CI → webhook → 派单 → QCM6125 开发板实测 → SNPE 1.68 / SNPE 2.21 / TFLite 全部
PASSED(exit 0,真实推理耗时)→ MinIO 附件直传 → 飞书通知,全程无人工干预、零重复执行。

### Phase 2 — 证据-分析-通知增强(已完成)

| 步骤 | 状态 |
|---|---|
| Evidence Extractor 完整化(签名匹配 ±50 行 + junit 失败 + 指标差值 → evidence.json + 基线比较) | ✅ |
| Analyzer 完善(LLM 分析 evidence → 结构化结论 → decisions 落库,失败时规则引擎保底) | ✅ |
| 飞书交互卡片(展示卡片 2026-07-30;重试/忽略按钮 2026-08-03,经 WS listener 执行) | ✅ |
| Planner v1(自然语言 → Plan DSL,Schema 校验打回重试 ≤3) | ✅ |
| Hermes 接入(2026-07-21 决策:复用 q-uat hermes-agent 平台,不自研 Agent 循环) | ✅ |

### Phase 3 — 硬化(已完成)

| 步骤 | 状态 |
|---|---|
| mTLS 双向认证(Agent→Runtime 回调 18091 强制客户端证书) | ✅ |
| 全链路幂等键核验 + ≥10 场景故障注入矩阵 | ✅ |
| 审计完备(audit_log:dispatched/device_leased/device_released/escalated + card_retry/card_ignore) | ✅ |
| MinIO 生命周期(每日 UTC 3:00:runs/ PASSED 7 天、失败 90 天;evidence/ 快照不过期) | ✅ |
| Agent 版本上报 + 最低版本门禁 | ✅ |
| 设备能力表归 Runtime 统一管理(方案 B,服务端权威配置) | ✅ |

### Phase 4 — 扩展(进行中)

| 步骤 | 状态 |
|---|---|
| Grafana 看板(2026-08-04,13 面板,纯只读 postgres,自动 provisioning) | ✅ |
| 多设备并发调度 | 进行中 |
| 性能基线 MR 门禁 | 待做 |
| Linux 变体 SSH Adapter | 待做 |

完整阶段规划(Phase 0–4)与 DoD 见 CLAUDE.md §12。

## 开发与测试

```bash
# 契约测试(python3 >= 3.9,依赖见 contracts/tests/requirements.txt)
python -m pytest contracts/tests

# ci 脚本测试
python -m pytest ci/tests

# hermes bridges(pytest,假 Runtime/CLI 驱动)
.venv/bin/python -m pytest hermes/mcp_bridge hermes/analyze_bridge hermes/kanban_bridge -q

# agent(Go 1.22+,交叉编译见 agent/README.md)
cd agent && go test ./...

# runtime(部分测试需 temporal CLI;Postgres 集成测试由 TEST_DATABASE_URL 门控)
cd runtime && go test ./...
```

## 工程约定(摘要)

- Go 1.22+;wrapped errors;跨网络调用带 context 超时。
- 含状态迁移的模块必须有表驱动状态机单测;恢复路径必须有故障注入测试。
- 契约只加字段不删字段,`*_version` 递增;消费 Plan/Manifest/result.json 前必过 Schema 校验。
- 提交信息用英文;秘钥不落 Git;时间一律 UTC 存储。
