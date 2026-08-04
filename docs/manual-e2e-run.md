# 端到端手动运行指南

本文档描述如何手动触发一次完整的设备测试链路：**从 GitLab CI 构建到飞书收到通知**。
覆盖两条路径：变体级 `/kick`（主路径）与 pipeline webhook（兜底），以及
Windows Client Agent 侧的准备与核对。

所有命令假设工作目录为仓库根目录（`~/Code/hermes_ai_devops`），服务器为
`q-uat`（`10.88.118.251`），Windows Client 为 `10.88.118.51`。

---

## 前置条件清单

开始前确认以下条件全部满足：

| # | 条件 | 核对命令 |
|---|---|---|
| 1 | Runtime 三个进程健康 | `curl -fsS http://127.0.0.1:18090/healthz && curl -fsS --cacert deploy/certs/ca-cert.pem --cert deploy/certs/client-windows-client-01.pem https://127.0.0.1:18091/healthz && docker logs hermes-runtime-relay-1 --tail 3` |
| 2 | Temporal 健康 | `docker exec hermes-runtime-temporal-1 tctl --address temporal:7233 cluster health` |
| 3 | MinIO 健康 | `curl -fsS http://127.0.0.1:9000/minio/health/live` |
| 4 | 有可用设备 | `docker exec hermes-runtime-postgres-1 psql -U hermes_runtime -d hermes_runtime -c "SELECT device_id, status, fail_streak FROM devices"` — 至少一行 `status=IDLE` |
| 5 | Windows Agent 心跳在线 | 同表 `SELECT client_id, last_heartbeat FROM clients` — 心跳在 30s 内 |
| 6 | GitLab 可达 | `curl -fsS -H "PRIVATE-TOKEN: $GITLAB_TOKEN" "$GITLAB_BASE_URL/api/v4/version"` |
| 7 | 业务仓库已有成功 pipeline | GitLab 上 algo-super-sdk 的 master 分支至少有一条 `success` 状态的 pipeline，且 8 个变体的 `bundle-g{sha}-p{iid}.json` 已上传 Registry |

如果第 4 项无设备，先在 Windows Client 上启动 Agent（见 §3）。

---

## 1. 触发测试（服务器侧）

有两种触发方式，按优先级选择：

### 方式 A：变体级 `/kick`（推荐，主路径）

变体级触发不依赖 pipeline webhook，**一个包上传成功即可测试**，无需等待全部 8 个变体。

**前提**：业务仓库 CI 已接入 `ci/kick.py`（§6.3），且 GitLab CI/CD 变量
`TRIGGER_KICK_URL` / `TRIGGER_KICK_TOKEN` 已配置。如果尚未接入，可手动模拟：

```bash
# 1. 从 Registry 拉取已有 meta JSON（以某个变体为例）
#    或直接从 GitLab CI artifact 下载 dist/meta/{variant}.json

# 2. 手动发送 /kick（使用与 webhook 相同的 TRIGGER_WEBHOOK_SECRET）
curl -v -X POST \
  -H "Content-Type: application/json" \
  -H "X-Gitlab-Token: $(grep TRIGGER_WEBHOOK_SECRET deploy/.env | cut -d= -f2)" \
  --data-binary @dist/meta/aarch64_Android_SNPE_2.21.json \
  http://127.0.0.1:18090/kick
```

**预期响应**：`202 Accepted`，JSON body 含 `workflow_id` 和 `started: true`。

如果该变体已有结论（`PASSED` 或 `TEST_FAILED`），返回 `started: false`（幂等跳过）。

### 方式 B：Pipeline Webhook（兜底）

模拟 GitLab 发送 pipeline success webhook，触发完整 bundle workflow：

