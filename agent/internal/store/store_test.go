package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"hermes-devops/agent/internal/executor"
)

// openTemp 在 t.TempDir() 下打开一个临时库。
func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mkTask(id, key string) Task {
	return Task{
		TaskID:         id,
		IdempotencyKey: key,
		Attempt:        1,
		DispatchJSON:   `{"task_id":"` + id + `"}`,
		OutDir:         "/tmp/out/" + id,
	}
}

func TestTerminalSetMatchesExecutor(t *testing.T) {
	// store 终态集合必须与 executor.Status 的终态集合一致(§9)
	cases := []struct {
		status   executor.Status
		terminal bool
	}{
		{executor.StatusPreparing, false},
		{executor.StatusDownloading, false},
		{executor.StatusDeploying, false},
		{executor.StatusRunning, false},
		{executor.StatusCollecting, false},
		{executor.StatusCompleted, true},
		{executor.StatusFailed, true},
		{executor.StatusTimeout, true},
		{executor.StatusCanceled, true},
	}
	for _, tc := range cases {
		if got := IsTerminal(State(tc.status)); got != tc.terminal {
			t.Errorf("IsTerminal(%s) = %v, want %v", tc.status, got, tc.terminal)
		}
	}
}

func TestCreateAndGetRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	before := time.Now().UTC().Add(-time.Second)
	if err := s.CreateTask(ctx, mkTask("t1", "wf1:t1:a1")); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State != StateQueued {
		t.Errorf("State = %s, want QUEUED", got.State)
	}
	if got.Attempt != 1 || got.DispatchJSON == "" || got.OutDir != "/tmp/out/t1" {
		t.Errorf("round trip mismatch: %+v", got)
	}
	if got.StartedAt.Before(before) || got.StartedAt.After(time.Now().UTC().Add(time.Second)) {
		t.Errorf("StartedAt %v not near now", got.StartedAt)
	}
	// 毫秒精度往返
	if got.StartedAt != got.StartedAt.Truncate(time.Millisecond) {
		t.Errorf("StartedAt %v lost millisecond precision", got.StartedAt)
	}
	if got.EndedAt != nil {
		t.Errorf("EndedAt = %v, want nil", got.EndedAt)
	}
	if got.ResultRecorded {
		t.Error("ResultRecorded = true, want false")
	}

	if _, err := s.GetTask(ctx, "nope"); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("GetTask missing: err = %v, want ErrTaskNotFound", err)
	}
}

func TestLookupByIdempotencyKey(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	if err := s.CreateTask(ctx, mkTask("t1", "wf1:t1:a1")); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	got, err := s.LookupByIdempotencyKey(ctx, "wf1:t1:a1")
	if err != nil {
		t.Fatalf("LookupByIdempotencyKey: %v", err)
	}
	if got.TaskID != "t1" {
		t.Errorf("TaskID = %s, want t1", got.TaskID)
	}
	if _, err := s.LookupByIdempotencyKey(ctx, "wf1:t9:a1"); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("lookup missing key: err = %v, want ErrTaskNotFound", err)
	}
	// 同幂等键重复插入必须失败(派单侧据此返回既有状态)
	if err := s.CreateTask(ctx, mkTask("t2", "wf1:t1:a1")); err == nil {
		t.Error("duplicate idempotency_key accepted, want unique violation")
	}
}

// driveTransitions 顺序执行迁移并校验返回的 seq。
func driveTransitions(t *testing.T, s *Store, taskID string, steps []struct{ from, to State }) []int64 {
	t.Helper()
	ctx := context.Background()
	seqs := make([]int64, 0, len(steps))
	for _, st := range steps {
		seq, err := s.Transition(ctx, taskID, st.from, st.to, "")
		if err != nil {
			t.Fatalf("Transition %s -> %s: %v", st.from, st.to, err)
		}
		seqs = append(seqs, seq)
	}
	return seqs
}

