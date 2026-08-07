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
	// 原则 6:心跳不再发任何 workflow signal(续租只写 DB,workflow 用 Timer+CheckLease)
	if len(sig.calls) != 0 {
		t.Errorf("心跳不得发 signal: %v", sig.calls)
	}
}

// heartbeatLeaseFixture 注册设备、以 0s 租约(立即过期,模拟持有者失联临界点)
// 租给 "w1:t:a1" 并登记带 device_id 的任务行,返回 selector 与租约所有权凭据。
func heartbeatLeaseFixture(t *testing.T, s *store.MemStore, srv *httptest.Server) (wf.DeviceSelector, *wf.Lease) {
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
	return sel, l
}

func activeTaskEntry(l *wf.Lease, taskID string, attempt int) map[string]any {
	return map[string]any{"task_id": taskID, "attempt": attempt,
		"lease_id": l.LeaseID, "lease_generation": l.Generation}
}

// TestHeartbeatRenewsLeaseWithCredentials:新格式心跳携带所有权凭据
// (task_id/attempt/lease_id/lease_generation,§10/差距 #15)条件续租成功,
// 设备不得被回收。
func TestHeartbeatRenewsLeaseWithCredentials(t *testing.T) {
	s, _, srv := newEnv(t)
	sel, l := heartbeatLeaseFixture(t, s, srv)

	resp := post(t, srv.URL+"/callbacks/v1/heartbeat", map[string]any{
		"client_id":       "c1",
		"active_task_ids": []any{activeTaskEntry(l, "w1:t:a1", 1)},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var ack struct {
		NotOwned []struct{ TaskID string } `json:"not_owned"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ack)
	if len(ack.NotOwned) != 0 {
		t.Errorf("正确凭据不得被判 LEASE_NOT_OWNED: %+v", ack.NotOwned)
	}
	if l2, _ := s.AcquireDevice(ctx, sel, "w2:t:a1", 120); l2 != nil {
		t.Errorf("凭据续租后设备不得被回收: %+v", l2)
	}
}

// TestHeartbeatLeaseNotOwned:故障注入——凭据失配(租约已易主/伪造)必须逐项
// 返回 LEASE_NOT_OWNED 且不续租(HTTP 仍 200,见 callbacks OpenAPI HeartbeatAck);
// Client 收到后应停止操作该任务。
func TestHeartbeatLeaseNotOwned(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(entry map[string]any, clientID *string)
	}{
		{"错 generation", func(e map[string]any, _ *string) { e["lease_generation"] = 99 }},
		{"错 lease_id", func(e map[string]any, _ *string) { e["lease_id"] = "forged" }},
		{"错 attempt", func(e map[string]any, _ *string) { e["attempt"] = 2 }},
		{"错 client", func(_ map[string]any, c *string) { *c = "intruder" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, srv := newEnv(t)
			sel, l := heartbeatLeaseFixture(t, s, srv)
			clientID := "c1"
			entry := activeTaskEntry(l, "w1:t:a1", 1)
			tc.mutate(entry, &clientID)

			resp := post(t, srv.URL+"/callbacks/v1/heartbeat", map[string]any{
				"client_id":       clientID,
				"active_task_ids": []any{entry},
			})
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d(失配也回 200,错误码在响应体)", resp.StatusCode)
			}
			var ack struct {
				NotOwned []struct {
					TaskID string `json:"task_id"`
					Code   string `json:"code"`
				} `json:"not_owned"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&ack)
			if len(ack.NotOwned) != 1 || ack.NotOwned[0].TaskID != "w1:t:a1" ||
				ack.NotOwned[0].Code != "LEASE_NOT_OWNED" {
				t.Errorf("ack.NotOwned = %+v, want 单条 LEASE_NOT_OWNED", ack.NotOwned)
			}
			if l2, _ := s.AcquireDevice(ctx, sel, "w2:t:a1", 120); l2 == nil {
				t.Error("失配不得续租,过期租约应可被懒回收")
			}
		})
	}
}

