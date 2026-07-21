# Client Agent 服务化实施计划（Phase 1.7）

日期：2026-07-21。设计：`../specs/2026-07-21-agent-service-design.md`。基线：master `52950b5`。

每任务：失败测试先行 → 实现 → 全量测试 → 提交（英文提交信息）。

## Task 2 — executor 取消 + adb 补全

- `executor.Status` 加 `CANCELED`；`Executor.Cancel()` 幂等：取消标志 → RUNNING 中则
  `context.WithoutCancel` + 超时 `ShellPkill`（复用超时 kill 路径）→ COLLECTING →
  cleanup（尊重 keep_on_failure）→ 终态 CANCELED；终态后调用无副作用。
- `adb.Devices()`（无 `-s`，解析 `devices -l`）；`adb.LogcatTail(serial, lines)`。
- 表驱动状态机测试：cancel-during-RUNNING、cancel-after-terminal、取消仍收集。
- 提交：`feat(agent): add task cancellation and adb probes`

## Task 3 — store：SQLite 本地持久化

- `modernc.org/sqlite`，WAL。schema：
  `tasks(task_id PK, idempotency_key UNIQUE, state, attempt, dispatch_json, out_dir, started_at, ended_at)`、
  `events(task_id, seq, from_state, to_state, ts, detail, reported, PK(task_id,seq))`。
- 状态迁移单事务落盘；`LoadInflight()` 供崩溃恢复补报。
- 故障注入测试：异常关闭后重开恢复。
- 提交：`feat(agent): add SQLite task store with crash recovery`

## Task 4 — reporter：心跳/事件/结果

- HeartbeatLoop 10s：`{client_id, agent_version, base_url, devices, active_task_ids}`，
  退避重试不阻塞执行，ack 只要求 `ok`。
- EventReporter：挂 `OnTransition`，seq 单事务递增 → POST task-events，未确认后台重发。
- ResultReporter：组装 result.json（过 result.schema.json）→ POST results；500 重发，400 不重发。
- httptest 假 Runtime 覆盖乱序/重发/恢复补报。
- 提交：`feat(agent): add heartbeat and callback reporters`

## Task 5 — uploader：预签名直传

- 对 `presigned_uploads[]` 逐项 PUT（Content-Length + sha256）→ attachments 条目；
  单项失败降级"本地保留"，不阻断结果上报。
- 提交：`feat(agent): add presigned upload client`

## Task 6 — server：§8.1 RPC 壳 + cmd/agent

- `POST /api/v1/tasks`（Schema 校验 → store 幂等：同键返现状/异键 409 → 202 异步执行）、
  `GET`/`DELETE /api/v1/tasks/{id}`（404；DELETE 经 executor.Cancel）、
  `GET /api/v1/devices`、`POST /api/v1/diagnostics`（四探测白名单 + 截断）、
  `GET /healthz`（status/agent_version/5137）。
- 请求体过嵌入契约 schema，防漂移测试同 manifest 模式。
- `cmd/agent`：env + 配置文件；`AGENT_CLIENT_ID/AGENT_RUNTIME_CALLBACK_URL/AGENT_BASE_URL/AGENT_ADB_PATH` 必填；
  kardianos/service：run/install/uninstall/start/stop；启动 LoadInflight 恢复。agent-cli 不动。
- 提交：`feat(agent): add RPC server and service mode`

## Task 7 — Runtime：MinIO + 预签名 + LAN 暴露

- compose：`minio`（固定版本+digest 锁）+ `minio-init`（建 bucket `hermes-evidence`）；
  `.env.example` 加 `MINIO_ROOT_USER/MINIO_ROOT_PASSWORD/MINIO_BUCKET/MINIO_PRESIGN_TTL_MIN/
  MINIO_PUBLIC_ENDPOINT`；18091/9000 绑 LAN（bind IP 变量化）；deploy 契约测试同步。
- worker env：`MINIO_ENDPOINT/MINIO_PUBLIC_ENDPOINT/MINIO_ACCESS_KEY/MINIO_SECRET_KEY/MINIO_BUCKET/MINIO_PRESIGN_TTL`；
  dispatch activity 用 minio-go v7 预签固定键集（`runs/{task_id}/{result.json,junit.xml,logcat.txt,stdout.log,stderr.log}`）；
  MinIO 不可用 → 空数组降级 + 日志（不触发 INFRA 重试）。
- 提交：`feat(runtime): presign MinIO uploads in dispatch`

## Task 8 — 契约 private_token（仅验收证实需要时）

- `contracts/client-agent-api.openapi.yaml` auth enum 加 `private_token` + 正反例；
  artifact.Auth 支持 PRIVATE-TOKEN 头；runtime dispatch 透传。
- 提交：`feat(contracts): add private_token artifact auth type`

## Task 9 — 端到端验收（q-uat + Windows 实机）

1. q-uat：lock-images → validate-env → compose up → 含 MinIO 全健康；bucket 就绪；预签 URL 可 PUT。
2. Windows（用户按 runbook）：5137 adb 就绪 → agent 前台/服务启动 → 心跳出现在 worker 日志、
   `GET /api/v1/devices` 返回开发板。
3. 全链路：重放 pipeline → 派单 → 实测 → result → verdict → Temporal 收敛；
   核对 MinIO 附件、results 表、零重复执行。
4. 故障注入：杀 agent 恢复补报；拔 USB INFRA 收敛；重复回调幂等。
5. `ARTIFACT_AUTH_TOKEN`（轮换后 PAT）+ `FEISHU_WEBHOOK_URL` → 含 verdict 与日志链接的通知 = Phase 1 DoD。

## Task 10 — 文档、审查、交付

- 更新 `agent/README.md`、`deploy/README.md`、根 README 进度表；
  独立审查（幂等键、取消竞态、秘密、LAN 边界）；推送 → PR → 合并；写 handoff。

## 本地验证基线（每任务必跑）

```bash
cd agent && ~/.local/go/bin/go test ./...
cd runtime && ~/.local/go/bin/go test ./...
python3 -m unittest discover -s deploy/tests
python3 -m pytest contracts/tests
```
