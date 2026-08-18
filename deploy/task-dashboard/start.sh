#!/usr/bin/env bash
# 启动 task-dashboard 服务(任务执行实时面板)。
# 用法: bash deploy/task-dashboard/start.sh [port]
set -euo pipefail
cd "$(dirname "$0")/../.."
PORT="${1:-18687}"
LOG=/tmp/task-dashboard.log

# 从 deploy/.env 提取 postgres 凭据(不能 source 整个 .env——FEISHU_SENDERS 等
# 含 JSON 花括号会让 bash 报错)。
getenv() { grep -E "^$1=" deploy/.env | head -1 | cut -d= -f2-; }

# postgres 无宿主端口映射(compose 内部网络);经 hermes-runtime 网络访问
# 容器 IP(172.31.240.x)。动态解析,避免写死。
PG_IP="$(docker inspect hermes-runtime-postgres-1 --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' 2>/dev/null || echo 172.31.240.3)"
export DASHBOARD_PORT="$PORT"
export DASHBOARD_DB_HOST="${DASHBOARD_DB_HOST:-$PG_IP}"
export DASHBOARD_DB_PORT="${DASHBOARD_DB_PORT:-5432}"
export DASHBOARD_DB_USER="${DASHBOARD_DB_USER:-$(getenv RUNTIME_DB_USER)}"
export DASHBOARD_DB_USER="${DASHBOARD_DB_USER:-hermes_runtime}"
PWD_ENV="$(getenv RUNTIME_DB_PASSWORD)"
export DASHBOARD_DB_PASSWORD="${DASHBOARD_DB_PASSWORD:-$PWD_ENV}"
if [ -z "$DASHBOARD_DB_PASSWORD" ]; then
  echo "error: RUNTIME_DB_PASSWORD 未在 deploy/.env" >&2
  exit 1
fi
export DASHBOARD_DB_NAME="${DASHBOARD_DB_NAME:-hermes_runtime}"

if ss -tln | grep -q ":${PORT} "; then
  echo "already listening on :${PORT}"
  exit 0
fi

nohup python3 deploy/task-dashboard/server.py >> "$LOG" 2>&1 &
echo "started pid=$! log=$LOG"