// TestHeartbeatLegacyStringFormatNoRenew:过渡兼容——旧格式(纯字符串数组)
// 仅续 client 心跳,不续租、不报错、不判 LEASE_NOT_OWNED;
// 过期租约照常懒回收(滚动升级窗口,下线时点见契约注释)。
func TestHeartbeatLegacyStringFormatNoRenew(t *testing.T) {
	s, sig, srv := newEnv(t)
	sel, _ := heartbeatLeaseFixture(t, s, srv)

	resp := post(t, srv.URL+"/callbacks/v1/heartbeat", map[string]any{
		"client_id":       "c1",
		"active_task_ids": []string{"w1:t:a1"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("旧格式不应报错: status = %d", resp.StatusCode)
	}
	if len(sig.calls) != 0 {
		t.Errorf("旧格式也不得发 signal: %v", sig.calls)
	}
	if l2, _ := s.AcquireDevice(ctx, sel, "w2:t:a1", 120); l2 == nil {
		t.Error("旧格式不续租,过期租约应可被懒回收")
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

// TestResultWritesOutboxAtomically:终态结果走事务性 Outbox(原则 3,差距 #1)——
// 重发去重后 outbox 仍只有一行,event_key 为 {task_id}:result,payload 带 workflow_id
// 供 Relay 路由 signal。
func TestResultWritesOutboxAtomically(t *testing.T) {
	s, _, srv := newEnv(t)
	_ = s.CreateTask(ctx, wf.TaskRow{TaskID: "w1:t:a1", WorkflowID: "w1", IdempotencyKey: "w1:t:a1"})

	body := map[string]any{"task_id": "w1:t:a1", "idempotency_key": "w1:t:a1", "result": validResult("w1:t:a1")}
	for i := 0; i < 2; i++ { // 首发 + 重发
		if resp := post(t, srv.URL+"/callbacks/v1/results", body); resp.StatusCode != http.StatusOK {
			t.Fatalf("第 %d 次 status = %d", i+1, resp.StatusCode)
		}
	}
	rows, err := s.ClaimUnpublished(ctx, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("outbox rows = %+v err=%v, want 单行(重发不产生第二行)", rows, err)
	}
	ev := rows[0]
	if ev.EventType != store.EventTypeTaskResult || ev.EventKey != "w1:t:a1:result" ||
		ev.AggregateType != "task" || ev.AggregateID != "w1:t:a1" {
		t.Errorf("outbox row = %+v", ev)
	}
	var p store.ResultEventPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatalf("payload 不可解析: %v", err)
	}
	if p.WorkflowID != "w1" || p.Result.TaskID != "w1:t:a1" || p.Result.Status != "COMPLETED" {
		t.Errorf("payload = %+v", p)
	}
}

// TestResultSignalFailureStillOK:故障注入——Temporal 抖动直发 signal 失败;
// outbox 已单事务落库(Relay 会补投),回调仍返回 200,Client 无需重发。
func TestResultSignalFailureStillOK(t *testing.T) {
	s, sig, srv := newEnv(t)
	sig.err = errors.New("temporal unavailable")
	_ = s.CreateTask(ctx, wf.TaskRow{TaskID: "w1:t:a1", WorkflowID: "w1", IdempotencyKey: "w1:t:a1"})

	resp := post(t, srv.URL+"/callbacks/v1/results",
		map[string]any{"task_id": "w1:t:a1", "idempotency_key": "w1:t:a1", "result": validResult("w1:t:a1")})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signal 失败不应影响回调(outbox 兜底): status = %d", resp.StatusCode)
	}
	rows, err := s.ClaimUnpublished(ctx, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("outbox 必须有待 Relay 补投的行: rows=%+v err=%v", rows, err)
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

// TestHeartbeatCalibratesCapabilitiesFromServerTable:方案 B(2026-08-06)——
// 能力以服务端表为权威。serial 优先于 soc;命中 → 覆盖 Agent 上报值;
// 未命中 → 保留上报值(向后兼容)。
func TestHeartbeatCalibratesCapabilitiesFromServerTable(t *testing.T) {
	t.Run("soc 命中覆盖", func(t *testing.T) {
		s := store.NewMemStore()
		sig := &fakeSignaler{}
		h := New(s, sig, nil, 120).WithDeviceCaps(map[string][]string{
			"qcm6125": {"hexagon"},
			"idp":     {"hexagon"},
		})
		srv := httptest.NewServer(h.Mux())
		defer srv.Close()
		// Agent 上报空能力(新板未配置 agent),服务端表按 soc 校准
		resp := post(t, srv.URL+"/callbacks/v1/heartbeat", map[string]any{
			"client_id": "c1", "agent_version": "0.12.0",
			"devices": []map[string]any{
				{"serial": "825485946", "state": "IDLE",
					"props": map[string]any{"os": "linux", "soc": "idp", "abi": "arm64-v8a", "capabilities": []string{}}},
			},
			"active_task_ids": []string{},
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		devs, _ := s.ListFleet(ctx)
		if len(devs) != 1 || len(devs[0].Capabilities) != 1 || devs[0].Capabilities[0] != "hexagon" {
			t.Errorf("devices = %+v, want 服务端校准为 hexagon", devs)
		}
	})
	t.Run("serial 优先于 soc", func(t *testing.T) {
		s := store.NewMemStore()
		sig := &fakeSignaler{}
		h := New(s, sig, nil, 120).WithDeviceCaps(map[string][]string{
			"qcm6125":    {"hexagon"},
			"513cd3de":   {"hexagon", "custom"},
		})
		srv := httptest.NewServer(h.Mux())
		defer srv.Close()
		resp := post(t, srv.URL+"/callbacks/v1/heartbeat", map[string]any{
			"client_id": "c1", "agent_version": "0.12.0",
			"devices": []map[string]any{
				{"serial": "513cd3de", "state": "IDLE",
					"props": map[string]any{"os": "android", "soc": "QCM6125", "abi": "arm64-v8a", "capabilities": []string{}}},
			},
			"active_task_ids": []string{},
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		devs, _ := s.ListFleet(ctx)
		if len(devs) != 1 || len(devs[0].Capabilities) != 2 || devs[0].Capabilities[1] != "custom" {
			t.Errorf("devices = %+v, want serial 级覆盖(custom)", devs)
		}
	})
	t.Run("未命中保留 Agent 上报", func(t *testing.T) {
		s := store.NewMemStore()
		sig := &fakeSignaler{}
		h := New(s, sig, nil, 120).WithDeviceCaps(map[string][]string{"qcm6125": {"hexagon"}})
		srv := httptest.NewServer(h.Mux())
		defer srv.Close()
		resp := post(t, srv.URL+"/callbacks/v1/heartbeat", map[string]any{
			"client_id": "c1", "agent_version": "0.12.0",
			"devices": []map[string]any{
				{"serial": "b84aa09110cfc84a", "state": "IDLE",
					"props": map[string]any{"os": "android", "soc": "rk3576", "abi": "arm64-v8a", "capabilities": []string{"custom-caps"}}},
			},
			"active_task_ids": []string{},
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		devs, _ := s.ListFleet(ctx)
		if len(devs) != 1 || len(devs[0].Capabilities) != 1 || devs[0].Capabilities[0] != "custom-caps" {
			t.Errorf("devices = %+v, want 保留 Agent 上报 custom-caps", devs)
		}
	})
}

// TestHeartbeatNormalizesSOCFromServerTable(2026-08-07):Agent 服务模式读系统
// env,start-agent.ps1 的 $env: 不生效,平台代号(idp/bengal)在服务端兜底
// 归一化为型号。命中 → soc 与显示名统一;未命中 → 保留上报值。
func TestHeartbeatNormalizesSOCFromServerTable(t *testing.T) {
	t.Run("idp 命中归一化为 QCS6490", func(t *testing.T) {
		s := store.NewMemStore()
		sig := &fakeSignaler{}
		h := New(s, sig, nil, 120).WithSOCAliases(map[string]string{"idp": "QCS6490"})
		srv := httptest.NewServer(h.Mux())
		defer srv.Close()
		resp := post(t, srv.URL+"/callbacks/v1/heartbeat", map[string]any{
			"client_id": "c1", "agent_version": "0.12.0",
			"devices": []map[string]any{
				{"serial": "825485946", "state": "IDLE",
					"props": map[string]any{"os": "linux", "soc": "idp", "abi": "arm64-v8a", "capabilities": []string{}}},
			},
			"active_task_ids": []string{},
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		devs, _ := s.ListFleet(ctx)
		if len(devs) != 1 || devs[0].SOC != "QCS6490" {
			t.Fatalf("devices = %+v, want soc=QCS6490", devs)
		}
		if devs[0].DisplayName != "QCS6490-825485946" {
			t.Errorf("display_name = %q, want QCS6490-825485946", devs[0].DisplayName)
		}
	})
	t.Run("未命中保留上报代号", func(t *testing.T) {
		s := store.NewMemStore()
		sig := &fakeSignaler{}
		h := New(s, sig, nil, 120).WithSOCAliases(map[string]string{"idp": "QCS6490"})
		srv := httptest.NewServer(h.Mux())
		defer srv.Close()
		resp := post(t, srv.URL+"/callbacks/v1/heartbeat", map[string]any{
			"client_id": "c1", "agent_version": "0.12.0",
			"devices": []map[string]any{
				{"serial": "2a7359cf", "state": "IDLE",
					"props": map[string]any{"os": "android", "soc": "bengal", "abi": "arm64-v8a", "capabilities": []string{}}},
			},
			"active_task_ids": []string{},
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		devs, _ := s.ListFleet(ctx)
		if len(devs) != 1 || devs[0].SOC != "bengal" {
			t.Errorf("devices = %+v, want 未命中保留 bengal", devs)
		}
	})
	t.Run("soc 归一化后能力按型号校准", func(t *testing.T) {
		s := store.NewMemStore()
		sig := &fakeSignaler{}
		h := New(s, sig, nil, 120).
			WithSOCAliases(map[string]string{"idp": "QCS6490"}).
			WithDeviceCaps(map[string][]string{"qcs6490": {"hexagon", "adreno"}})
		srv := httptest.NewServer(h.Mux())
		defer srv.Close()
		resp := post(t, srv.URL+"/callbacks/v1/heartbeat", map[string]any{
			"client_id": "c1", "agent_version": "0.12.0",
			"devices": []map[string]any{
				{"serial": "825485946", "state": "IDLE",
					"props": map[string]any{"os": "linux", "soc": "idp", "abi": "arm64-v8a", "capabilities": []string{}}},
			},
			"active_task_ids": []string{},
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		devs, _ := s.ListFleet(ctx)
		if len(devs) != 1 || len(devs[0].Capabilities) != 2 || devs[0].Capabilities[0] != "hexagon" {
			t.Errorf("devices = %+v, want 归一化后按 QCS6490 校准 hexagon+adreno", devs)
		}
	})
}
