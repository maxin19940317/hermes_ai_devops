package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"hermes-devops/runtime/internal/store"
	"hermes-devops/runtime/internal/testtemporal"
	wf "hermes-devops/runtime/internal/workflow"
)

var ctx = context.Background()

type fakeSignaler struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeSignaler) SignalWorkflow(_ context.Context, wfID, _ string, name string, arg interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, wfID+"/"+name)
	return f.err
}

func seedOutbox(t *testing.T, s *store.MemStore, ev store.OutboxEvent) int64 {
	t.Helper()
	_, err := s.SaveResultWithOutbox(ctx,
		wf.ResultRecord{TaskID: ev.AggregateID, Result: wf.TaskResultSignal{TaskID: ev.AggregateID}}, ev)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.ClaimUnpublished(ctx, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("seed: rows=%+v err=%v", rows, err)
	}
	return rows[0].ID
}

func resultEvent(t *testing.T, workflowID, taskID string) store.OutboxEvent {
	t.Helper()
	payload, err := json.Marshal(store.ResultEventPayload{
		WorkflowID: workflowID,
		Result:     wf.TaskResultSignal{TaskID: taskID, Status: "COMPLETED"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return store.OutboxEvent{AggregateType: "task", AggregateID: taskID,
		EventType: store.EventTypeTaskResult, EventKey: taskID + ":result", Payload: payload}
}

// fakeNotifier 记录每次 SendText 的调用内容,便于断言通知文案用到了展示字段。
type fakeNotifier struct {
	mu    sync.Mutex
	texts []string
	err   error
}

func (f *fakeNotifier) SendText(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.texts = append(f.texts, text)
	return f.err
}

func (f *fakeNotifier) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.texts)
}

func quarantineEvent(t *testing.T) store.OutboxEvent {
	t.Helper()
	payload, err := json.Marshal(store.QuarantineEventPayload{
		DeviceID: "dev-1", ClientID: "client-1", Serial: "SN123",
		DisplayName: "QCM6125-01", FailStreak: 3, TaskID: "wf-1:t:a3", FailureStage: "deploy",
	})
	if err != nil {
		t.Fatal(err)
	}
	return store.OutboxEvent{AggregateType: "device", AggregateID: "dev-1",
		EventType: store.EventTypeDeviceQuarantined, EventKey: "dev-1:quarantined:wf-1:t:a3", Payload: payload}
}

// published 判断该 outbox 行是否已被标记投递(不在 ClaimUnpublished 结果中)。
func published(t *testing.T, s *store.MemStore, id int64) bool {
	t.Helper()
	rows, err := s.ClaimUnpublished(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == id {
			return false
		}
	}
	return true
}

// 隔离通知投递:成功 → published,文案带上展示字段;Notifier 返回错误 →
// MarkFailed 且保持 pending,不得吞错误。
func TestDeliverQuarantineNotification(t *testing.T) {
	t.Run("投递成功标记published且文案带展示字段", func(t *testing.T) {
		s := store.NewMemStore()
		ev := quarantineEvent(t)
		id := seedOutbox(t, s, ev)
		notifier := &fakeNotifier{}
		r := &Relay{Store: s, Notifier: notifier}

		r.deliver(ctx, store.OutboxEvent{ID: id, AggregateType: ev.AggregateType,
			AggregateID: ev.AggregateID, EventType: ev.EventType, EventKey: ev.EventKey, Payload: ev.Payload})

		if !published(t, s, id) {
			t.Fatal("通知发送成功后应标记 published")
		}
		if notifier.callCount() != 1 {
			t.Fatalf("SendText 调用次数 = %d, want 1", notifier.callCount())
		}
		text := notifier.texts[0]
		for _, want := range []string{"client-1", "3", "deploy", "dev-1"} {
			if !strings.Contains(text, want) {
				t.Errorf("通知文案缺少展示字段 %q: %q", want, text)
			}
		}
	})

	t.Run("Notifier返回错误则MarkFailed并保持pending", func(t *testing.T) {
		s := store.NewMemStore()
		ev := quarantineEvent(t)
		id := seedOutbox(t, s, ev)
		notifier := &fakeNotifier{err: errors.New("feishu unavailable")}
		r := &Relay{Store: s, Notifier: notifier}

		r.deliver(ctx, store.OutboxEvent{ID: id, AggregateType: ev.AggregateType,
			AggregateID: ev.AggregateID, EventType: ev.EventType, EventKey: ev.EventKey, Payload: ev.Payload})

		if published(t, s, id) {
			t.Fatal("Notifier 失败却标记 published")
		}
		rows, err := s.ClaimUnpublished(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].Attempts != 1 {
			t.Fatalf("rows = %+v, want 1 row with attempts=1", rows)
		}
		if !strings.Contains(rows[0].LastError, "feishu unavailable") {
			t.Errorf("last_error = %q, want 包含底层错误", rows[0].LastError)
		}
	})
}

// 保证等级的关键:未配置且未显式关闭时不得静默成功(spec §9.3)。
// 若这里改成直接 MarkPublished,"绝不丢"就退化成静默丢弃隔离通知。
func TestQuarantineEventStaysPendingWhenNotifierUnconfigured(t *testing.T) {
	s := store.NewMemStore()
	ev := quarantineEvent(t)
	id := seedOutbox(t, s, ev)
	r := &Relay{Store: s} // Notifier 为 nil,DeviceNotifyDisabled 缺省 false

	r.deliver(ctx, store.OutboxEvent{ID: id, AggregateType: ev.AggregateType,
		AggregateID: ev.AggregateID, EventType: ev.EventType, EventKey: ev.EventKey, Payload: ev.Payload})

	if published(t, s, id) {
		t.Fatal("未配置通知端却标记已投递 = 静默丢弃隔离通知")
	}
	rows, err := s.ClaimUnpublished(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Attempts != 1 {
		t.Fatalf("rows = %+v, want 1 row with attempts=1", rows)
	}
	if !strings.Contains(rows[0].LastError, "notifier not configured") {
		t.Errorf("last_error = %q, want 明确指出未配置", rows[0].LastError)
	}
}

// 显式 RELAY_DEVICE_NOTIFY=off:有意关闭,标记已投递,不占 backlog。
func TestQuarantineEventPublishedWhenNotifyExplicitlyOff(t *testing.T) {
	s := store.NewMemStore()
	ev := quarantineEvent(t)
	id := seedOutbox(t, s, ev)
	r := &Relay{Store: s, DeviceNotifyDisabled: true} // Notifier 仍为 nil,验证关闭优先于"未配置"分支

	r.deliver(ctx, store.OutboxEvent{ID: id, AggregateType: ev.AggregateType,
		AggregateID: ev.AggregateID, EventType: ev.EventType, EventKey: ev.EventKey, Payload: ev.Payload})

	if !published(t, s, id) {
		t.Fatal("显式关闭时应标记已投递,避免永久占用 backlog")
	}
	rows, err := s.ClaimUnpublished(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %+v, want 0(已发布)", rows)
	}
}

// payload 解码失败(部署/版本错配)按坏行处理:记 failed 供监控介入,不得 panic。
func TestQuarantineEventBadPayloadMarksFailed(t *testing.T) {
	s := store.NewMemStore()
	ev := store.OutboxEvent{AggregateType: "device", AggregateID: "dev-1",
		EventType: store.EventTypeDeviceQuarantined, EventKey: "dev-1:quarantined:bad", Payload: json.RawMessage(`{bad`)}
	id := seedOutbox(t, s, ev)
	r := &Relay{Store: s, Notifier: &fakeNotifier{}}

	r.deliver(ctx, store.OutboxEvent{ID: id, AggregateType: ev.AggregateType,
		AggregateID: ev.AggregateID, EventType: ev.EventType, EventKey: ev.EventKey, Payload: ev.Payload})

	if published(t, s, id) {
		t.Fatal("payload 解码失败却标记已投递")
	}
}

// 投递归宿状态机(表驱动):成功/NotFound → published;瞬时错误/未知事件 →
// attempts+1 记 last_error,行保持未投递待重试。
func TestDeliverOutcome(t *testing.T) {
	cases := []struct {
		name         string
		event        func(t *testing.T) store.OutboxEvent
		signalErr    error
		wantPending  bool
		wantAttempts int
		wantSignal   bool // 是否调用了 SignalWorkflow
	}{
		{"投递成功标记published", func(t *testing.T) store.OutboxEvent {
			return resultEvent(t, "w1", "w1:t:a1")
		}, nil, false, 0, true},
		{"瞬时错误记failed待重试", func(t *testing.T) store.OutboxEvent {
			return resultEvent(t, "w1", "w1:t:a1")
		}, errors.New("temporal unavailable"), true, 1, true},
		{"NotFound视为已消费", func(t *testing.T) store.OutboxEvent {
			return resultEvent(t, "gone-workflow", "w1:t:a1")
		}, serviceerror.NewNotFound("workflow execution not found"), false, 0, true},
		{"未知事件类型记failed", func(t *testing.T) store.OutboxEvent {
			return store.OutboxEvent{AggregateType: "task", AggregateID: "w1:t:a1",
				EventType: "future-event", EventKey: "w1:t:a1:future", Payload: json.RawMessage(`{}`)}
		}, nil, true, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := store.NewMemStore()
			ev := tc.event(t)
			id := seedOutbox(t, s, ev)
			sig := &fakeSignaler{err: tc.signalErr}
			r := &Relay{Store: s, Signaler: sig, BatchSize: 10, PollInterval: time.Millisecond, MaxBackoff: 10 * time.Millisecond}

			r.deliver(ctx, store.OutboxEvent{ID: id, AggregateType: ev.AggregateType,
				AggregateID: ev.AggregateID, EventType: ev.EventType, EventKey: ev.EventKey, Payload: ev.Payload})

			rows, err := s.ClaimUnpublished(ctx, 10)
			if err != nil {
				t.Fatal(err)
			}
			if (len(rows) == 1) != tc.wantPending {
				t.Fatalf("pending rows = %d, wantPending=%v", len(rows), tc.wantPending)
			}
			if tc.wantPending && rows[0].Attempts != tc.wantAttempts {
				t.Errorf("attempts = %d, want %d", rows[0].Attempts, tc.wantAttempts)
			}
			if (len(sig.calls) > 0) != tc.wantSignal {
				t.Errorf("signal calls = %v, wantSignal=%v", sig.calls, tc.wantSignal)
			}
		})
	}
}

// ---- e2e:真实 dev server 验证投递与 NotFound 语义(internal/testtemporal) ----

const e2eTaskQueue = "relay-e2e"

// relayE2EWorkflow 阻塞等待一个 task-result signal 后完成(模拟 DeviceTestWorkflow
// 的 awaitResult 段)。
func relayE2EWorkflow(wctx workflow.Context) (wf.TaskResultSignal, error) {
	var res wf.TaskResultSignal
	workflow.GetSignalChannel(wctx, wf.SignalTaskResult).Receive(wctx, &res)
	return res, nil
}

func waitPublished(t *testing.T, s *store.MemStore, wantPending int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := s.ClaimUnpublished(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == wantPending {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("10s 内 outbox 未收敛到 pending=%d", wantPending)
}

func TestRelayE2EDeliversSignal(t *testing.T) {
	addr := testtemporal.StartDevServer(t)
	c, err := client.Dial(client.Options{HostPort: addr})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	w := worker.New(c, e2eTaskQueue, worker.Options{})
	w.RegisterWorkflow(relayE2EWorkflow)
	if err := w.Start(); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	defer w.Stop()

	s := store.NewMemStore()
	r := &Relay{Store: s, Signaler: c, BatchSize: 10, PollInterval: 50 * time.Millisecond, MaxBackoff: time.Second}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = r.Run(rctx) }()

	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: "relay-e2e-target", TaskQueue: e2eTaskQueue,
	}, relayE2EWorkflow)
	if err != nil {
		t.Fatalf("start workflow: %v", err)
	}
	seedOutbox(t, s, resultEvent(t, "relay-e2e-target", "relay-e2e-target:t:a1"))

	// Relay 投递 → workflow 收到 signal 完成;outbox 行标记已投
	var got wf.TaskResultSignal
	gctx, gcancel := context.WithTimeout(ctx, 30*time.Second)
	defer gcancel()
	if err := run.Get(gctx, &got); err != nil {
		t.Fatalf("workflow 未在 30s 内完成(relay 未投递?): %v", err)
	}
	if got.TaskID != "relay-e2e-target:t:a1" || got.Status != "COMPLETED" {
		t.Errorf("workflow 收到的 signal = %+v", got)
	}
	waitPublished(t, s, 0)
}

