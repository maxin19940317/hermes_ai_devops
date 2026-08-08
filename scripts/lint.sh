#!/usr/bin/env bash
# lint.sh — Go 代码质量门禁(2026-08-08 Review P4):gofmt + go vet + go test。
# 用法:./scripts/lint.sh   (CI 或本地提交前跑;任一失败非零退出)
# 门禁规则:
#   - gofmt -l 非空 → 失败(格式未通过)
#   - go vet 有输出 → 失败
#   - go test 失败 → 失败
set -euo pipefail
cd "$(dirname "$0")/.."

echo "== gofmt =="
if ! command -v gofmt >/dev/null 2>&1; then echo "gofmt 不可用"; exit 1; fi
unformatted=$(gofmt -l runtime/ agent/ 2>/dev/null || true)
if [ -n "$unformatted" ]; then
  echo "gofmt 未通过,以下文件需要格式化:"
  echo "$unformatted"
  echo "修复: gofmt -w \$(gofmt -l runtime/ agent/)"
  exit 1
fi
echo "gofmt OK"

echo "== go vet =="
(cd runtime && go vet ./... 2>&1 | tee /tmp/lint-vet-runtime.log; exit ${PIPESTATUS[0]})
(cd agent && go vet ./... 2>&1 | tee /tmp/lint-vet-agent.log; exit ${PIPESTATUS[0]})
echo "go vet OK"

echo "== go test =="
(cd runtime && go test ./... 2>&1 | tail -3)
(cd agent && go test ./... 2>&1 | tail -3)
echo "go test OK"

echo "ALL LINT CHECKS PASSED"
