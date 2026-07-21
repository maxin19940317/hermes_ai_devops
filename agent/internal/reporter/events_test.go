package reporter

import (
	"context"
	"testing"
	"time"

	"hermes-devops/agent/internal/executor"
	"hermes-devops/agent/internal/store"
)

// statusQueued 是 executor.Status 尚未覆盖的初始态;首个迁移的 from 由
// 调用方(server 适配器)按 store 当前态转换传入,这里直接构造。
const statusQueued = executor.Status("QUEUED")

func newEventReporter(s *store.Store, baseURL string) *EventReporter {
	return &EventReporter{Store: s, Client: &Client{BaseURL: baseURL}}
}

func TestOnTransitionPersistsAndSends(t *testing.T) {
	f, srv := newFakeRuntime(t)
	s := openTempStore(t)
	seedTask(t, s, "t1", "wf1:t1:a1", "SERIAL1")
	er := newEventReporter(s, srv.URL)

	er.OnTransition("t1", statusQueued, executor.StatusPreparing, "开始准备")

	_, events, _ := f.snapshot()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev["task_id"] != "t1" || ev["idempotency_key"] != "wf1:t1:a1" {
		t.Errorf("event identity = %v", ev)
	}
	if ev["seq"] != float64(1) {
		t.Errorf("seq = %v, want 1", ev["seq"])
	}
	if ev["from"] != "QUEUED" || ev["to"] != "PREPARING" {
		t.Errorf("from/to = %v/%v", ev["from"], ev["to"])
	}
	if ts, _ := ev["ts"].(string); !tsPattern.MatchString(ts) {
		t.Errorf("ts = %q, want UTC millisecond ISO-8601", ts)
	}
	if ev["detail"] != "开始准备" {
		t.Errorf("detail = %v", ev["detail"])
	}

	// 落盘 + 确认:任务态已推进,事件已标记上报
	task, err := s.GetTask(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != store.StatePreparing {
		t.Errorf("task state = %s, want PREPARING", task.State)
	}
	unreported, err := s.UnreportedEvents(context.Background())
	if err != nil {
		t.Fatalf("UnreportedEvents: %v", err)
	}
	if len(unreported) != 0 {
		t.Errorf("unreported events = %v, want none", unreported)
	}
}

func TestEventRetryPreservesSeqOrder(t *testing.T) {
	f, srv := newFakeRuntime(t)
	f.eventStatus = 500 // 即发全部失败,事件积压
	s := openTempStore(t)
	seedTask(t, s, "t1", "wf1:t1:a1", "SERIAL1")
	seedTask(t, s, "t2", "wf1:t2:a1", "SERIAL2")
	er := newEventReporter(s, srv.URL)

	// 交错产生两任务的迁移,全部即发失败
	er.OnTransition("t1", statusQueued, executor.StatusPreparing, "")
	er.OnTransition("t2", statusQueued, executor.StatusPreparing, "")
	er.OnTransition("t1", executor.StatusPreparing, executor.StatusRunning, "")
	er.OnTransition("t2", executor.StatusPreparing, executor.StatusRunning, "")
	er.OnTransition("t1", executor.StatusRunning, executor.StatusCollecting, "")

	f.mu.Lock()
	f.eventStatus = 0 // 恢复 200
	f.mu.Unlock()
	er.drain(context.Background())

	_, events, _ := f.snapshot()
	// 全局按 (task_id, seq):t1 三条在前且 seq 严格递增,t2 两条随后
	want := []struct{ task, from, to string }{
		{"t1", "QUEUED", "PREPARING"},
		{"t1", "PREPARING", "RUNNING"},
		{"t1", "RUNNING", "COLLECTING"},
		{"t2", "QUEUED", "PREPARING"},
		{"t2", "PREPARING", "RUNNING"},
	}
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d: %v", len(events), len(want), events)
	}
	for i, w := range want {
		if events[i]["task_id"] != w.task || events[i]["from"] != w.from || events[i]["to"] != w.to {
			t.Errorf("events[%d] = %v %v->%v, want %s %s->%s",
				i, events[i]["task_id"], events[i]["from"], events[i]["to"], w.task, w.from, w.to)
		}
	}
	for i, seq := range []int{1, 2, 3} {
		if events[i]["seq"] != float64(seq) {
			t.Errorf("t1 events[%d].seq = %v, want %d", i, events[i]["seq"], seq)
		}
	}
	for i, seq := range []int{1, 2} {
		if events[3+i]["seq"] != float64(seq) {
			t.Errorf("t2 events[%d].seq = %v, want %d", i, events[3+i]["seq"], seq)
		}
	}

	unreported, err := s.UnreportedEvents(context.Background())
	if err != nil {
		t.Fatalf("UnreportedEvents: %v", err)
	}
	if len(unreported) != 0 {
		t.Errorf("unreported = %v, want none after drain", unreported)
	}

	// 重发安全:重复投递同 (key, seq) 由 Runtime 去重,不重复生效
	if err := er.Client.ReportEvent(context.Background(), TaskEvent{
		TaskID: "t1", IdempotencyKey: "wf1:t1:a1", Seq: 1,
		From: "QUEUED", To: "PREPARING", Ts: utcNowMs(),
	}); err != nil {
		t.Fatalf("resend dup event: %v", err)
	}
	if f.dupEventCount() != 1 {
		t.Errorf("dup events = %d, want 1", f.dupEventCount())
	}
	_, eventsAfter, _ := f.snapshot()
	if len(eventsAfter) != len(events) {
		t.Errorf("dup delivery applied: events %d → %d", len(events), len(eventsAfter))
	}
}

func TestEvent400NotRetried(t *testing.T) {
	f, srv := newFakeRuntime(t)
	f.eventStatus = 400 // 事件不合法:永久拒绝
	s := openTempStore(t)
	seedTask(t, s, "t1", "wf1:t1:a1", "SERIAL1")
	er := newEventReporter(s, srv.URL)

	er.OnTransition("t1", statusQueued, executor.StatusPreparing, "") // 即发失败(400)
	er.drain(context.Background())                                    // 补报再遭 400 → 按已上报处理,不再重发

	before := f.eventCallCount()
	if before != 2 {
		t.Fatalf("event calls = %d, want 2 (即发 1 + 补报 1)", before)
	}
	unreported, err := s.UnreportedEvents(context.Background())
	if err != nil {
		t.Fatalf("UnreportedEvents: %v", err)
	}
	if len(unreported) != 0 {
		t.Errorf("400 后事件仍滞留: %v", unreported)
	}

	er.drain(context.Background()) // 再次抽干:毒事件已清除,不得重发
	if got := f.eventCallCount(); got != before {
		t.Errorf("event calls grew %d → %d, 400 事件不应重发", before, got)
	}
}

func TestEventRetryLoopStopsOnCancel(t *testing.T) {
	_, srv := newFakeRuntime(t)
	s := openTempStore(t)
	er := newEventReporter(s, srv.URL)
	er.RetryInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- er.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run 取消应返回 nil, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run 未在 ctx 取消后及时返回")
	}
}