func TestRelayE2ENotFoundConsumed(t *testing.T) {
	addr := testtemporal.StartDevServer(t)
	c, err := client.Dial(client.Options{HostPort: addr})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	s := store.NewMemStore()
	r := &Relay{Store: s, Signaler: c, BatchSize: 10, PollInterval: 50 * time.Millisecond, MaxBackoff: time.Second}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = r.Run(rctx) }()

	// workflow 不存在(已结束/从未启动):Relay 必须视为已消费标记 published,
	// 而不是死循环重试
	seedOutbox(t, s, resultEvent(t, "no-such-workflow", "no-such-workflow:t:a1"))
	waitPublished(t, s, 0)

	// attempts 保持 0:NotFound 不是失败,不得计入重试
	rows, err := s.ClaimUnpublished(ctx, 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("行应已投递: rows=%+v err=%v", rows, err)
	}
}

// backlogLevels 断言积压报告的分级:卡住的行或过老的积压必须升到 warn,
// 否则日志无法直接当告警条件用(只是流水就没人看)。
func TestReportBacklogLevels(t *testing.T) {
	cases := []struct {
		name      string
		attempts  int // 对唯一一行调用 MarkFailed 的次数
		published bool
		warnAge   time.Duration
		wantLevel string
	}{
		{name: "empty is debug", published: true, wantLevel: "debug"},
		{name: "pending is info", wantLevel: "info"},
		{name: "stuck is warn", attempts: 3, wantLevel: "warn"},
		{name: "old backlog is warn", warnAge: time.Nanosecond, wantLevel: "warn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := store.NewMemStore()
			id := seedOutbox(t, st, resultEvent(t, "wf-1", "t1"))
			for i := 0; i < tc.attempts; i++ {
				if err := st.MarkFailed(ctx, id, "boom"); err != nil {
					t.Fatal(err)
				}
			}
			if tc.published {
				if err := st.MarkPublished(ctx, id); err != nil {
					t.Fatal(err)
				}
			}
			var buf bytes.Buffer
			log := zerolog.New(&buf).Level(zerolog.DebugLevel)
			r := &Relay{Store: st, Log: &log, StuckAttempts: 3, BacklogWarnAge: tc.warnAge}
			r.reportBacklog(ctx)

			var got map[string]any
			if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
				t.Fatalf("log not JSON: %v (%q)", err, buf.String())
			}
			if got["level"] != tc.wantLevel {
				t.Errorf("level = %v, want %v (log: %s)", got["level"], tc.wantLevel, buf.String())
			}
			if got["message"] != "outbox backlog" {
				t.Errorf("message = %v", got["message"])
			}
		})
	}
}

// 查询失败不得中断投递循环——监控挂了不该拖垮被监控的东西。
func TestReportBacklogQueryErrorIsNonFatal(t *testing.T) {
	var buf bytes.Buffer
	log := zerolog.New(&buf)
	r := &Relay{Store: backlogErrStore{store.NewMemStore()}, Log: &log}
	r.reportBacklog(ctx) // 不 panic 即通过
	if !strings.Contains(buf.String(), "outbox backlog query failed") {
		t.Errorf("查询失败必须留痕, got %q", buf.String())
	}
}

type backlogErrStore struct{ *store.MemStore }

func (backlogErrStore) OutboxBacklog(context.Context, int) (*store.OutboxBacklog, error) {
	return nil, errors.New("db down")
}
