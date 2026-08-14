#!/usr/bin/env bash
# devops-cli — 简化版设备测试 CLI(封装 cmdapi 的 mTLS + Bearer)。
#
# 用法:
#   devops-cli.sh devices [all]
#   devops-cli.sh status
#   devops-cli.sh test <variant>
#   devops-cli.sh runs [n]
#   devops-cli.sh result <workflow_id>
#   devops-cli.sh metrics <variant>
#   devops-cli.sh artifacts <variant>
#   devops-cli.sh cancel <workflow_id>
#   devops-cli.sh quarantine <device_id>
#
# 证书/token 自动从仓库默认位置读取,无需每次带参数。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# 从 deploy/.env 读 CMD_API_TOKEN(不 export,避免泄漏到子进程)。
TOKEN="$(grep -E '^CMD_API_TOKEN=' "$REPO_ROOT/deploy/.env" | head -1 | cut -d= -f2-)"
CA_CERT="$REPO_ROOT/deploy/certs/ca-cert.pem"
CLIENT_CERT="$REPO_ROOT/deploy/certs/client-windows-client-01.pem"
ENDPOINT="${DEVOPS_CLI_ENDPOINT:-https://127.0.0.1:18091/api/v1/cmd}"

usage() {
  sed -n '2,17p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
  exit 1
}

cmd="$1"; shift || true
case "$cmd" in
  devices|status|test|rerun|cancel|quarantine|unquarantine|runs|result|metrics|artifacts)
    ;;
  help|-h|--help|"")
    usage
    ;;
  *)
    echo "未知命令: $cmd" >&2
    usage
    ;;
esac

# 构造 JSON 参数。
args=("$@")
json_args="[]"
if [[ ${#args[@]} -gt 0 ]]; then
  json_args=$(printf '%s\n' "${args[@]}" | python3 -c 'import json,sys; print(json.dumps([l.strip() for l in sys.stdin if l.strip()]))')
fi

payload="{\"command\":\"$cmd\",\"args\":$json_args}"
curl -sk --cacert "$CA_CERT" --cert "$CLIENT_CERT" \
  -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d "$payload" "$ENDPOINT" | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d.get("reply", d))'
