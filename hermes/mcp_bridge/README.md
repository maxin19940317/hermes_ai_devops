# mcp_bridge — Hermes Agent 平台 ↔ DevOps Runtime 的 MCP 适配层(2026-08-07)

把 hermes-agent 平台(WebUI `/chat?profile=tobias_pm` / 飞书 gateway)里的自然
语言指令翻译成 **Runtime 受控接口调用**,从而在 Hermes 里就能查看设备、触发测试。

**这是对 18765 端口那个 `tobias_adb_server`(直连 ADB 的违规 MCP)的合规替代**:
本 bridge 不直连 ADB、不执行任意命令,所有工具都是封闭指令的翻译
(CLAUDE.md §3.1/§3.3/§14 红线)。

## 架构

```
hermes-rocklin(WebUI :9221, tobias_pm)
   ↓ MCP Streamable HTTP(2025-03-26)
mcp_bridge(hermes-devops-analyzer 容器, :8645)
   ↓ HTTPS + mTLS(client-mcp-bridge 证书)+ Bearer
worker(:8091 /api/v1/cmd, cmdapi 包)
   ↓
feishucmd.Executor(全部指令逻辑,与飞书共用)
   ↓
Postgres / Temporal → Windows Agent → 设备测试
```

- **MCP bridge**:Python FastMCP,只做信封翻译(自然语言→封闭指令),无 LLM、无状态。
- **cmdapi**(`runtime/internal/cmdapi`):Runtime 侧受控 HTTP 接口,Bearer 鉴权,
  封闭枚举指令,复用 feishucmd.Executor。
- 执行逻辑全部在确定性 Runtime(worker),LLM 不在关键路径(§3 规则 5)。

## 工具(11 个)

| 工具 | 指令 | 类型 |
|---|---|---|
| `devops_devices` | devices | 只读 |
| `devops_status` | status | 只读 |
| `devops_runs [n]` | runs | 只读 |
| `devops_result <workflow_id>` | result | 只读 |
| `devops_metrics <variant>` | metrics | 只读 |
| `devops_artifacts <variant>` | artifacts | 只读 |
| `devops_test <variant>` | test | 副作用 |
| `devops_cancel <workflow_id>` | cancel | 副作用 |
| `devops_rerun <source_wf> [variant]` | rerun | 副作用 |
| `devops_quarantine [device_id]` | quarantine | 副作用 |
| `devops_unquarantine [device_id]` | unquarantine | 副作用 |

## 部署(q-uat)

1. **Runtime cmdapi**:`CMD_API_TOKEN` 写入 `deploy/.env`,compose 传给 worker;
   重建镜像 `docker build -f runtime/Dockerfile -t hermes-runtime:dev .`
   + `docker compose up -d --no-deps --force-recreate worker`。
2. **mTLS 证书**:`./scripts/generate-certs.sh mcp-bridge` 签专用客户端证书;
   服务端证书重签时 SAN 必须含 `DNS:worker`(MCP bridge 用 `https://worker:8091`
   访问)。注意:重签后 `server-key.pem`/`server-cert.pem`/`server-combined.pem`
   属主必须是 `100:101`(hermes 用户),否则 worker 起不来。
3. **MCP bridge 文件**拷入 analyzer 容器 `/opt/data/bin/`:
   `mcp_bridge.py`、`start-mcp-bridge`、`ca-cert.pem`、`client-mcp-bridge.pem`。
4. **env 文件** `/opt/data/bin/mcp_bridge.env`:
   `RUNTIME_CMD_API_URL=https://worker:8091/api/v1/cmd`、
   `RUNTIME_CMD_API_TOKEN=<与 CMD_API_TOKEN 一致>`、
   `MTLS_CA_FILE=/opt/data/bin/ca-cert.pem`、
   `MTLS_CLIENT_CERT=/opt/data/bin/client-mcp-bridge.pem`。
5. **启动**:`cd /opt/data/bin && bash start-mcp-bridge`(幂等,pidfile 在
   `/opt/data/logs/mcp_bridge.pid`)。
6. **analyzer 端口**:`8645` 必须暴露到 `0.0.0.0`(hermes-rocklin 经
   `172.17.0.1:8645` 访问;analyzer 原 8643 是 `127.0.0.1`,重建容器时加
   `-p 0.0.0.0:8645:8645`)。
7. **hermes-rocklin 注册**(tobias_pm profile):
   `hermes mcp add hermes_devops --url http://172.17.0.1:8645/mcp`,
   交互:auth→No,enable all 11 tools→Yes。需 pty 驱动交互
   (getpass 不吃管道;见下)。

## 验证

```bash
# MCP server 就绪
curl -s -N -X POST http://127.0.0.1:8645/mcp -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"p","version":"1"}}}'

# hermes 能看到 server
hermes mcp list   # hermes_devops ✓ enabled

# 端到端(聊天触发工具)
hermes -z "查看设备列表"
hermes -z "用 devops_test 测试变体 aarch64_Android_QCM6125_SNPE_1.68"

# Temporal 里确认 workflow 起来了
docker exec hermes-runtime-temporal-1 temporal workflow list --address temporal:7233 \
  --query "WorkflowType='DeviceTestWorkflow' AND ExecutionStatus='Running'"
```

## 已知坑

- **Host 421**:FastMCP 默认 DNS rebinding 保护只放行 localhost Host;hermes-rocklin
  经 `172.17.0.1:8645` 访问,必须在 `TransportSecuritySettings.allowed_hosts` 加
  `172.17.0.1:*`(带端口通配)。否则 `421 Misdirected Request`。
- **mTLS AKI**:自签 CA 缺 Authority Key Identifier,Python OpenSSL 默认
  `VERIFY_X509_STRICT` 会拒;bridge 里 `ssl_ctx.verify_flags &= ~ssl.VERIFY_X509_STRICT`
  放宽(仅跳过 AKI,CA 链校验保留)。
- **hostname SAN**:MCP bridge 用 `https://worker:8091`(不是容器全名),服务端证书
  SAN 必须含 `DNS:worker`。
- **getpass 不吃管道**:`hermes mcp add` 的 auth 提示用 getpass,printf 管道无效,
  需 pty 驱动(时序:n → 等工具发现 → y)。

## 测试

```bash
# 容器内跑逻辑测试(工具注册/参数翻译/鉴权/降级)
docker exec hermes-devops-analyzer sh -c 'cd /tmp && /opt/hermes/.venv/bin/python -m pytest test_mcp_bridge.py -q'
# 仓库内跑 cmdapi Go 测试
cd runtime && go test ./internal/cmdapi/ ./internal/feishucmd/
```

## 文件

- `mcp_bridge.py` — FastMCP server(工具 + Runtime 调用翻译)
- `start-mcp-bridge` — 启动脚本(env + pidfile + nohup,幂等)
- `test_mcp_bridge.py` — 逻辑测试(假 httpx)
- Runtime 侧:`runtime/internal/cmdapi/`(cmdapi.go + cmdapi_test.go)
