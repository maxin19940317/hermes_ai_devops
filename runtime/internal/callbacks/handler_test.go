package callbacks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

var ctx = context.Background()

type fakeSignaler struct {
	mu    sync.Mutex
	calls []string // "workflowID/signalName/taskID"
	err   error
}

func (f *fakeSignaler) SignalWorkflow(_ context.Context, wfID, _ string, name string, arg interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var tid string
	switch v := arg.(type) {
	case wf.TaskHeartbeat:
		tid = v.TaskID
	case wf.TaskResultSignal:
		tid = v.TaskID
	}
	f.calls = append(f.calls, fmt.Sprintf("%s/%s/%s", wfID, name, tid))
	return f.err
}

func newEnv(t *testing.T) (*store.MemStore, *fakeSignaler, *httptest.Server) {
	t.Helper()
	s := store.NewMemStore()
	sig := &fakeSignaler{}
	h := New(s, sig, nil, 120)
	srv := httptest.NewServer(h.Mux())
	t.Cleanup(srv.Close)
	return s, sig, srv
}

func TestHealthz(t *testing.T) {
	_, _, srv := newEnv(t)
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "ok" {
		t.Errorf("body = %q, want %q", body, "ok")
	}
}

func post(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func validResult(taskID string) map[string]any {
	return map[string]any{
		"result_version": 1, "task_id": taskID, "attempt": 1,
		"status": "COMPLETED", "exit_code": 0, "duration_sec": 412.5,
		"cases":          map[string]any{"total": 38, "passed": 38, "failed": 0, "skipped": 0},
		"signatures_hit": []string{},
		"metrics":        map[string]any{"latency_ms_p50": 12.4},
		"attachments": []map[string]any{{"name": "logcat.txt", "object_key": "runs/x/logcat.txt",
			"sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "size": 1024}},
	}
}

func TestHeartbeatUpsertsAndRenewsLeases(t *testing.T) {
	s, sig, srv := newEnv(t)
	_ = s.CreateTask(ctx, wf.TaskRow{TaskID: "w1:t:a1", WorkflowID: "w1", IdempotencyKey: "w1:t:a1"})

	resp := post(t, srv.URL+"/callbacks/v1/heartbeat", map[string]any{
		"client_id": "c1", "agent_version": "0.1.0", "base_url": "https://client:8443",
		"ts": "2026-07-17T08:00:00.000Z",
		"devices": []map[string]any{{"serial": "513cd3de", "state": "IDLE",
			"props": map[string]any{"soc": "QCM6125", "abi": "arm64-v8a", "capabilities": []string{"hexagon"}}}},
		"active_task_ids": []string{"w1:t:a1", "unknown-task"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// 设备入库可被调度
	l, err := s.AcquireDevice(ctx, wf.DeviceSelector{SOC: []string{"QCM6125"}}, "t9", 120)
	if err != nil || l == nil || l.ClientBaseURL != "https://client:8443" {
		t.Errorf("lease=%+v err=%v", l, err)
	}
	// 已知任务续租 signal;未知任务忽略不报错
	if len(sig.calls) != 1 || sig.calls[0] != "w1/"+wf.SignalTaskHeartbeat+"/w1:t:a1" {
		t.Errorf("signals = %v", sig.calls)
	}
}

// heartbeatLeaseFixture 注册设备、以 0s 租约(立即过期,模拟持有者失联)占用,
// 并登记带 device_id 的任务行,返回设备的 selector。
func heartbeatLeaseFixture(t *testing.T, s *store.MemStore, srv *httptest.Server) wf.DeviceSelector {
	t.Helper()
	resp := post(t, srv.URL+"/callbacks/v1/heartbeat", map[string]any{
		"client_id": "c1", "agent_version": "0.1.0", "base_url": "https://client:8443",
		"devices": []map[string]any{{"serial": "513cd3de", "state": "IDLE",
			"props": map[string]any{"soc": "QCM6125", "abi": "arm64-v8a", "capabilities": []string{"hexagon"}}}},
		"active_task_ids": []string{},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register heartbeat status = %d", resp.StatusCode)
	}
	sel := wf.DeviceSelector{SOC: []string{"QCM6125"}}
	l, err := s.AcquireDevice(ctx, sel, "w1:t:a1", 0) // 0s 租约立即过期
	if err != nil || l == nil {
		t.Fatalf("acquire: lease=%v err=%v", l, err)
	}
	if err := s.CreateTask(ctx, wf.TaskRow{TaskID: "w1:t:a1", WorkflowID: "w1",
		IdempotencyKey: "w1:t:a1", ClientID: "c1", DeviceID: l.DeviceID, Status: "RUNNING"}); err != nil {
		t.Fatal(err)
	}
	return sel
}

// TestHeartbeatRenewsDatabaseLease:心跳中的 active_task_ids 必须续 DB 租约
// (§10 心跳即租约续期);否则 lease_expires_at 只是 acquire 时写的一次性死值,
// AcquireDevice 的过期懒回收会偷走健康长任务(900s)正在使用的设备。
func TestHeartbeatRenewsDatabaseLease(t *testing.T) {
	s, _, srv := newEnv(t)
	sel := heartbeatLeaseFixture(t, s, srv)

	resp := post(t, srv.URL+"/callbacks/v1/heartbeat", map[string]any{
		"client_id": "c1", "active_task_ids": []string{"w1:t:a1"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if l, _ := s.AcquireDevice(ctx, sel, "t2", 120); l != nil {
		t.Errorf("心跳续期后设备不得被回收: %+v", l)
	}
}

// TestHeartbeatSignalFailureStillRenewsLease:故障注入——workflow 已被 Terminate,
// 续租 signal 必然失败;但只要 Client 仍在上报该任务(设备被物理占用),
// DB 租约必须照样续期且心跳仍返回 200,设备不得被其他 workflow 偷走。
// Client 停止上报后租约自然过期,由 AcquireDevice 回收(恢复路径,见 store 一致性测试)。
func TestHeartbeatSignalFailureStillRenewsLease(t *testing.T) {
	s, sig, srv := newEnv(t)
	sig.err = errors.New("workflow execution not found")
	sel := heartbeatLeaseFixture(t, s, srv)

	resp := post(t, srv.URL+"/callbacks/v1/heartbeat", map[string]any{
		"client_id": "c1", "active_task_ids": []string{"w1:t:a1"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signal 失败不应影响心跳响应: status = %d", resp.StatusCode)
	}
	if l, _ := s.AcquireDevice(ctx, sel, "t2", 120); l != nil {
		t.Errorf("signal 失败但 Client 仍在心跳,设备不得被回收: %+v", l)
	}
}

func TestTaskEventDedupAndStatus(t *testing.T) {
	s, _, srv := newEnv(t)
	_ = s.CreateTask(ctx, wf.TaskRow{TaskID: "w1:t:a1", WorkflowID: "w1", IdempotencyKey: "w1:t:a1", Status: "DISPATCHING"})
	ev := map[string]any{"task_id": "w1:t:a1", "idempotency_key": "w1:t:a1", "seq": 1,
		"from": "ACCEPTED", "to": "RUNNING", "ts": "2026-07-17T08:00:01.000Z"}
	if resp := post(t, srv.URL+"/callbacks/v1/task-events", ev); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp := post(t, srv.URL+"/callbacks/v1/task-events", ev); resp.StatusCode != http.StatusOK {
		t.Fatalf("重发 status = %d(幂等,§8.2)", resp.StatusCode)
	}
	row, _ := s.GetTask(ctx, "w1:t:a1")
	if row.Status != "RUNNING" {
		t.Errorf("status = %s", row.Status)
	}
}

func TestResultValidateSaveSignalOnce(t *testing.T) {
	s, sig, srv := newEnv(t)
	_ = s.CreateTask(ctx, wf.TaskRow{TaskID: "w1:t:a1", WorkflowID: "w1", IdempotencyKey: "w1:t:a1"})

	body := map[string]any{"task_id": "w1:t:a1", "idempotency_key": "w1:t:a1", "result": validResult("w1:t:a1")}
	if resp := post(t, srv.URL+"/callbacks/v1/results", body); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp := post(t, srv.URL+"/callbacks/v1/results", body); resp.StatusCode != http.StatusOK {
		t.Fatalf("重发 status = %d", resp.StatusCode)
	}
	// signal 只投递一次(§8.2),载荷字段来自 result.json
	if len(sig.calls) != 1 || sig.calls[0] != "w1/"+wf.SignalTaskResult+"/w1:t:a1" {
		t.Errorf("signals = %v", sig.calls)
	}
}

func TestResultSchemaViolationIs400(t *testing.T) {
	s, sig, srv := newEnv(t)
	_ = s.CreateTask(ctx, wf.TaskRow{TaskID: "w1:t:a1", WorkflowID: "w1", IdempotencyKey: "w1:t:a1"})
	bad := validResult("w1:t:a1")
	delete(bad, "cases") // 缺必填字段
	resp := post(t, srv.URL+"/callbacks/v1/results",
		map[string]any{"task_id": "w1:t:a1", "idempotency_key": "w1:t:a1", "result": bad})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400(红线:未经 Schema 校验不消费,§14)", resp.StatusCode)
	}
	if len(sig.calls) != 0 {
		t.Errorf("非法 result 不得 signal: %v", sig.calls)
	}
}

func TestResultUnknownTaskIs400(t *testing.T) {
	_, sig, srv := newEnv(t)
	resp := post(t, srv.URL+"/callbacks/v1/results",
		map[string]any{"task_id": "ghost", "idempotency_key": "ghost", "result": validResult("ghost")})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if len(sig.calls) != 0 {
		t.Errorf("未知任务不得 signal: %v", sig.calls)
	}
}
