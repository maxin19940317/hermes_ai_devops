// Package testtemporal 提供测试用 Temporal dev server 拉起助手
// (temporal CLI 单二进制 + SQLite,无需 Docker)。仅测试引用。
package testtemporal

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
)

// FreePort 返回一个当前空闲的 TCP 端口。
func FreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// StartDevServer 拉起 temporal dev server 并等待就绪,返回 gRPC 地址。
// temporal CLI 不在 PATH 时跳过测试。进程随测试清理。
func StartDevServer(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("temporal"); err != nil {
		t.Skip("temporal CLI 不在 PATH,跳过(安装: temporal.download/cli)")
	}
	grpcPort := FreePort(t)
	cmd := exec.Command("temporal", "server", "start-dev",
		"--headless", "--log-level", "error",
		"--ip", "127.0.0.1",
		"--port", fmt.Sprint(grpcPort),
		"--http-port", fmt.Sprint(FreePort(t)),
		"--metrics-port", fmt.Sprint(FreePort(t)),
		"--db-filename", filepath.Join(t.TempDir(), "temporal.db"),
	)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start dev server: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	addr := fmt.Sprintf("127.0.0.1:%d", grpcPort)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		c, err := client.Dial(client.Options{HostPort: addr}) // Dial 自带健康检查
		if err == nil {
			c.Close()
			return addr
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("dev server 30s 内未就绪")
	return ""
}
