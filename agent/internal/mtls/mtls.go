// Package mtls 提供 Agent 侧 mTLS 客户端传输配置(CLAUDE.md §12 Phase 3)。
package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
)

// Transport 从文件路径构建带 mTLS 客户端认证的 http.RoundTripper。
// caFile: CA 证书路径(验证服务端身份)
// certFile: 客户端证书路径(client-{id}.pem 合体文件,含证书+私钥)
// 返回 nil = mTLS 未配置(任一文件路径为空时降级为普通 HTTP)
func Transport(caFile, certFile string) (http.RoundTripper, error) {
	if caFile == "" || certFile == "" {
		return nil, nil
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("mtls agent: read ca %s: %w", caFile, err)
	}
	cert, err := tls.LoadX509KeyPair(certFile, certFile)
	if err != nil {
		return nil, fmt.Errorf("mtls agent: load client cert %s: %w", certFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("mtls agent: no certificates in %s", caFile)
	}
	return &http.Transport{
		TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
			MinVersion:   tls.VersionTLS13,
		},
	}, nil
}
