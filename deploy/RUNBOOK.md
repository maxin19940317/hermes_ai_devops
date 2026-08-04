# q-uat 运维手册(Runbook)

组件拓扑与详细配置见 [README.md](README.md);本文件是可复制的操作流程。
所有命令假设工作目录为仓库根目录(`~/Code/hermes_ai_devops`)。

## 组件清单与归属

| 组件 | 形态 | 更新方式 |
|---|---|---|
| trigger / worker / relay | compose 三进程,共享镜像 `hermes-runtime:dev` | §1 标准流程 |
| postgres / temporal / minio / temporal-ui / grafana | compose 基础设施 | 不随业务更新;勿动 |
| analyze_bridge | `hermes-devops-analyzer` 容器内 uvicorn(:8643) | §3.1 |
| kanban_bridge | 宿主 tobias 账号 uvicorn(:8644) | §3.2 |
| Windows agent | `windows-client-01` 前台进程(8480/5137) | §3.3 |

## 1. Runtime 标准更新流程

```bash
cd ~/Code/hermes_ai_devops

# 0. 记录回滚镜像
docker image inspect hermes-runtime:dev --format '{{.Id}}'

# 1. 迁移:deploy/postgres/migrations/ 有新文件就先跑(幂等,可重复执行)
docker exec -i hermes-runtime-postgres-1 psql -U hermes_runtime -d hermes_runtime \
  -v ON_ERROR_STOP=1 < deploy/postgres/migrations/<新文件>.sql

# 2. 构建 + 滚动重建(只动业务三进程,基础设施保持不动)
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml build trigger
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml up -d --no-deps trigger worker relay

# 3. 验证
curl -fsS http://127.0.0.1:18090/healthz   # trigger
# worker/callbacks 已启用 mTLS(2026-08-04):必须带客户端证书
curl -fsS --cacert deploy/certs/ca-cert.pem \
  --cert deploy/certs/client-windows-client-01.pem \
  https://127.0.0.1:18091/healthz
docker logs hermes-runtime-relay-1 --tail 5
docker logs hermes-runtime-worker-1 2>&1 | grep -E "feishu|listener|nl=" | head -3
```

注意事项:

- `lock-images.sh` 依赖 dockerhub 镜像站解析 digest;镜像站超时(实测 dockerhub.icu
  会挂)时**跳过该步直接 build**,tag 未变就无影响,网络恢复后补跑。
- 新增环境变量必须同时在 `deploy/docker-compose.yml` 的 environment 段透传,否则
  容器内读不到(踩过两次:ARTIFACT_AUTH_USERNAME、FEISHU_CMD_NL)。检查方法:
  `grep -oE '"[A-Z_]{4,}"' runtime/cmd/worker/config.go | sort -u` 对照 compose。
- workflow_runs 类「删旧约束」的迁移是**停写窗口型**:先停 trigger/worker → 迁移 →
  部署新二进制 → 再启动,不可混跑。迁移文件头注释会写明,执行前先读。

## 2. 回滚

```bash
docker tag <第 0 步记录的镜像ID> hermes-runtime:dev
docker compose --env-file deploy/.env --env-file deploy/images.lock.env \
  -f deploy/docker-compose.yml up -d --no-deps trigger worker relay
```

不要用 `down -v`:`hermes-runtime-postgres` 卷里有 Temporal 与业务状态。

## 3. 旁挂服务更新

### 3.1 analyze_bridge(hermes-devops-analyzer 容器)

```bash
docker cp hermes/analyze_bridge/analyze_bridge.py hermes-devops-analyzer:/opt/data/bin/
# 契约副本有更新时一并拷贝(防漂移测试会强制两边一致;
# 三份:command=NL 指令翻译,analysis=Analyzer,plan=Planner)
docker cp contracts/command.schema.json hermes-devops-analyzer:/opt/data/bin/ || true
docker cp contracts/analysis.schema.json hermes-devops-analyzer:/opt/data/bin/ || true
docker cp contracts/plan.schema.json hermes-devops-analyzer:/opt/data/bin/ || true
docker exec hermes-devops-analyzer sh -c 'kill $(cat /opt/data/logs/analyze-bridge-8643.pid) 2>/dev/null; sleep 1; /opt/data/bin/start-analyze-bridge'
curl -fsS http://127.0.0.1:8643/health
```

### 3.2 kanban_bridge(宿主)

```bash
cd hermes/kanban_bridge
UVICORN_BIN=~/Code/hermes_ai_devops/.venv/bin/uvicorn ./start-kanban-bridge   # 幂等重启
curl -fsS http://127.0.0.1:8644/health
```

