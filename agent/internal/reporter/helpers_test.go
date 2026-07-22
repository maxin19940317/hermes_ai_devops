package reporter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"hermes-devops/agent/internal/executor"
	"hermes-devops/agent/internal/store"
)

// openTempStore 在 t.TempDir() 下打开临时库。
func openTempStore(t *testing.T) *store.Store {
	t.Helper()
	return openStoreAt(t, filepath.Join(t.TempDir(), "agent.db"))
}

func openStoreAt(t *testing.T, path string) *store.Store {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedTask 插入一个 QUEUED 任务,dispatch_json 带 device_serial。
func seedTask(t *testing.T, s *store.Store, taskID, key, serial string) store.Task {
	t.Helper()
	dispatch := map[string]any{"task_id": taskID, "device_serial": serial}
	raw, err := json.Marshal(dispatch)
	if err != nil {
		t.Fatalf("marshal dispatch: %v", err)
	}
	task := store.Task{
		TaskID:         taskID,
		IdempotencyKey: key,
		Attempt:        1,
		DispatchJSON:   string(raw),
		OutDir:         t.TempDir(),
	}
	if err := s.CreateTask(context.Background(), task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return task
}

// driveTerminal 沿流水线把任务推到指定终态(顺带产生 4 条事件)。
func driveTerminal(t *testing.T, s *store.Store, taskID string, final store.State) {
	t.Helper()
	ctx := context.Background()
	steps := [][2]store.State{
		{store.StateQueued, store.StatePreparing},
		{store.StatePreparing, store.StateRunning},
		{store.StateRunning, store.StateCollecting},
		{store.StateCollecting, final},
	}
	for _, st := range steps {
		if _, err := s.Transition(ctx, taskID, st[0], st[1], ""); err != nil {
			t.Fatalf("Transition %s->%s: %v", st[0], st[1], err)
		}
	}
}

// writeSummary 往 out_dir 写 executor 的 run-summary.json。
func writeSummary(t *testing.T, sum executor.Summary) {
	t.Helper()
	data, err := json.Marshal(sum)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sum.OutDir, "run-summary.json"), data, 0o644); err != nil {
		t.Fatalf("write run-summary.json: %v", err)
	}
}

// writeDeviceResult 往 out_dir/device/results/result.json 写设备端结果。
func writeDeviceResult(t *testing.T, outDir, body string) {
	t.Helper()
	p := deviceResultPath(outDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write device result.json: %v", err)
	}
}

const testSHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
