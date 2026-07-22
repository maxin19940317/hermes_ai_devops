package store

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

// PG 集成测试基建:优先用 TEST_DATABASE_URL 指向的外部库(服务器/CI);
// 未设置时启动 embedded-postgres(真实 PG 15,二进制缓存在
// ~/.embedded-postgres-go,数据目录在临时目录)。两者都不可用则 skip。

func TestMain(m *testing.M) {
	os.Exit(testMain(m))
}

func testMain(m *testing.M) int {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		stop, dsn, err := startEmbeddedPG()
		if err != nil {
			fmt.Fprintf(os.Stderr, "embedded postgres 不可用,PG 集成测试将 skip: %v\n", err)
		} else {
			defer stop()
			os.Setenv("TEST_DATABASE_URL", dsn)
		}
	}
	return m.Run()
}

func startEmbeddedPG() (stop func(), dsn string, err error) {
	port, err := freePort()
	if err != nil {
		return nil, "", err
	}
	dataDir, err := os.MkdirTemp("", "hermes-pgtest-*")
	if err != nil {
		return nil, "", err
	}
	epg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V15).
		Port(uint32(port)).
		DataPath(filepath.Join(dataDir, "data")).
		StartTimeout(60 * time.Second).
		Logger(io.Discard))
	if err := epg.Start(); err != nil {
		_ = os.RemoveAll(dataDir)
		return nil, "", err
	}
	stop = func() {
		_ = epg.Stop()
		_ = os.RemoveAll(dataDir)
	}
	dsn = fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/postgres?sslmode=disable", port)
	return stop, dsn, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// openTestPG 返回连接测试库的 PGStore,并清空全部表(测试间隔离;
// 测试不并行,串行 TRUNCATE 足够)。无可用库则 skip。
func openTestPG(t *testing.T) *PGStore {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL 未设置且 embedded postgres 不可用,跳过 PG 集成测试")
	}
	s, err := OpenPG(ctx, dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = s.DB.Close() })
	if _, err := s.DB.ExecContext(ctx,
		`TRUNCATE artifacts, clients, devices, device_leases, tasks, task_events, results, decisions CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}