```bash
# 使用 verify-pipeline.sh 同样的逻辑，手动构造 payload
# 需要一个已知成功的 pipeline（替换 PROJECT_ID 和 PIPELINE_GLOBAL_ID）
PROJECT_ID=651
PIPELINE_GLOBAL_ID=656

# 获取 pipeline 信息
SHA=$(curl -fsS -H "PRIVATE-TOKEN: $GITLAB_TOKEN" \
  "$GITLAB_BASE_URL/api/v4/projects/$PROJECT_ID/pipelines/$PIPELINE_GLOBAL_ID" \
  | jq -er '.sha')
REF=$(curl -fsS -H "PRIVATE-TOKEN: $GITLAB_TOKEN" \
  "$GITLAB_BASE_URL/api/v4/projects/$PROJECT_ID/pipelines/$PIPELINE_GLOBAL_ID" \
  | jq -er '.ref')
PROJECT_PATH=$(curl -fsS -H "PRIVATE-TOKEN: $GITLAB_TOKEN" \
  "$GITLAB_BASE_URL/api/v4/projects/$PROJECT_ID" \
  | jq -er '.path_with_namespace')

# 构造 webhook payload
PAYLOAD=$(jq -n \
  --argjson project_id "$PROJECT_ID" \
  --arg project_path "$PROJECT_PATH" \
  --argjson pipeline_id "$PIPELINE_GLOBAL_ID" \
  --arg sha "$SHA" \
  --arg ref "$REF" \
  '{object_kind:"pipeline",object_attributes:{id:$pipeline_id,ref:$ref,tag:false,sha:$sha,status:"success"},project:{id:$project_id,path_with_namespace:$project_path}}')

# 发送 webhook
curl -v -X POST \
  -H "Content-Type: application/json" \
  -H "X-Gitlab-Token: $(grep TRIGGER_WEBHOOK_SECRET deploy/.env | cut -d= -f2)" \
  --data-binary "$PAYLOAD" \
  http://127.0.0.1:18090/webhooks/gitlab
```

**预期响应**：`202 Accepted`，含 `workflow_id`。

**注意**：如果 `TRIGGER_PIPELINE_WEBHOOK=false`（kick 模式），webhook 仅记录不起 workflow。

### 方式 C：完整验证脚本

一键跑完 webhook + 去重验证 + artifact 计数 + Temporal workflow 确认：

```bash
PROJECT_ID=651 PIPELINE_GLOBAL_ID=656 \
  deploy/scripts/verify-pipeline.sh deploy/.env deploy/images.lock.env
```

预期输出：`PASS: pipeline 656 -> device-test-..., 8 artifacts, duplicate suppressed`。

---

## 2. 监控执行（服务器侧）

触发后立即可以开始观察：

### 2.1 查看 Workflow 状态

```bash
# 列出最近运行的 workflow
docker exec hermes-runtime-temporal-1 tctl --address temporal:7233 \
  workflow list --query 'ExecutionStatus="Running"'

# 查看具体 workflow 详情（用上一步返回的 workflow_id）
docker exec hermes-runtime-temporal-1 tctl --address temporal:7233 \
  workflow describe --workflow_id '<workflow_id>'
```

### 2.2 Temporal UI（浏览器）

```bash
# 本地转发（UI 仅绑定 127.0.0.1）
ssh -L 18080:127.0.0.1:18080 q-uat
# 浏览器打开 http://127.0.0.1:18080
```

在 UI 中可以看到 workflow 的完整 History：每个 Activity 的输入输出、Signal 接收、
Timer 触发等。

### 2.3 实时日志

```bash
# Worker 日志（Activity 执行、飞书通知、Hermes 分析）
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml logs -f --tail 50 worker

# Trigger 日志（webhook/kick 接收、bundle 拉取）
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml logs -f --tail 50 trigger

# Relay 日志（Outbox 投递）
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml logs -f --tail 50 relay
```

### 2.4 数据库状态

```bash
PG="docker exec hermes-runtime-postgres-1 psql -U hermes_runtime -d hermes_runtime"

# 任务进度
$PG -c "SELECT task_id, status, verdict, error_category FROM tasks ORDER BY created_at DESC LIMIT 10"

# 设备租约（看哪个设备被哪个任务占用）
$PG -c "SELECT dl.device_id, dl.task_id, dl.lease_expires_at, d.status
        FROM device_leases dl JOIN devices d ON d.device_id = dl.device_id"

# Artifact 登记（应看到 8 个变体）
$PG -c "SELECT variant, commit_sha, pipeline_id FROM artifacts ORDER BY variant"

# Outbox 积压（应接近 0）
$PG -c "SELECT * FROM outbox_backlog"
```

---

## 3. Windows Client Agent 准备

如果 Agent 尚未启动，在 Windows Client 机器上执行：

### 3.1 启动 Agent 服务模式

```powershell
cd D:\agent\dist

# 一键启动：准备 ADB 5137 + 自检 + 前台运行
powershell -ExecutionPolicy Bypass -File .\start-agent.ps1
```

