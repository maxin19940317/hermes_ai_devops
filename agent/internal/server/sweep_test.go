package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"hermes-devops/agent/internal/store"
)

// 任务终态即清 SDK 负载:package.tar.gz 与 package/ 必须删除,
// 上报/恢复依赖的小文件(run-summary.json、device/)必须保留。
func TestPurgeRunPayload(t *testing.T) {
	outDir := t.TempDir()
	payloads := []string{
		filepath.Join(outDir, "package.tar.gz"),
		filepath.Join(outDir, "package", "lib", "big.so"),
	}
	keeps := []string{
		filepath.Join(outDir, "run-summary.json"),
		filepath.Join(outDir, "device", "results", "result.json"),
		filepath.Join(outDir, "stdout.log"),
	}
	for _, p := range append(payloads, keeps...) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s := &Server{}
	s.purgeRunPayload(outDir)

	for _, p := range []string{payloads[0], filepath.Join(outDir, "package")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("负载 %s 应被删除", p)
		}
	}
	for _, p := range keeps {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("小文件 %s 应保留: %v", p, err)
		}
	}
}

func openTempStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// seedRecordedTerminal 造一个"已终态 + 结果已上报"的任务,outDir 为其实际目录。
func seedRecordedTerminal(t *testing.T, st *store.Store, id, outDir string) {
	t.Helper()
	ctx := context.Background()
	task := store.Task{
		TaskID: id, IdempotencyKey: "k:" + id, Attempt: 1,
		DispatchJSON: `{"task_id":"` + id + `"}`, OutDir: outDir,
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
	if _, err := st.Transition(ctx, id, store.StateQueued, store.StateCompleted, ""); err != nil {
		t.Fatalf("Transition %s: %v", id, err)
	}
	if err := st.MarkResultRecorded(ctx, id); err != nil {
		t.Fatalf("MarkResultRecorded %s: %v", id, err)
	}
}

// 保留期清理(2026-08-10):超期且已上报的目录删除;未上报任务、孤儿目录、
// RunsRoot 之外的目录一律不碰。cutoff 取未来时刻,免去回拨 ended_at。
func TestSweepRuns(t *testing.T) {
	ctx := context.Background()
	st := openTempStore(t)
	root := t.TempDir()
	mkdirRun := func(name string) string {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "run-summary.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	recordedDir := mkdirRun("task-recorded")
	seedRecordedTerminal(t, st, "task-recorded", recordedDir)

	unrecordedDir := mkdirRun("task-unrecorded")
	if err := st.CreateTask(ctx, store.Task{
		TaskID: "task-unrecorded", IdempotencyKey: "k:task-unrecorded", Attempt: 1,
		DispatchJSON: `{}`, OutDir: unrecordedDir,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Transition(ctx, "task-unrecorded", store.StateQueued, store.StateFailed, ""); err != nil {
		t.Fatal(err)
	}

	orphanDir := mkdirRun("orphan-no-db-row") // DB 无记录:不删(人工处理)

	outsideDir := t.TempDir() // RunsRoot 之外:防御分支,绝不删
	seedRecordedTerminal(t, st, "task-outside", outsideDir)

	srv := &Server{cfg: Config{Store: st, RunsRoot: root}}
	srv.SweepRuns(ctx, time.Now().Add(time.Hour)) // cutoff 在未来:全部超期

	if _, err := os.Stat(recordedDir); !os.IsNotExist(err) {
		t.Error("已上报超期目录应被删除")
	}
	for name, dir := range map[string]string{
		"未上报任务目录": unrecordedDir, "孤儿目录": orphanDir, "RunsRoot 外目录": outsideDir,
	} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("%s应保留: %v", name, err)
		}
	}
}

func TestIsRunsRootChild(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		dir  string
		want bool
	}{
		{filepath.Join(root, "run-1"), true},
		{root, false},                                    // root 本身不是子目录
		{filepath.Join(root, "a", "b"), false},           // 只认直接子目录
		{filepath.Dir(root), false},                      // 上级
		{t.TempDir(), false},                             // 完全不相关
		{"", false},                                      // 空串
		{filepath.Join(root, "..", "escape"), false},     // 越界
	}
	for _, tc := range cases {
		if got := isRunsRootChild(root, tc.dir); got != tc.want {
			t.Errorf("isRunsRootChild(%q) = %v, want %v", tc.dir, got, tc.want)
		}
	}
}
