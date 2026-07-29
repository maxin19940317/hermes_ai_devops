# runtime — Temporal Worker + Trigger + REST API(Go)

当前内容：Phase 1.4 Temporal spike、Phase 1.5 Trigger、Phase 1.6
DeviceTestWorkflow/Worker 主干,可靠事件链路第一批(docs/device-test-sequence.md
差距清单 #1/#2:outbox 表 + Callback API 单事务写入 + 独立 Outbox Relay
进程(`cmd/relay`)+ workflow LoadResult 权威读),以及 Temporal History 与
重放安全第二批(#3/#7/#11/#15:心跳只续 DB 租约(所有权凭据条件续租,
失配 LEASE_NOT_OWNED)、workflow 改租约到期 Durable Timer + CheckLease、
plan.rule_version 版本路由、workflow ID RejectDuplicate + 显式 retry -r{N})。
q-uat 容器部署见 [`../deploy/README.md`](../deploy/README.md)。

## Trigger 服务(Phase 1.5)

`cmd/trigger`:GitLab pipeline webhook(Secret Token 验签,恒定时间比较)
→ Packages API 定位并拉取 `bundle-g{sha}-p{pipeline_global_id}.json`(GitLab 13.8
Pipeline Hook 的 `object_attributes.id` 是与 `CI_PIPELINE_ID` 相同的全局 pipeline ID;
webhook 不携带 Registry 版本号,按 package_name 倒序逐版本探测)
→ Schema 校验(内嵌 bundle.schema.json,防漂移测试)
→ 登记 artifacts 表(幂等 upsert)→ 按名启动 DeviceTestWorkflow。

去重语义:workflow ID = `device-test-{project}-g{commit}-p{iid}`(显式 retry
时加 `-r{N}`,N 为 artifacts.workflow_attempt 原子递增),复用策略
RejectDuplicate——webhook/kick 重复投递一律幂等不重启(含上次失败),
只有 `/kick` 载荷显式 `retry: true` 才派生新 ID 起新 run(差距 #11)。
bundle webhook 启动前还会跳过已测变体:kick 变体级 workflow 已出结论
(status=COMPLETED 且 verdict ∈ {PASSED, TEST_FAILED})的包从 Packages 中过滤,
全部有结论则不启动(200);INFRA_ERROR/TIMEOUT/无记录照常测,查询失败 fail-open 全量测。
无 bundle 的成功 pipeline(如 MR 构建)安静跳过(200)。
配置见 `cmd/trigger/main.go` 头注释(环境变量)。

Postgres 集成测试由 `TEST_DATABASE_URL` 门控(本机跳过,服务器部署后必须跑通);
其余测试含真实 dev server 上的启动/去重 e2e(`internal/testtemporal` 拉起)。

## Worker 服务(Phase 1.6 / Phase 2)

`cmd/worker`:Temporal worker + Client 回调 HTTP 服务,配置见
`cmd/worker/main.go` 头注释(环境变量全表)。飞书指令自然语言翻译新增两项
(设计文档 2026-07-28,详见 `../deploy/README.md` "飞书指令自然语言翻译"小节):

当前 Evidence v3 提取器会单遍流式扫描完整日志,保留真实全局行号、签名命中
±50 行上下文和有界兜底摘录,已无只读尾部 8MiB 的盲区;签名上下文与
兜底摘录共享 96KiB 内容预算,超过 1MiB 的单行会明确降级并标记截断。

| 变量 | 缺省 | 说明 |
|---|---|---|
| `FEISHU_CMD_NL` | `false` | 翻译旁路总开关(灰度)。真正启用还需 `HERMES_ENDPOINT` 非空且 `FEISHU_CMD_WHITELIST` 非空(指令 listener 已启用),三者合取 |
| `FEISHU_CMD_NL_TIMEOUT_SEC` | `60` | `/translate` 调用超时,不复用 `HERMES_TIMEOUT_SEC`(bridge 实测 `-t ""` 冷/热约 76s/13s,这是交互路径,需单独调) |
| `UPLOAD_REQUEST_MAX_FILES` | `64` | `POST /callbacks/v1/upload-requests` 单次请求文件数上限(差距 #8 按需签发,2026-07-29),超限整请求拒绝而非截断 |

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
cmd/worker/             # Temporal worker + Client 回调 HTTP 服务
cmd/relay/              # 独立 Outbox Relay(claim 未投递行 → Signal → 标记已投)
internal/trigger/       # handler / bundle 校验 / GitLab 客户端 / Temporal starter
internal/store/         # Postgres 访问层(schema.sql + 内存实现,含 outbox)
internal/workflow/      # DeviceTestWorkflow(signal 唤醒 + LoadResult 权威读)
internal/relay/         # Relay 投递循环(task-result;NotFound 视为已消费)
internal/testtemporal/  # 测试用 dev server 拉起助手
```

后续：Client Agent RPC/心跳接入、MinIO 预签名直传，以及生产 HTTPS/mTLS 硬化。