脚本会自动完成：
1. 停止系统 ADB Server（5037），启动私有 ADB Server（5137）
2. 检查设备在线状态和属性（ABI、SoC）
3. 验证到 Runtime 的网络连通性（callbacks :18091、MinIO :9000）
4. 前台启动 Agent（输出 tee 到 `agent-console.log`）

### 3.2 验证 Agent 就绪

另开 PowerShell：

```powershell
# 健康检查
curl.exe http://127.0.0.1:8480/healthz
# 期望: {"status":"ok","agent_version":"...","adb_server_port":5137}

# 设备列表
curl.exe http://127.0.0.1:8480/api/v1/devices
# 期望: [{"serial":"...","state":"IDLE","props":{"soc":"QCM6125","abi":"arm64-v8a",...}}]
```

服务器侧确认心跳：

```bash
docker exec hermes-runtime-postgres-1 psql -U hermes_runtime -d hermes_runtime \
  -c "SELECT client_id, last_heartbeat FROM clients"
# last_heartbeat 应在 10s 内
```

### 3.3 用 agent-cli 手动跑 Smoke Test（可选，独立验证）

不依赖 Runtime，在 Windows 上直接用 CLI 跑 smoke 包验证 ADB 链路：

```powershell
$adb = "D:\platform-tools\adb.exe"
$serial = "513cd3de"

# 正常场景
.\agent-cli.exe run --package-file .\smoke-pkg-ok.tar.gz --serial $serial --adb $adb
echo $LASTEXITCODE   # 期望: 0

# 失败场景
.\agent-cli.exe run --package-file .\smoke-pkg-fail.tar.gz --serial $serial --adb $adb
echo $LASTEXITCODE   # 期望: 2

# 超时场景
.\agent-cli.exe run --package-file .\smoke-pkg-timeout.tar.gz --serial $serial --adb $adb
echo $LASTEXITCODE   # 期望: 3
```

产出在 `agent-runs\<UTC时间戳>\`，检查 `run-summary.json`、`device/results/result.json`、
`logcat.txt`。

---

## 4. 全链路执行流程（自动）

触发后，系统自动完成以下步骤（参考 `docs/device-test-sequence.md` 时序图）：

```
触发 → Trigger 拉 Bundle → 登记 Artifact → 启动 Workflow
  → SelectTestSpecs（按 variants.yaml 确定可测变体）
  → AcquireDevice（SELECT FOR UPDATE 独占锁设备）
  → CreateTask → DispatchTask（POST Client /api/v1/tasks）
  → Client: 下载产物 → Manifest 校验 → SHA256 校验 → ADB Push → 执行测试
  → Client: ADB Pull 收集 → 预签名直传 MinIO → POST 结果回调
  → Callback API: 单事务写 Result + Outbox
  → Outbox Relay: Signal 唤醒 Workflow
  → Workflow: LoadResult → 规则引擎判 verdict
  → [非 PASSED] ExtractEvidence → Hermes 分析 → SaveDecision
  → ReleaseDevice → Notify（飞书）
```

---

## 5. 核对终态

### 5.1 飞书通知

如果 `FEISHU_APP_ID`/`FEISHU_APP_SECRET`/`FEISHU_RECEIVE_ID` 或
`FEISHU_WEBHOOK_URL` 已配置，终态时飞书会收到通知，含：

- verdict（PASSED / TEST_FAILED / INFRA_ERROR / ...）
- 耗时、用例统计
- 非 PASSED 时附 Hermes summary
- 日志附件的 MinIO 链接

### 5.2 数据库终态

```bash
PG="docker exec hermes-runtime-postgres-1 psql -U hermes_runtime -d hermes_runtime"

# 任务终态（status 应为 COMPLETED/FAILED/TIMEOUT，verdict 已填）
$PG -c "SELECT task_id, status, verdict, error_category, ended_at
        FROM tasks ORDER BY created_at DESC LIMIT 10"

# 结果详情
$PG -c "SELECT task_id, result_json->>'status', result_json->'cases'
        FROM results ORDER BY created_at DESC LIMIT 10"

# 决策记录（规则引擎 + Hermes）
$PG -c "SELECT decision_id, task_id, actor, output->>'verdict', created_at
        FROM decisions ORDER BY created_at DESC LIMIT 10"

# Evidence 快照
$PG -c "SELECT evidence_id, task_id, object_key, extractor_version
        FROM evidence_snapshots ORDER BY created_at DESC LIMIT 10"

# 设备状态（应恢复 IDLE，fail_streak 已清零）
$PG -c "SELECT device_id, status, fail_streak FROM devices"

