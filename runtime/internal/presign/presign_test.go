package presign

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestEnabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"全配齐", Config{Endpoint: "minio:9000", AccessKey: "a", SecretKey: "b"}, true},
		{"缺 endpoint", Config{AccessKey: "a", SecretKey: "b"}, false},
		{"缺 access key", Config{Endpoint: "minio:9000", SecretKey: "b"}, false},
		{"缺 secret key", Config{Endpoint: "minio:9000", AccessKey: "a"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.Enabled(); got != tc.want {
				t.Errorf("Enabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// 未启用时 NewSigner 返回 (nil, nil):调用方据此判"优雅降级",不是错误(§3.7)。
func TestNewSignerDisabledReturnsNil(t *testing.T) {
	s, err := NewSigner(Config{})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if s != nil {
		t.Errorf("signer = %v, want nil", s)
	}
}

// 签名是纯离线操作:用 public endpoint 构造(签名覆盖 Host,事后改写 URL 会失效),
// 即使该 host 在集群内不可达也能签出来。
func TestPutURLUsesPublicEndpoint(t *testing.T) {
	s, err := NewSigner(Config{
		Endpoint: "minio:9000", PublicEndpoint: "http://10.88.118.251:9000",
		AccessKey: "ak", SecretKey: "sk", Bucket: "hermes-evidence", TTL: time.Hour,
	})
	if err != nil || s == nil {
		t.Fatalf("NewSigner: %v (signer=%v)", err, s)
	}
	u, exp, err := s.PutURL(context.Background(), "runs/t1/result.json")
	if err != nil {
		t.Fatalf("PutURL: %v", err)
	}
	if !strings.Contains(u, "10.88.118.251:9000") {
		t.Errorf("URL 应含 public host, got %q", u)
	}
	if !strings.Contains(u, "runs/t1/result.json") {
		t.Errorf("URL 应含 object key, got %q", u)
	}
	if exp.IsZero() || exp.Before(time.Now()) {
		t.Errorf("expiresAt = %v, 应为将来时刻", exp)
	}
}

// PublicEndpoint 为空时退回 Endpoint(仅同 host 可达时正确,与改造前一致)。
func TestPutURLFallsBackToEndpoint(t *testing.T) {
	s, err := NewSigner(Config{
		Endpoint: "minio:9000", AccessKey: "ak", SecretKey: "sk",
		Bucket: "hermes-evidence", TTL: time.Hour,
	})
	if err != nil || s == nil {
		t.Fatalf("NewSigner: %v", err)
	}
	u, _, err := s.PutURL(context.Background(), "runs/t1/a.log")
	if err != nil {
		t.Fatalf("PutURL: %v", err)
	}
	if !strings.Contains(u, "minio:9000") {
		t.Errorf("URL 应含 endpoint host, got %q", u)
	}
}
