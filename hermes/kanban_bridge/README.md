# kanban_bridge — DevOps → PM 升级通道的宿主侧薄桥

Runtime Escalate 活动与 kanban(`hermes kanban`,在 `hermes-rocklin` 容器内)之间的
唯一适配点(设计:docs/superpowers/specs/2026-07-30-devops-pm-escalation-design.md §6):
信封(escalation.schema.json)→ 渲染 title/body → `docker exec` 参数数组调用
(非 shell 拼接)→ 返回 `{kanban_task_id, created|existing}`。无状态、无 LLM。

- `POST /escalations` — Bearer 校验 → Schema 校验 → kanban create;
  幂等键重复(kanban 内建去重返回同一 task id)时追加 comment(新 pipeline 摘要)。
- worker 不持 docker 权限;本服务跑在 q-uat 宿主(tobias 账号,docker 组),
  **不在任何容器内,不在 deploy/docker-compose.yml 里**,由 `start-kanban-bridge` 启停。
- 渲染上限:title ≤ 200、body ≤ 8KB(截断标记 `…(truncated)`);幂等键由
  devops 在信封 `idempotency_key` 给出,bridge 不推导。
- CLI 非零/超时(`KANBAN_TIMEOUT_SEC`,缺省 30)→ 502,Runtime 按旁路降级只记日志。

## 配置(env 文件,`start-kanban-bridge` source)

| 变量 | 缺省 | 说明 |
|---|---|---|
| `KANBAN_BRIDGE_TOKEN` | (必填) | Bearer 共享密钥,对应 Runtime `ESCALATION_TOKEN` |
| `KANBAN_CONTAINER` | `hermes-rocklin` | kanban 所在容器 |
| `KANBAN_BOARD` | `algo_super_sdk` | 目标 board |
| `KANBAN_ASSIGNEE` | `tobias_pm` | 受理人 |
| `KANBAN_TIMEOUT_SEC` | `30` | 单次 docker exec 超时 |
| `KANBAN_DOCKER_BIN` | `docker` | docker CLI 路径(测试注入 fake) |

## 文件

- `kanban_bridge.py` — FastAPI 应用(`GET /health`、`POST /escalations`)
- `escalation.schema.json` — `contracts/escalation.schema.json` 的部署副本
  (防漂移由 `test_kanban_bridge.py::test_schema_copy_matches_contracts` 保证)
- `start-kanban-bridge` — 启动脚本(env 文件 + pidfile + nohup uvicorn,幂等;端口 8644)
- `test_kanban_bridge.py` — pytest(假 docker exec 驱动)

## 测试

```bash
.venv/bin/python -m pytest hermes/kanban_bridge -q
```

依赖 `fastapi uvicorn httpx jsonschema`(宿主 uvicorn 经 `UVICORN_BIN` 指定)。

## 部署(q-uat 宿主,tobias 账号)

```bash
# 1. 写 env 文件(含共享密钥,勿入 git)
install -m 600 /dev/null ~/kanban_bridge.env
echo 'KANBAN_BRIDGE_TOKEN=<与 Runtime ESCALATION_TOKEN 相同>' >> ~/kanban_bridge.env

# 2. 启动(幂等)
./start-kanban-bridge        # 需要 uvicorn 在 PATH;否则 UVICORN_BIN=/path/to/uvicorn

# 3. 验证
curl -fsS http://127.0.0.1:8644/health
curl -fsS -X POST http://127.0.0.1:8644/escalations \
  -H "Authorization: Bearer <token>" -H 'Content-Type: application/json' \
  -d @contracts/tests/examples/escalation/valid/full_envelope.json
# 预期:{"kanban_task_id":"t_...","result":"created"};再发一次 → "existing" 且 kanban 上有新 comment
```

Runtime 侧启用:deploy/.env 填 `ESCALATION_ENDPOINT=http://<宿主>:8644` 与
`ESCALATION_TOKEN`(同上),重启 worker;空 = 升级禁用(现状)。