# Outbox 清空
$PG -c "SELECT * FROM outbox_backlog"   # pending 应为 0
```

### 5.3 MinIO 证据

```bash
# 查看附件（runs/ 前缀）和证据快照（evidence/ 前缀）
# 通过 MinIO Console（localhost:9001）或 mc 客户端：
docker exec hermes-runtime-worker-1 sh -c \
  'mc alias set hermes http://minio:9000 $MINIO_ACCESS_KEY $MINIO_SECRET_KEY && mc ls hermes/hermes-evidence/runs/'
```

### 5.4 Workflow 完成确认

```bash
docker exec hermes-runtime-temporal-1 tctl --address temporal:7233 \
  workflow describe --workflow_id '<workflow_id>'
# ExecutionStatus 应为 COMPLETED
```

---

## 6. 故障排查

### 6.1 常见问题速查

| 症状 | 排查 | 处置 |
|---|---|---|
| "no device available" | `$PG -c "SELECT * FROM devices"` — 可能全 BUSY 或 OFFLINE | 检查 Agent 心跳；手动 `UPDATE devices SET status='IDLE' WHERE ...`；清 `device_leases` |
| 任务卡在 DOWNLOADING | Worker 日志搜 `download` | 检查 `ARTIFACT_AUTH_TYPE`/`TOKEN`/`USERNAME` 是否正确透传 |
| 飞书未收到通知 | Worker 日志搜 `feishu` | 检查 `FEISHU_*` 环境变量是否齐全；`mode=disabled` 表示全空（开发模式） |
| Outbox 积压 | `$PG -c "SELECT * FROM outbox_backlog"` | Relay 日志查 `last_error`；Temporal 不可达时 Signal 失败 |
| Workflow 未启动 | Trigger 日志搜 `start workflow` | 检查 bundle 是否存在于 Registry；Schema 校验是否失败 |
| Agent 回调 401 | Worker 日志搜 `lease` | 心跳携带的 lease 凭据不匹配（设备已被重新分配） |
| Hermes 分析 502 | Worker 日志搜 `hermes` | `HERMES_ENDPOINT` 不可达或模型余额不足；规则引擎保底，不影响 verdict |

### 6.2 手动重试失败 Workflow

```bash
# 通过飞书指令（如果 FEISHU_CMD_WHITELIST 已配置）：
# 发送: rerun <source_workflow_id> [variant]
# 只接受 workflow_runs 里有权威记录且已关闭的源运行;
# 同一源运行+变体已有进行中的重试时会被认领拦截("重试正在进行中")。

# 或通过 /kick 带 retry 标记（需修改 meta JSON 加 "retry": true）
```

### 6.3 重启 Runtime（不影响进行中的任务）

```bash
# 滚动重建（只动业务三进程，基础设施保持不动）
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml build trigger
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml up -d --no-deps trigger worker relay

# 验证
curl -fsS http://127.0.0.1:18090/healthz
curl -fsS --cacert deploy/certs/ca-cert.pem --cert deploy/certs/client-windows-client-01.pem https://127.0.0.1:18091/healthz
```

Temporal 保证 Workflow 从 History 重放恢复，已完成的 Activity 不重复执行。

---

## 7. 快速参考

```bash
# ---- 服务器侧 ----
# 健康检查
curl -fsS http://127.0.0.1:18090/healthz   # trigger
curl -fsS --cacert deploy/certs/ca-cert.pem --cert deploy/certs/client-windows-client-01.pem https://127.0.0.1:18091/healthz   # worker/callbacks(mTLS)

# 完整验证
PROJECT_ID=651 PIPELINE_GLOBAL_ID=656 \
  deploy/scripts/verify-pipeline.sh deploy/.env deploy/images.lock.env

# 查看最新任务
docker exec hermes-runtime-postgres-1 psql -U hermes_runtime -d hermes_runtime \
  -c "SELECT task_id, status, verdict FROM tasks ORDER BY created_at DESC LIMIT 5"

# 查看日志
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml logs -f --tail 100 worker

# ---- Windows Client 侧 ----
# 启动 Agent
powershell -ExecutionPolicy Bypass -File .\dist\start-agent.ps1

# Agent 健康
curl.exe http://127.0.0.1:8480/healthz

# Smoke test（不依赖 Runtime）
.\dist\agent-cli.exe run --package-file .\dist\smoke-pkg-ok.tar.gz --serial 513cd3de --adb D:\platform-tools\adb.exe
```
