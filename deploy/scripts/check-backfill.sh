#!/bin/sh
# check-backfill.sh — 排行榜回填链路健康检查(2026-08-10)
#
# 检测 worker → hermes-rocklin:8646 (workflow_bridge) 的排行榜回填链路是否完好。
# 两个前置条件任一缺失都会静默丢回填(worker 只记日志,不阻断主链路):
#   1. hermes-rocklin 容器挂在 hermes-runtime 网络(容器重建会脱网)
#   2. workflow_bridge 进程在 rocklin 内运行
#
# 用法:
#   deploy/scripts/check-backfill.sh           # 只检查,不修复(CI/巡检)
#   deploy/scripts/check-backfill.sh --fix     # 检查 + 尝试自动修复(运维)
#   deploy/scripts/check-backfill.sh --count   # 只打印排行榜 devops run 数
#
# 退出码:0 = 链路完好;1 = 存在缺失(--fix 已尝试修复但仍有问题)
set -u

WORKER=hermes-runtime-worker-1
ROCKLIN=hermes-rocklin
BRIDGE_URL="http://hermes-rocklin:8646"
BRIDGE_DB="/opt/data/profiles/workflow_runtime/workflow-runtime.db"
FIX=0
COUNT_ONLY=0

for arg in "$@"; do
  case "$arg" in
    --fix) FIX=1 ;;
    --count) COUNT_ONLY=1 ;;
  esac
done

have_docker() { command -v docker >/dev/null 2>&1; }

if ! have_docker; then
  echo "ERROR: docker not available" >&2
  exit 1
fi

if [ "$COUNT_ONLY" = 1 ]; then
  docker exec "$ROCKLIN" bash -c 'PATH=/opt/hermes/.venv/bin:$PATH python3 /dev/stdin <<"PYEOF"
import sqlite3
n = sqlite3.connect("/opt/data/profiles/workflow_runtime/workflow-runtime.db").execute(
    "SELECT COUNT(*) FROM workflow_runs WHERE run_id LIKE ?",
    ("wr-devops-%",),
).fetchone()[0]
print("leaderboard devops runs:", n)
PYEOF'
  exit 0
fi

rc=0

# ---- 检查 1:worker 能解析并访问 bridge ----
if docker exec "$WORKER" wget -q -T 5 -O- "$BRIDGE_URL/health" 2>/dev/null | grep -q '"status": *"ok"'; then
  echo "OK   bridge reachable: $BRIDGE_URL/health"
else
  echo "FAIL bridge unreachable from worker (network attach lost or bridge not running)"
  rc=1
  if [ "$FIX" = 1 ]; then
    # 修复 1a:重挂网络
    if docker network connect hermes-runtime "$ROCKLIN" 2>/dev/null; then
      echo "  FIX  re-attached hermes-rocklin to hermes-runtime network"
    else
      echo "  WARN network connect failed (may already be attached): $(docker network inspect hermes-runtime --format '{{json .Containers}}' 2>/dev/null | head -c 200)"
    fi
    # 修复 1b:重启 bridge(幂等)
    if docker exec "$ROCKLIN" bash /opt/data/bin/start-workflow-bridge >/dev/null 2>&1; then
      echo "  FIX  workflow_bridge started"
    else
      echo "  WARN start-workflow-bridge failed; is /opt/data/bin/workflow_bridge.py present?"
    fi
    # 复检
    if docker exec "$WORKER" wget -q -T 5 -O- "$BRIDGE_URL/health" 2>/dev/null | grep -q '"status": *"ok"'; then
      echo "  OK   bridge reachable after fix"
      rc=0
    else
      echo "  FAIL still unreachable after fix" >&2
    fi
  fi
fi

# ---- 检查 2:worker 最近有无 DNS 解析错误 ----
# 用 5 分钟窗口判断当前健康:1h 窗口会包含历史修复前的错误(误报)。
recent_errs=$(docker logs "$WORKER" --since 5m 2>&1 | grep -c "server misbehaving" || true)
if [ "$recent_errs" -gt 0 ]; then
  echo "WARN $recent_errs 'server misbehaving' errors in worker log (last 5m)"
  [ "$rc" = 0 ] && rc=1
fi

# ---- 检查 3:排行榜库里有 devops 记录 ----
devops_runs=$(docker exec "$ROCKLIN" bash -c 'PATH=/opt/hermes/.venv/bin:$PATH python3 /dev/stdin <<"PYEOF"
import sqlite3
db_path = "/opt/data/profiles/workflow_runtime/workflow-runtime.db"
try:
    n = sqlite3.connect(db_path).execute(
        "SELECT COUNT(*) FROM workflow_runs WHERE run_id LIKE ?",
        ("wr-devops-%",),
    ).fetchone()[0]
    print(n)
except Exception:
    print(-1)
PYEOF' 2>/dev/null)
if [ "$devops_runs" = "-1" ] || [ -z "$devops_runs" ]; then
  echo "FAIL cannot read leaderboard db ($devops_runs)"
  rc=1
else
  echo "OK   leaderboard devops runs: $devops_runs"
fi

[ "$rc" = 0 ] && echo "PASS backfill chain healthy" || echo "DEGRADED backfill chain has issues" >&2
exit $rc
