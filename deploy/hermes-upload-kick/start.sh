#!/usr/bin/env bash
# 启动 hermes-upload-kick 服务(Hermes 上传 tar → MinIO → kick)。
# 用法: bash deploy/hermes-upload-kick/start.sh [port]
set -euo pipefail
cd "$(dirname "$0")/../.."
PORT="${1:-18686}"
LOG=/tmp/hermes-upload-kick.log

set -a
# shellcheck disable=SC1091
. deploy/.env
set +a

if ss -tln | grep -q ":${PORT} "; then
  echo "already listening on :${PORT}"
  exit 0
fi

nohup python3 deploy/hermes-upload-kick/server.py --port "$PORT" >> "$LOG" 2>&1 &
echo "started pid=$! log=$LOG"