func TestTransitionSeqMonotonicPerTask(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	for _, id := range []string{"t1", "t2"} {
		if err := s.CreateTask(ctx, mkTask(id, "key:"+id)); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
	}

	// t1 走三段,t2 走一段;seq 各自从 1 单调递增,互不影响
	seqs1 := driveTransitions(t, s, "t1", []struct{ from, to State }{
		{StateQueued, StatePreparing},
		{StatePreparing, StateRunning},
		{StateRunning, StateCompleted},
	})
	seqs2 := driveTransitions(t, s, "t2", []struct{ from, to State }{
		{StateQueued, StatePreparing},
	})
	for i, want := range []int64{1, 2, 3} {
		if seqs1[i] != want {
			t.Errorf("t1 seq[%d] = %d, want %d", i, seqs1[i], want)
		}
	}
	if seqs2[0] != 1 {
		t.Errorf("t2 seq[0] = %d, want 1", seqs2[0])
	}

	// 终态迁移同事务写入 ended_at
	done, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask t1: %v", err)
	}
	if done.State != StateCompleted || done.EndedAt == nil {
		t.Errorf("t1 = %+v, want COMPLETED with ended_at", done)
	}
	// 非终态迁移不写 ended_at
	t2, err := s.GetTask(ctx, "t2")
	if err != nil {
		t.Fatalf("GetTask t2: %v", err)
	}
	if t2.EndedAt != nil {
		t.Errorf("t2 EndedAt = %v, want nil", t2.EndedAt)
	}
}

func TestTransitionTransactional(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	if err := s.CreateTask(ctx, mkTask("t1", "key:t1")); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// 失败迁移必须原子:state 与 events 都不落盘
	cases := []struct {
		name    string
		from    State
		to      State
		wantErr error
	}{
		{"state mismatch", StateRunning, StateCompleted, ErrStateMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.Transition(ctx, "t1", tc.from, tc.to, ""); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Transition err = %v, want %v", err, tc.wantErr)
			}
			task, err := s.GetTask(ctx, "t1")
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if task.State != StateQueued {
				t.Errorf("state = %s after rejected transition, want QUEUED", task.State)
			}
			evs, err := s.UnreportedEvents(ctx)
			if err != nil {
				t.Fatalf("UnreportedEvents: %v", err)
			}
			if len(evs) != 0 {
				t.Errorf("%d events persisted after rejected transition, want 0", len(evs))
			}
		})
	}

	// 不存在任务的迁移
	if _, err := s.Transition(ctx, "ghost", StateQueued, StateRunning, ""); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("Transition ghost: err = %v, want ErrTaskNotFound", err)
	}
}

func TestTransitionFromTerminalRejected(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	terminals := []State{StateCompleted, StateFailed, StateTimeout, StateCanceled}
	for i, term := range terminals {
		id := string(rune('a' + i))
		if err := s.CreateTask(ctx, mkTask(id, "key:"+id)); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		if _, err := s.Transition(ctx, id, StateQueued, term, ""); err != nil {
			t.Fatalf("Transition to %s: %v", term, err)
		}
		if _, err := s.Transition(ctx, id, term, StateRunning, ""); !errors.Is(err, ErrTerminalState) {
			t.Errorf("transition from %s: err = %v, want ErrTerminalState", term, err)
		}
	}
}

func TestUnreportedEventsTracking(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	if err := s.CreateTask(ctx, mkTask("t1", "key:t1")); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	driveTransitions(t, s, "t1", []struct{ from, to State }{
		{StateQueued, StatePreparing},
		{StatePreparing, StateRunning},
	})

	evs, err := s.UnreportedEvents(ctx)
	if err != nil {
		t.Fatalf("UnreportedEvents: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("len = %d, want 2", len(evs))
	}
	if evs[0].Seq != 1 || evs[1].Seq != 2 || evs[0].Reported || evs[1].Reported {
		t.Errorf("unexpected events: %+v", evs)
	}

	if err := s.MarkEventReported(ctx, "t1", 1); err != nil {
		t.Fatalf("MarkEventReported: %v", err)
	}
	// 幂等:重复标记不报错
	if err := s.MarkEventReported(ctx, "t1", 1); err != nil {
		t.Fatalf("MarkEventReported again: %v", err)
	}
	evs, err = s.UnreportedEvents(ctx)
	if err != nil {
		t.Fatalf("UnreportedEvents: %v", err)
	}
	if len(evs) != 1 || evs[0].Seq != 2 {
		t.Errorf("after mark: %+v, want only seq=2", evs)
	}
}

func TestResultRecordedDedup(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	if err := s.CreateTask(ctx, mkTask("t1", "key:t1")); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	ok, err := s.ResultRecorded(ctx, "t1")
	if err != nil {
		t.Fatalf("ResultRecorded: %v", err)
	}
	if ok {
		t.Error("ResultRecorded = true, want false")
	}
	if _, err := s.ResultRecorded(ctx, "ghost"); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("ResultRecorded ghost: err = %v, want ErrTaskNotFound", err)
	}

	if err := s.MarkResultRecorded(ctx, "t1"); err != nil {
		t.Fatalf("MarkResultRecorded: %v", err)
	}
	if err := s.MarkResultRecorded(ctx, "t1"); err != nil { // 幂等
		t.Fatalf("MarkResultRecorded again: %v", err)
	}
	if err := s.MarkResultRecorded(ctx, "ghost"); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("MarkResultRecorded ghost: err = %v, want ErrTaskNotFound", err)
	}
	ok, err = s.ResultRecorded(ctx, "t1")
	if err != nil || !ok {
		t.Errorf("ResultRecorded = %v, %v; want true, nil", ok, err)
	}
}

