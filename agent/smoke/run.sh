#!/system/bin/sh
# hermes-smoke: agent-cli 实机验证用最小设备端脚本(CLAUDE.md §12 Phase 1.3 第 1 步)。
# 模式由第一个参数决定(打包期写死在 manifest test.args,见 build.sh):
#   (无)    正常通过:写 results/result.json 并 exit 0
#   timeout 长睡眠,用于验证超时 kill 后仍收集
#   fail    打印失败签名标记并 exit 7,用于验证判据不满足(退出码 2)与 keep_on_failure
set -u

MODE="${1:-ok}"
START="$(date +%s)"

mkdir -p results logs

{
  echo "smoke: mode=$MODE"
  echo "smoke: pwd=$(pwd)"
  echo "smoke: SMOKE_WORKDIR=${SMOKE_WORKDIR:-<unset>}"
  echo "smoke: data/hello.txt -> $(cat data/hello.txt)"
} | tee logs/run.log

# 往 logcat 打标记,便于核对 logcat.txt 确实来自本次运行(logcat -c 后)
log -t hermes-smoke "smoke run mode=$MODE" 2>/dev/null || true

case "$MODE" in
  timeout)
    echo "smoke: sleeping to trigger timeout"
    sleep 3600
    ;;
  fail)
    echo "SMOKE-FAIL: injected failure"  # 命中 failure_signatures 的 smoke_fail_marker
    exit 7
    ;;
esac

DUR=$(( $(date +%s) - START ))
cat > results/result.json <<EOF
{
  "result_version": 1,
  "task_id": "smoke-local",
  "attempt": 1,
  "status": "COMPLETED",
  "exit_code": 0,
  "duration_sec": $DUR,
  "cases": { "total": 1, "passed": 1, "failed": 0, "skipped": 0, "failures": [] },
  "signatures_hit": [],
  "metrics": { "smoke_duration_sec": $DUR }
}
EOF

echo "smoke: PASS"
exit 0
