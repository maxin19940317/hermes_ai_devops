# runtime — Temporal Worker + Trigger + REST API(Go)

当前内容:Phase 1.4 **Temporal spike**(完成,结论 GO)+ Phase 1.5 **Trigger 服务**。

## Trigger 服务(Phase 1.5)

`cmd/trigger`:GitLab pipeline webhook(Secret Token 验签,恒定时间比较)
→ Packages API 定位并拉取 `bundle-g{sha}-p{pipeline_global_id}.json`(GitLab 13.8
Pipeline Hook 的 `object_attributes.id` 是与 `CI_PIPELINE_ID` 相同的全局 pipeline ID;
webhook 不携带 Registry 版本号,按 package_name 倒序逐版本探测)
→ Schema 校验(内嵌 bundle.schema.json,防漂移测试)
→ 登记 artifacts 表(幂等 upsert)→ 按名启动 DeviceTestWorkflow。

去重语义:workflow ID = `device-test-{project}-g{commit}-p{iid}`,
复用策略 AllowDuplicateFailedOnly——webhook 重复投递不重跑,
上次失败的 bundle 可通过重新投递重触发。无 bundle 的成功 pipeline(如 MR 构建)
安静跳过(200)。配置见 `cmd/trigger/main.go` 头注释(环境变量)。

Postgres 集成测试由 `TEST_DATABASE_URL` 门控(本机跳过,服务器部署后必须跑通);
其余测试含真实 dev server 上的启动/去重 e2e(`internal/testtemporal` 拉起)。

## Spike 结论(2026-07-17)

三个最小示例以 e2e 测试形式落在 `spike/`,测试自行拉起
`temporal server start-dev`(单二进制 + SQLite,无需 Docker):

```bash
# 前置:temporal CLI(https://temporal.download/cli,本机装在 ~/.local/bin)
export PATH=$HOME/.local/bin:$HOME/.local/go/bin:$PATH
cd runtime && go test ./spike/ -v
```

| 场景 | 验证点 | 结果 |
|---|---|---|
| signal 接收 | workflow 阻塞于 `GetSignalChannel().Receive`;signal 先于等待点发送也被缓存不丢 | ✅ |
| Activity 重试 | `RetryPolicy{MaximumAttempts:5}`,前 2 次注入失败,第 3 次成功,真实执行恰 3 次 | ✅ |
| 杀进程重放恢复 | worker 独立进程被 SIGKILL 后 workflow 在 server 端保持 RUNNING;重启 worker 后从历史重放继续,已完成的 activity **不重复执行**(跨进程计数文件=1),signal 照常送达并完成 | ✅ |

对 DeviceTestWorkflow(Phase 1.6)的直接印证:

- `dispatch → await_result(signal)` 主干形态可行,禁止轮询的红线(§14)由
  signal 机制天然满足;
- 机械重试(§9 INFRA ≤2 次)可直接映射为 Activity RetryPolicy;
- "重启 Runtime 收敛到正确终态、零重复执行"(Phase 1 DoD 故障注入之一)
  由 Temporal 历史重放保证,无需自研恢复逻辑。

注意事项:

- Activity 代码必须幂等或副作用外置(重试会真实重跑 activity;重放不会);
- workflow 代码必须确定性(禁 I/O/时间/随机,一律经 activity 或 SideEffect);
- dev server 仅用于开发;生产走 §4 的自托管部署(Docker Compose,Postgres)。

## 目录

```text
spike/                  # go/no-go 三场景(workflow/activity + e2e 测试)
cmd/spike-worker/       # 独立 worker 进程,供 SIGKILL 场景使用
cmd/trigger/            # Trigger 服务(webhook → bundle → artifacts → workflow)
internal/trigger/       # handler / bundle 校验 / GitLab 客户端 / Temporal starter
internal/store/         # Postgres 访问层(schema.sql + 内存实现)
internal/workflow/      # DeviceTestWorkflow 输入契约(本体属 Phase 1.6)
internal/testtemporal/  # 测试用 dev server 拉起助手
```

后续(§12):1.6 DeviceTestWorkflow 主干 + 规则引擎。