依赖在仓库 `.venv`(fastapi/uvicorn/jsonschema);env 文件 `~/kanban_bridge.env`
(600 权限,含 KANBAN_BRIDGE_TOKEN)。

### 3.3 Windows agent

```powershell
# 前台窗口 Ctrl+C 停止,覆盖 dist\agent.exe(新文件字节数与 q-uat dist 目录比对),然后:
powershell -ExecutionPolicy Bypass -File .\dist\start-agent.ps1
```

验证:q-uat 上 `docker exec hermes-runtime-postgres-1 psql -U hermes_runtime -d hermes_runtime -c "select client_id, last_heartbeat from clients"`,心跳应在 10s 内。

### 3.4 mTLS 证书(deploy/certs/,已 gitignore)

- **签发新 client**:`./scripts/generate-certs.sh <new-client-id>` —— CA/Server 已存在时自动复用,
  只签新 client(脚本是幂等的;删除 server-cert.pem 重跑可换 SAN,但 CA 绝不能删,
  重签 CA 会使全部已发证书作废)。产物 `client-<id>.pem` 拷到目标机。
- **属主约定**:容器内 worker 以 uid 100 运行,`server-*.pem`/`ca-cert.pem` 须属 100:101;
  `ca-key.pem` 属宿主用户(1006)即可——容器不需要也读不到它。重跑 generate-certs 后
  新增的 server 侧 pem 要重新 chown:`docker run --rm -v $PWD/deploy/certs:/c alpine chown 100:101 /c/server-*.pem /c/ca-cert.pem`。
- **回调健康检查**(18091 已强制客户端证书):
  `curl -fsS --cacert deploy/certs/ca-cert.pem --cert deploy/certs/client-windows-client-01.pem https://127.0.0.1:18091/healthz`
- **回滚纯 HTTP**:`deploy/.env` 的 MTLS_CA_FILE/CERT_FILE/KEY_FILE 三项清空 +
  `up -d --no-deps worker`,Windows 侧摘掉 `AGENT_MTLS_*` 两个变量重启 Agent。

## 4. 健康速查

```bash
# 设备与租约
docker exec hermes-runtime-postgres-1 psql -U hermes_runtime -d hermes_runtime \
  -c "select device_id, soc, status, fail_streak from devices" \
  -c "select * from device_leases"
# outbox 积压(应接近 0)
docker exec hermes-runtime-postgres-1 psql -U hermes_runtime -d hermes_runtime \
  -c "select count(*) filter (where published_at is null) as pending, max(attempts) from outbox"
# workflow
docker exec hermes-runtime-temporal-1 temporal workflow describe \
  --address temporal:7233 -w '<workflow_id>'
# MinIO 证据(附件 runs/,快照 evidence/)
docker logs hermes-runtime-worker-1 --since 1h 2>&1 | grep -v DEBUG | tail -20
```

## 5. 已知坑(实机踩过)

| 症状 | 根因 | 处置 |
|---|---|---|
| 设备永久 BUSY,后续全部 "no device available" | workflow 被 terminate,旧代码无租约回收 | 已修(懒回收);残留时手动清 `device_leases` 行 + 置 IDLE |
| 任务 DOWNLOADING 秒挂,basic auth 报错 | compose 未透传 env | 见 §1 注意 2,补透传后 `up -d --no-deps worker` |
| adb 只显示 `?`,任务 precheck "device not found" | USB gadget serial 丢失 | 新 agent 自动解析寻址;旧 agent 需 ConfigFS 写 serial(见 agent/dist/README.md) |
| 非 Android 设备注册进表,soc 是 shell 错误文本 | probe 用 Android 命令探 Linux 设备 | 人工置 QUARANTINED;agent probe 防护待做 |
| 飞书 NL 指令收到两条回复 | WS 事件 ack 超时重投 | 已修(异步处理 + message_id 去重) |
| Hermes 分析连续 502 | DeepSeek 余额不足 | 充值或换 key;规则引擎保底,主链路不受影响 |
| 换了 LLM key/endpoint 后 hermes -z 仍 "no final response" | 凭证池缓存:`/opt/data/auth.json` 里 provider 凭证带着旧 base_url 和 `exhausted` 状态,优先级高于 config.yaml 和 .env | 除改 `config.yaml`(base_url/default)和 `.env`(key)外,必须清凭证池:`hermes auth remove deepseek <id>`(注意它会顺带删 `.env` 里的 key 行并禁止 env 重播种,需补回)或 `hermes auth add deepseek --type api-key --api-key <key>`,并确认 auth.json 里凭证的 base_url 指向新 endpoint |
| `docker compose up` 后 NL/新功能不生效 | 同"compose 未透传" | 同上 |