// TestCrashRecovery 模拟崩溃:生命周期中途直接 Close(不做逻辑收尾),
// 重新打开后 LoadInflight 必须返回恰好预期的在途任务/未上报事件/待补结果。
func TestCrashRecovery(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "agent.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// running:执行中崩溃 → 非终态在途,带 2 条未上报事件
	// queued:未启动 → 非终态在途,无事件
	// done:已终态,事件 1 条未上报 + 结果未记录 → 待补报
	// reported:已终态且全部上报 → 恢复视图之外
	for _, tk := range []Task{
		mkTask("running", "key:running"),
		mkTask("queued", "key:queued"),
		mkTask("done", "key:done"),
		mkTask("reported", "key:reported"),
	} {
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatalf("CreateTask %s: %v", tk.TaskID, err)
		}
	}
	driveTransitions(t, s, "running", []struct{ from, to State }{
		{StateQueued, StatePreparing},
		{StatePreparing, StateRunning},
	})
	driveTransitions(t, s, "done", []struct{ from, to State }{
		{StateQueued, StateCompleted},
	})
	driveTransitions(t, s, "reported", []struct{ from, to State }{
		{StateQueued, StateFailed},
	})
	if err := s.MarkEventReported(ctx, "reported", 1); err != nil {
		t.Fatalf("MarkEventReported: %v", err)
	}
	if err := s.MarkResultRecorded(ctx, "reported"); err != nil {
		t.Fatalf("MarkResultRecorded: %v", err)
	}

	// 模拟崩溃:不做任何逻辑收尾,直接关库
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 重启恢复
	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	inf, err := s2.LoadInflight(ctx)
	if err != nil {
		t.Fatalf("LoadInflight: %v", err)
	}

	// 非终态任务:恰好 running + queued
	if len(inf.Tasks) != 2 {
		t.Fatalf("inflight tasks = %+v, want 2", inf.Tasks)
	}
	got := map[string]State{}
	for _, tk := range inf.Tasks {
		got[tk.TaskID] = tk.State
	}
	if got["running"] != StateRunning || got["queued"] != StateQueued {
		t.Errorf("inflight tasks = %v, want running@RUNNING + queued@QUEUED", got)
	}

	// 未上报事件:running 的 2 条 + done 的 1 条,按 (task_id, seq) 排序
	if len(inf.UnreportedEvents) != 3 {
		t.Fatalf("unreported events = %+v, want 3", inf.UnreportedEvents)
	}
	type evKey struct {
		task string
		seq  int64
	}
	var gotEvs []evKey
	for _, e := range inf.UnreportedEvents {
		gotEvs = append(gotEvs, evKey{e.TaskID, e.Seq})
	}
	wantEvs := []evKey{{"done", 1}, {"running", 1}, {"running", 2}}
	for i := range wantEvs {
		if gotEvs[i] != wantEvs[i] {
			t.Errorf("event[%d] = %v, want %v", i, gotEvs[i], wantEvs[i])
		}
	}

	// 待补结果:恰好 done
	if len(inf.PendingResults) != 1 || inf.PendingResults[0].TaskID != "done" {
		t.Errorf("pending results = %+v, want [done]", inf.PendingResults)
	}
	if inf.PendingResults[0].EndedAt == nil {
		t.Error("done task missing ended_at after reopen")
	}

	// 恢复后补报可正常收敛
	for _, e := range inf.UnreportedEvents {
		if err := s2.MarkEventReported(ctx, e.TaskID, e.Seq); err != nil {
			t.Fatalf("MarkEventReported %v: %v", e, err)
		}
	}
	if err := s2.MarkResultRecorded(ctx, "done"); err != nil {
		t.Fatalf("MarkResultRecorded: %v", err)
	}
	inf2, err := s2.LoadInflight(ctx)
	if err != nil {
		t.Fatalf("LoadInflight after drain: %v", err)
	}
	if len(inf2.UnreportedEvents) != 0 || len(inf2.PendingResults) != 0 {
		t.Errorf("after drain: %+v, want no unreported/pending", inf2)
	}
}
