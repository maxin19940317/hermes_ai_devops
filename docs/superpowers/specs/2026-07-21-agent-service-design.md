# Client Agent 服务化设计（Phase 1.7）

日期：2026-07-21

状态：已批准（实施计划见 `../plans/2026-07-21-agent-service.md`）

## 1. 背景与决策

agent-cli 已在 Windows 实机验证完整执行流水线（下载→校验→部署→执行→收集）。
Phase 1 闭环的最后缺口是：Runtime 无法向 Client 派单，Client 执行结果无法回流。
本设计把 agent-cli 的执行能力套入 §8.1 RPC 壳，补齐 §8.2 回调与 MinIO 直传，
使 Runtime 的 DeviceTestWorkflow 能完成 `acquire_device → dispatch → await_result`
全链路。

关键决策（已与负责人确认，2026-07-21）：

1. **MinIO 本轮交付**。compose 加 MinIO；dispatch 时按固定键集预签名
   （`runs/{task_id}/{result.json,junit.xml,logcat.txt,stdout.log,stderr.log}`），
   Agent 收集后直传，附件不经 Runtime（红线 §14）。
   CONTRACT-ISSUE：manifest collect 的 glob（如 `dumps/**`）在派发期无法预知文件名，
   不在预签集内的命中文件本轮不上传（本地保留并记日志）。后续可加
   "按需申请预签名"回调端点（契约只加字段不删字段）。
2. **UAT 期间 LAN 暴露**。Worker callbacks（18091）与 MinIO（9000）绑 q-uat LAN；
   Windows Client 与服务器同网段。`deploy/README.md` 标注：测试网段专用，
   Phase 3 mTLS 落地前不得暴露到更大范围。`CALLBACK_BASE_URL` 随之改为
   `http://<q-uat-LAN-IP>:18091`——compose 部署审查时标记的 localhost 脚枪就此解除。
3. **SQLite 用 `modernc.org/sqlite`**（纯 Go，Linux 交叉编译 Windows 免 CGO）。
4. **Windows Service 用 `kardianos/service`**（CLAUDE.md §4 已定）。
5. Agent 容忍 HeartbeatAck 仅含 `ok`（`min_agent_version`/`cancel_task_ids`
   属 Phase 3 版本门禁，本轮不实现）。

不采用的方案：

- 先通闭环、MinIO 后置：Phase 1 DoD 要求通知含日志链接，附件必须可寻址；
- 按需申请预签名端点：更灵活但改契约加端点，本轮固定键集已覆盖 DoD 所需证据；
- mattn/go-sqlite3：CGO 依赖使 Windows 交叉编译复杂化。

## 2. 范围

交付：

- agent 新包：`internal/{store,reporter,uploader,server}`，`cmd/agent`（服务模式）；
- executor 增加 `CANCELED` 状态与外部取消；adb 增加 `Devices`/`LogcatTail`；
- runtime：compose MinIO 服务、dispatch 预签名、callbacks/MinIO LAN 绑定；
- contracts：仅当验收证实 GitLab 13.8 不接受 bearer 形式的 PAT 时，
  artifact auth enum 增加 `private_token`（只加不删）；
- q-uat + Windows 端到端验收与故障注入。

不交付：mTLS、clients 离线判定、min_agent_version 门禁（Phase 3）；
Agent 内置固定版本 adb（仍 `--adb` 指定）；glob 附件全量上传；
metrics 基线与飞书交互卡片（Phase 2）。

## 3. 组件设计

### 3.1 executor 取消（agent/internal/executor）

- `Status` 增加 `CANCELED`（与 §9 status 对齐）。
- `Cancel()`：幂等；置取消标志后，若处于 RUNNING，用
  `context.WithoutCancel` + 超时执行设备端 `ShellPkill`（复用超时 kill 路径），
  流水线继续走 COLLECTING → cleanup（尊重 `keep_on_failure`）→ 终态 CANCELED。
- 已终态后调用 Cancel 无副作用。

### 3.2 store（agent/internal/store，SQLite WAL）

```text
tasks(task_id PK, idempotency_key UNIQUE, state, attempt,
      dispatch_json, out_dir, started_at, ended_at)
events(task_id, seq, from_state, to_state, ts, detail, reported,
       PK(task_id, seq))
```

- 每次状态迁移单事务落盘（§11）；`seq` 每任务单调递增，与 idempotency_key
  联合作为 Runtime 去重依据。
- `LoadInflight()`：进程重启后返回非终态任务与未上报事件/结果，
  供恢复补报——崩溃不丢执行一致性（§1）。

### 3.3 reporter（agent/internal/reporter）

- HeartbeatLoop（10s，§10）：`{client_id, agent_version, base_url, devices[],
  active_task_ids[]}`；devices 由 `adb Devices` + getprop + df 组装；
  失败指数退避，不阻塞任务执行；ack 仅要求 `ok`。
