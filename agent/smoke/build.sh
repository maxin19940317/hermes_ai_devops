#!/usr/bin/env bash
# 构建 hermes-smoke 最小测试包(agent-cli 实机验证用,CLAUDE.md §12 Phase 1.3)。
# 用法: ./build.sh [ok|timeout|fail]
# 输出: agent/dist/smoke-pkg-<variant>.tar.gz,并打印整包 sha256;
#       若本机有 go,构建后用 smoke/check 做无设备校验(解压+Schema+逐文件 sha256)。
set -euo pipefail
cd "$(dirname "$0")"

VARIANT="${1:-ok}"
case "$VARIANT" in
  ok)      ARGS='[]';          TIMEOUT=60 ;;
  timeout) ARGS='["timeout"]'; TIMEOUT=5 ;;
  fail)    ARGS='["fail"]';    TIMEOUT=60 ;;
  *) echo "usage: $0 [ok|timeout|fail]" >&2; exit 1 ;;
esac

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
mkdir -p "$STAGE/data"
cp run.sh "$STAGE/run.sh"
cp data/hello.txt "$STAGE/data/hello.txt"

RUN_SHA=$(sha256sum "$STAGE/run.sh" | cut -d' ' -f1)
HELLO_SHA=$(sha256sum "$STAGE/data/hello.txt" | cut -d' ' -f1)

cat > "$STAGE/manifest.yaml" <<EOF
manifest_version: 1
artifact:
  project: hermes-smoke
  commit: deadbeef
  pipeline_id: 1
  platform: aarch64_Android_smoke
  build_type: Release
requirements:
  os: android
  abi: arm64-v8a
  min_free_storage_mb: 16
deploy:
  workdir: /data/local/tmp/hermes-smoke
  files:
    - { src: run.sh, dst: run.sh, mode: "0755", sha256: "$RUN_SHA" }
    - { src: data/hello.txt, dst: data/hello.txt, mode: "0644", sha256: "$HELLO_SHA" }
  env:
    SMOKE_WORKDIR: "{workdir}"
test:
  entry: ./run.sh
  args: $ARGS
  timeout_sec: $TIMEOUT
  success:
    exit_code: 0
    require_files: [results/result.json]
  failure_signatures:
    - { id: smoke_fail_marker, where: stdout, pattern: "SMOKE-FAIL", classify: UNKNOWN }
collect:
  - results/*
  - logs/*.log
cleanup:
  remove_workdir: true
  keep_on_failure: true
EOF

# 与 gen_manifest.py 产出的真实包保持同样的顶层布局(manifest.yaml + files.sha256 + 载荷)
(cd "$STAGE" && sha256sum run.sh data/hello.txt > files.sha256)

mkdir -p ../dist
OUT="../dist/smoke-pkg-$VARIANT.tar.gz"
tar -czf "$OUT" -C "$STAGE" manifest.yaml files.sha256 run.sh data

PKG_SHA=$(sha256sum "$OUT" | cut -d' ' -f1)
echo "package: $OUT"
echo "sha256:  $PKG_SHA"

if command -v go >/dev/null 2>&1; then
  (cd .. && go run ./smoke/check "dist/smoke-pkg-$VARIANT.tar.gz")
else
  echo "warn: go 不在 PATH,跳过无设备校验(agent-cli 运行时仍会校验)" >&2
fi
