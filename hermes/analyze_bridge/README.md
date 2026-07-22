# analyze_bridge — hermes-agent 平台的 Analyzer HTTP 适配层

Runtime(`hermesclient`)与 hermes-agent 平台之间的唯一适配点(CLAUDE.md §4/§12
Phase 2):把 Runtime 的 `POST /analyze` 翻译成平台内 `hermes -z` 一次性调用,
输出经 `analysis.schema.json` 校验后返回;平台失败/输出不合规返回 502,
Runtime 按 §9 降级到规则引擎保底。

- 工具白名单(§3):每次调用固定 `-t ""`(空工具集),Analyzer 无任何工具能力。
- 打回重试:输出未过 Schema 校验时,把校验错误附进 prompt 重试,上限
  `ANALYZE_MAX_ATTEMPTS`(缺省 3)。
- 凭据:provider key/模型配置全在实例内,本服务不感知;`ANALYZE_BRIDGE_TOKEN`
  是与 Runtime `HERMES_AUTH_TOKEN` 对应的共享密钥。
- 实测基线(2026-07-22,deepseek-v4-pro):`-z` stdout 只含最终响应文本;
  `-t ""` 冷/热约 76s/13s——Runtime `HERMES_TIMEOUT_SEC` 建议 ≥120。

## 文件

- `analyze_bridge.py` — FastAPI 应用(`GET /health`、`POST /analyze`)
- `analysis.schema.json` — `contracts/analysis.schema.json` 的部署副本
  (防漂移由 `test_analyze_bridge.py::test_schema_copy_matches_contracts` 保证)
- `start-analyze-bridge` — 实例内启动脚本(env 文件 + pidfile + nohup uvicorn,
  幂等;形态同实例既有 `start-queinfer-gitlab-bridge`)
- `test_analyze_bridge.py` — pytest(假 hermes CLI 驱动,13 例)

## 测试

```bash
.venv/bin/python -m pytest hermes/analyze_bridge -q
```

依赖 `fastapi uvicorn httpx jsonschema`(实例 venv `/opt/hermes/.venv` 已自带
fastapi/uvicorn/jsonschema;仓库 `.venv` 用于跑测试)。

## 部署(专用实例,CLAUDE.md Phase 2:不宜挂在个人实例上)

以下在 q-uat 宿主执行。实例名 `hermes-devops-analyzer`,端口 8643。

```bash
# 1. 准备实例 home(最小配置:provider + key)
docker volume create hermes-analyzer-data
docker run --rm -v hermes-analyzer-data:/opt/data nousresearch/hermes-agent:latest \
  sh -c 'mkdir -p /opt/data/bin /opt/data/logs'
# 写入 config.yaml(provider: deepseek)与 .env(DEEPSEEK_API_KEY=...)——
# 参照现有实例 /opt/data 的同名文件,秘钥不落 Git。

# 2. 拷贝 bridge 文件到实例 home
for f in analyze_bridge.py analysis.schema.json start-analyze-bridge; do
  docker run --rm -v "$PWD/hermes/analyze_bridge:/src:ro" \
    -v hermes-analyzer-data:/opt/data alpine:3.22.1 \
    sh -c "cp /src/$f /opt/data/bin/ && chmod +x /opt/data/bin/start-analyze-bridge || true"
done

# 3. 起专用实例,接入 hermes-runtime 网络(worker 按容器名直连,不经宿主端口)
docker run -d --name hermes-devops-analyzer --restart unless-stopped \
  --network hermes-runtime \
  -v hermes-analyzer-data:/opt/data \
  -p 127.0.0.1:8643:8643 \
  nousresearch/hermes-agent:latest sleep infinity

# 4. 写入共享密钥并启动 bridge
docker exec hermes-devops-analyzer sh -c \
  'echo "ANALYZE_BRIDGE_TOKEN=<与 deploy/.env 的 HERMES_AUTH_TOKEN 一致>" > /opt/data/analyze_bridge.env && chmod 600 /opt/data/analyze_bridge.env'
docker exec --user hermes hermes-devops-analyzer bash /opt/data/bin/start-analyze-bridge

# 5. 验证
curl -fsS http://127.0.0.1:8643/health
```

然后在 `deploy/.env` 配置 Runtime 侧(worker 与 analyzer 同在 `hermes-runtime`
网络,按容器名解析):

```bash
HERMES_ENDPOINT=http://hermes-devops-analyzer:8643/analyze
HERMES_AUTH_TOKEN=<同上>
HERMES_TIMEOUT_SEC=180   # 覆盖 -z 冷启动(实测 76s)
```

最后按 `deploy/README.md` 升级路径重建并重启 worker(`build` 后
`up -d --no-deps trigger worker`)。

## 运维

- 日志:`docker exec hermes-devops-analyzer tail -f /opt/data/logs/analyze-bridge-8643.log`
  (含每次分析的 token/成本 usage 报告)。
- 重启 bridge:重复执行 `start-analyze-bridge` 即可(幂等)。
- Analyzer 未配置/宕机时 Runtime 行为不变:verdict 由规则引擎保底,decisions
  表只有 rule 行。