- EventReporter：挂 `Executor.OnTransition`，seq 单事务递增 →
  POST `/callbacks/v1/task-events`；未确认事件后台重发。
- ResultReporter：run-summary + 收集清单 + 上传结果组装 result.json
  （符合 `contracts/result.schema.json`）→ POST `/callbacks/v1/results`；
  500（signal_error）重发，400（schema_violation/unknown_task）不重发。

### 3.4 uploader（agent/internal/uploader）

对 dispatch 载荷的 `presigned_uploads[]` 逐项 PUT（Content-Length + 已有
sha256），产出 attachments 条目 `{name, object_key, sha256, size}`。
单项失败/过期降级为"该附件未上传，本地保留"，不阻断结果上报。

### 3.5 server（agent/internal/server）

§8.1 全端点：

- `POST /api/v1/tasks`：请求体过 JSON Schema（嵌入契约派生 schema，防漂移
  测试同 manifest 模式）→ store 幂等（同 idempotency_key 返回既有状态；
  同 task_id 异键 409）→ 202 入队异步执行；
- `GET /api/v1/tasks/{id}`：200 现状 / 404；
- `DELETE /api/v1/tasks/{id}`：202 受理（executor.Cancel）/ 404；
- `GET /api/v1/devices`：adb devices + 属性 + workdir_free_mb；
- `POST /api/v1/diagnostics`：`adb_devices|logcat_tail|df|getprop` 白名单，
  输出截断；**禁止任意 shell（红线 §14）**；
- `GET /healthz`：`{status: ok, agent_version, adb_server_port: 5137}`。

### 3.6 cmd/agent（服务模式）

- 配置：环境变量 + 配置文件（§13）；必填 `AGENT_CLIENT_ID`、
  `AGENT_RUNTIME_CALLBACK_URL`、`AGENT_BASE_URL`、`AGENT_ADB_PATH`。
- kardianos/service 包装：`run`（前台，默认）/`install`/`uninstall`/`start`/`stop`。
- 启动时 `LoadInflight` 恢复补报；agent-cli 保留不动。

### 3.7 runtime：MinIO 与预签名

- compose：`minio`（固定版本 + digest 锁）+ `minio-init`（建 bucket
  `hermes-evidence`）；`.env.example` 加 `MINIO_ROOT_USER/MINIO_ROOT_PASSWORD/
  MINIO_BUCKET/MINIO_PRESIGN_TTL_MIN`；18091、9000 绑 LAN（bind IP 变量化）。
- worker 新 env：`MINIO_ENDPOINT`（容器内）、`MINIO_PUBLIC_ENDPOINT`
  （预签名 URL 的 host，须为 Client 可达的 LAN 地址）、`MINIO_ACCESS_KEY/
  MINIO_SECRET_KEY/MINIO_BUCKET/MINIO_PRESIGN_TTL`。
- dispatch activity：用 minio-go v7 对固定键集预签 PUT，填入
  `presigned_uploads[]`；MinIO 不可用时预签失败 → 空数组降级 + 记日志
  （附件缺失不构成 INFRA 重试理由，结果回流优先）。

## 4. 幂等与恢复语义

- dispatch 幂等键 `{workflow_id}:{task_id}:a{attempt}` 全链路唯一依据：
  Runtime 重试（活动级 ≤3 + INFRA 级 ≤2）产生重复 POST /tasks，Agent 返现状不重复执行；
- 事件 `(idempotency_key, seq)` 去重，重发安全；
- result 按 task_id 去重，重复上报返回 200 不重投 signal；
- Agent 崩溃重启：SQLite 恢复进行中任务状态与未上报事件/结果；
- Runtime 判死（租约过期/硬超时）→ DELETE cancel → Agent kill 设备进程，
  终态以回调为准。

## 5. 验证

- 单测：fake `adb.Runner` + httptest 假 Runtime 全链路无设备覆盖；
  表驱动状态机（§13）；store 故障注入恢复测试；契约防漂移测试。
- q-uat：compose 含 MinIO 全健康；bucket 就绪；预签 URL 可 PUT。
- Windows 实机（用户提供 runbook）：服务安装启动 → 心跳登记 →
  真实 pipeline 派单 → 开发板实测 → verdict + MinIO 附件 + 飞书通知；
  故障注入：杀 agent 进程恢复补报、拔 USB INFRA 收敛、重复回调幂等。

## 6. 安全边界

- 回调与 MinIO 为明文 HTTP，仅限测试网段（见决策 2）；Phase 3 mTLS 收敛。
- 秘密（MinIO root 密钥、ARTIFACT_AUTH_TOKEN）只存 `deploy/.env`；
  预签名 URL 含签名但限时（缺省 60min），不落日志。
- Agent 不提供任意 shell；diagnostics 仅四探测。
