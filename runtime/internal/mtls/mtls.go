// Package mTLS 提供 Phase 3 mTLS 配置加载(CLAUDE.md §12)。
// Runtime 服务端:加载 CA cert + server cert/key → tls.Config{ClientAuth: RequireAndVerifyClientCert}
// Agent 客户端:加载 CA cert + client cert/key → 带双向认证的 http.Transport。
package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
)

// ServerConfig 从文件路径加载 Runtime TLS 服务端配置。
// caCertFile: CA 证书,用于验证客户端证书链
// certFile/keyFile: 服务端证书/私钥,用于证明自己身份
// 返回的 tls.Config 要求客户端证书 (RequireAndVerifyClientCert)
func ServerConfig(caCertFile, certFile, keyFile string) (*tls.Config, error) {
	if caCertFile == "" || certFile == "" || keyFile == "" {
		return nil, nil // mTLS not configured
	}
	caPEM, err := os.ReadFile(caCertFile)
	if err != nil {
		return nil, fmt.Errorf("mtls server: read ca cert %s: %w", caCertFile, err)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("mtls server: load cert pair %s/%s: %w", certFile, keyFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("mtls server: no certificates found in %s", caCertFile)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// ClientTransport 从文件路径构建带 mTLS 客户端认证的 http.Transport。
// caCertFile: CA 证书(验证服务端)
// certFile: 客户端证书(agent client-{id}.pem 合体文件)
// 返回 nil = mTLS 未配置(降级为普通 HTTP,兼容旧部署)
func ClientTransport(caCertFile, certFile string) (*http.Transport, error) {
	if caCertFile == "" || certFile == "" {
		return nil, nil
	}
	caPEM, err := os.ReadFile(caCertFile)
	if err != nil {
		return nil, fmt.Errorf("mtls client: read ca cert %s: %w", caCertFile, err)
	}
	cert, err := tls.LoadX509KeyPair(certFile, certFile)
	if err != nil {
		return nil, fmt.Errorf("mtls client: load client cert %s: %w", certFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("mtls client: no certificates found in %s", caCertFile)
	}
	return &http.Transport{
		TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
			MinVersion:   tls.VersionTLS13,
		},
	}, nil
}
