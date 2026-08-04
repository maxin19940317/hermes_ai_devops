#!/bin/bash
# generate-certs.sh — 生成 mTLS 所需的 CA、Server、Client 证书(CLAUDE.md §12 Phase 3)。
# 用法:
#   ./generate-certs.sh [client_id1] [client_id2] ...
#   不传 client_id 时生成一个默认 client 证书(CN=client-default)。
# 产物:
#   deploy/certs/ca-cert.pem        CA 证书(分发给所有节点)
#   deploy/certs/ca-key.pem         CA 私钥(仅 Runtime 保留,签名用)
#   deploy/certs/server-cert.pem    Runtime 服务端证书(CN=hermes-runtime)
#   deploy/certs/server-key.pem     Runtime 服务端私钥
#   deploy/certs/client-{id}.pem    每个 Client 的证书 + 私钥合体(Agent 单文件加载)
#                                   CN 与 client_id 一致,Runtime 凭 CN 确认身份
set -euo pipefail

CERTS_DIR="${CERTS_DIR:-deploy/certs}"
mkdir -p "$CERTS_DIR"

ORG="${CERT_ORG:-hermes-devops}"
VALIDITY="${CERT_VALIDITY_DAYS:-3650}"
# 服务端证书的 SAN:Go 1.15+ 忽略 CN 做主机名校验,没有 SAN 的证书客户端
# 一律拒连(x509: certificate relies on legacy Common Name field)。
# 默认覆盖 Agent 实际连接的 LAN 地址与服务器本机回环(排障 curl 用);
# 部署地址变化时用 CERT_SERVER_SAN 覆盖,逗号分隔的 OpenSSL SAN 列表。
SERVER_SAN="${CERT_SERVER_SAN:-IP:10.88.118.251,IP:127.0.0.1,DNS:hermes-runtime,DNS:localhost}"

if [ $# -eq 0 ]; then
	set -- "client-default"
fi
CLIENTS=("$@")

echo "=== 1/4 CA ==="
if [ -f "$CERTS_DIR/ca-key.pem" ] && [ -f "$CERTS_DIR/ca-cert.pem" ]; then
	echo "已存在,复用(重签 CA 会使全部已发证书作废)"
else
	openssl req -x509 -newkey rsa:4096 -days "$VALIDITY" -nodes \
		-keyout "$CERTS_DIR/ca-key.pem" -out "$CERTS_DIR/ca-cert.pem" \
		-subj "/O=${ORG}/CN=hermes-devops-ca" 2>/dev/null
fi

if [ -f "$CERTS_DIR/server-cert.pem" ] && [ -f "$CERTS_DIR/server-key.pem" ]; then
	echo "=== 2/4 Server: 已存在,复用(SAN 变更需先删除再重跑) ==="
else
	echo "=== 2/4 Server (hermes-runtime, SAN: ${SERVER_SAN}) ==="
	openssl req -newkey rsa:4096 -nodes \
		-keyout "$CERTS_DIR/server-key.pem" -out "$CERTS_DIR/server.csr" \
		-subj "/O=${ORG}/CN=hermes-runtime" 2>/dev/null
	# 注:openssl x509 的 -addext 是 OpenSSL 3.0 才有的;1.1.1 用 -extfile。
	printf 'subjectAltName=%s\n' "$SERVER_SAN" > "$CERTS_DIR/server-ext.cnf"
	openssl x509 -req -in "$CERTS_DIR/server.csr" -CA "$CERTS_DIR/ca-cert.pem" -CAkey "$CERTS_DIR/ca-key.pem" \
		-CAcreateserial -out "$CERTS_DIR/server-cert.pem" -days "$VALIDITY" \
		-extfile "$CERTS_DIR/server-ext.cnf" 2>/dev/null
	rm -f "$CERTS_DIR/server.csr" "$CERTS_DIR/server-ext.cnf"
fi

for client_id in "${CLIENTS[@]}"; do
	echo "=== 3/4 Client: ${client_id} ==="
	openssl req -newkey rsa:4096 -nodes \
		-keyout "$CERTS_DIR/client-${client_id}-key.pem" \
		-out "$CERTS_DIR/client-${client_id}.csr" \
		-subj "/O=${ORG}/CN=${client_id}" 2>/dev/null
	openssl x509 -req -in "$CERTS_DIR/client-${client_id}.csr" \
		-CA "$CERTS_DIR/ca-cert.pem" -CAkey "$CERTS_DIR/ca-key.pem" \
		-CAcreateserial -out "$CERTS_DIR/client-${client_id}-cert.pem" \
		-days "$VALIDITY" 2>/dev/null
	rm -f "$CERTS_DIR/client-${client_id}.csr"

	# Agent 单文件加载:cert + key 合并
	cat "$CERTS_DIR/client-${client_id}-cert.pem" "$CERTS_DIR/client-${client_id}-key.pem" \
		> "$CERTS_DIR/client-${client_id}.pem"
	rm -f "$CERTS_DIR/client-${client_id}-key.pem"
done

echo "=== 4/4 Permissions ==="
# pem 属主可能按需拆给容器用户(如 server-key 属容器 uid),chmod 失败不致命。
chmod 600 "$CERTS_DIR/"*-key.pem "$CERTS_DIR/ca-key.pem" 2>/dev/null || true
chmod 644 "$CERTS_DIR/ca-cert.pem" 2>/dev/null || true

echo "Done."
echo "  CA:        $CERTS_DIR/ca-cert.pem  (分发给所有节点)"
echo "  CA key:    $CERTS_DIR/ca-key.pem   (Runtime 保留)"
echo "  Server:    $CERTS_DIR/server-cert.pem + server-key.pem"
for client_id in "${CLIENTS[@]}"; do
	echo "  Client:    $CERTS_DIR/client-${client_id}.pem  → 复制到 Windows Agent"
done
