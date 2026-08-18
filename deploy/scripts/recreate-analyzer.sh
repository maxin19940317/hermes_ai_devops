#!/usr/bin/env bash
# recreate-analyzer.sh — 重建 hermes-devops-analyzer 容器并拉起两个 MCP bridge。
#
# 背景(2026-08-18):analyzer 容器承载两个 MCP bridge——
#   :8645 mcp_bridge.env        (tobias_pm 用,无提交人身份)
#   :8646 mcp_bridge_gene.env   (gene_pm 用,SUBMITTER_OPEN_ID=gene)
# 容器曾因 8646 未做端口映射导致 Hermes 连接失败;且 bridge 是手动 nohup 启动,
# 容器重建后不会自动拉起。本脚本把"重建容器(含 8645/8646 映射)+ 启动两个
# bridge"固化为一条命令。
#
# 用法:
#   bash deploy/scripts/recreate-analyzer.sh [--force]
#     --force  强制重建(即使容器已存在);缺省仅在容器不存在时重建
#
# 前提:analyzer 容器为独立运行(非本项目 compose 管理),挂载 hermes-analyzer-data。

set -euo pipefail

NAME="hermes-devops-analyzer"
IMAGE="nousresearch/hermes-agent:latest"
NETWORK="hermes-runtime"
VOLUME="hermes-analyzer-data"
FORCE=0
[[ "${1:-}" == "--force" ]] && FORCE=1

echo "==> 检查容器 $NAME ..."
if docker inspect "$NAME" >/dev/null 2>&1; then
  if [[ $FORCE -eq 0 ]]; then
    echo "    容器已存在(跳过重建;要强制重建加 --force)。"
    # 容器存在但仍可能缺 bridge,继续走到启动/验证步骤。
  else
    echo "    --force:停止并删除旧容器。"
    docker stop "$NAME" >/dev/null
    docker rm "$NAME" >/dev/null
  fi
fi

if ! docker inspect "$NAME" >/dev/null 2>&1; then
  echo "==> 重建容器 $NAME(端口 8643/8645/8646)..."
  docker run -d --name "$NAME" \
    --restart unless-stopped \
    --network "$NETWORK" \
    -v "$VOLUME":/opt/data \
    -p 127.0.0.1:8643:8643 \
    -p 0.0.0.0:8645:8645 \
    -p 0.0.0.0:8646:8646 \
    -e PYTHONUNBUFFERED=1 \
    -e HERMES_HOME=/opt/data \
    -e HERMES_WRITE_SAFE_ROOT=/opt/data \
    "$IMAGE" \
    sleep infinity
  echo "    容器已创建。"
else
  echo "    容器已存在,使用现有容器。"
fi

echo "==> 等待容器就绪..."
for i in $(seq 1 15); do
  if docker exec "$NAME" true >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

echo "==> 启动 analyze bridge(8643,翻译/分析/规划)..."
# HERMES_ENDPOINT 指向 hermes-devops-analyzer:8643/analyze;容器重建后必须
# 一并拉起,否则飞书自然语言指令降级为"翻译服务暂时不可用"。
docker exec "$NAME" sh -c 'export HOME=/opt/data; bash /opt/data/bin/start-analyze-bridge 2>&1 | tail -1'

echo "==> 启动 MCP bridge(8645 + 8646)..."
# 幂等:按 pid 文件判断进程是否存活;存活则跳过(容器重建后 pid 失效会重新拉起)。
docker exec "$NAME" sh -c '
  cd /opt/data/bin
  for envf in mcp_bridge.env mcp_bridge_gene.env; do
    logf="/opt/data/logs/$(basename "$envf" .env).log"
    pidf="/opt/data/logs/$(basename "$envf" .env).pid"
    port=$(grep -E "^MCP_BRIDGE_PORT=" "/opt/data/bin/$envf" 2>/dev/null | cut -d= -f2)
    port="${port:-?}"
    alive=0
    if [ -f "$pidf" ] && kill -0 "$(cat "$pidf")" 2>/dev/null; then
      alive=1
    fi
    if [ "$alive" -eq 1 ]; then
      echo "    $envf:进程 $(cat "$pidf") 存活,跳过"
      continue
    fi
    # shellcheck disable=SC1090
    set -a; . "/opt/data/bin/$envf" 2>/dev/null || true; set +a
    nohup /opt/hermes/.venv/bin/python mcp_bridge.py >>"$logf" 2>&1 &
    echo $! > "$pidf"
    echo "    启动 $envf: pid=$! port=$port"
  done
'

echo "==> 验证端口映射..."
sleep 3
docker ps --filter "name=$NAME" --format '    {{.Names}} {{.Status}} {{.Ports}}'

echo "==> 验证 MCP 连通(宿主机视角)..."
for p in 8645 8646; do
  code=$(curl -s -m 4 -o /dev/null -w '%{http_code}' \
    -X POST "http://127.0.0.1:$p/mcp" \
    -H "Content-Type: application/json" \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"probe","version":"1"}}}' || true)
  # 421(Invalid Host)或 406 都是 MCP server 活着(仅 Host 校验拒绝)的正常响应。
  echo "    :$p → HTTP $code($code=000 才是异常)"
done

echo "==> 完成。若 gene_pm/tobias_pm 的 hermes_devops 仍连接失败,在 hermes-rocklin 重启对应 gateway:"
echo "    docker exec hermes-rocklin /command/s6-svc -r /run/service/gateway-gene_pm"
echo "    docker exec hermes-rocklin /command/s6-svc -r /run/service/gateway-tobias_pm"
