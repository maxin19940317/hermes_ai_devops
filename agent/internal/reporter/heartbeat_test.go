package reporter

import (
	"context"
	"testing"
	"time"

	"hermes-devops/agent/internal/adb"
)

// heartbeatRunner 三台设备:SERIAL1 可达(busy),SERIAL2 可达(idle,
// platform 为空回退 ro.product.board),SERIAL3 getprop 不可达(offline)。
func heartbeatRunner() *fakeRunner {
	return &fakeRunner{responses: map[string]adb.Result{
		"devices -l": {Stdout: "List of devices attached\n" +
			"SERIAL1 device product:p1 model:m1 device:d1 transport_id:1\n" +
			"SERIAL2 device product:p2 model:m2 device:d2 transport_id:2\n" +
			"SERIAL3 device product:p3 model:m3 device:d3 transport_id:3\n" +
			"DEAD0  offline transport_id:4\n"}, // offline 条目被 ParseDevices 过滤
		"-s SERIAL1 shell getprop ro.product.cpu.abi":       {Stdout: "arm64-v8a\n"},
		"-s SERIAL1 shell getprop ro.build.version.release": {Stdout: "13\n"},
		"-s SERIAL1 shell getprop ro.board.platform":        {Stdout: "trinket\n"},
		"-s SERIAL1 shell df -k /data/local/tmp": {Stdout: "Filesystem 1K-blocks Used Available Use% Mounted on\n" +
			"/dev/block/dm-4 11585580 6435440 5150140 56% /data\n"},
		"-s SERIAL2 shell getprop ro.product.cpu.abi":       {Stdout: "armeabi-v7a\n"},
		"-s SERIAL2 shell getprop ro.build.version.release": {Stdout: "12\n"},
		"-s SERIAL2 shell getprop ro.board.platform":        {Stdout: "\n"}, // 空 → 回退 board
		"-s SERIAL2 shell getprop ro.product.board":         {Stdout: "msm8937\n"},
		// SERIAL2 的 df 未登记:探测失败时 workdir_free_mb 应省略
		"-s SERIAL3 shell getprop ro.product.cpu.abi": {ExitCode: 1, Stderr: "error: device offline"},
	}}
}

func newTestHeartbeat(t *testing.T, baseURL string) *Heartbeat {
	t.Helper()
	s := openTempStore(t)
	seedTask(t, s, "t1", "wf1:t1:a1", "SERIAL1") // QUEUED → 进行中,SERIAL1 应判 BUSY
	return &Heartbeat{
		Runner:       heartbeatRunner(),
		Store:        s,
		Client:       &Client{BaseURL: baseURL},
		ClientID:     "client-1",
		AgentVersion: "0.1.0",
		BaseURL:      "http://client-host:8080",
	}
}

func TestHeartbeatPayloadShape(t *testing.T) {
	f, srv := newFakeRuntime(t)
	h := newTestHeartbeat(t, srv.URL)

	if err := h.once(context.Background()); err != nil {
		t.Fatalf("once: %v", err)
	}

	heartbeats, _, _ := f.snapshot()
	if len(heartbeats) != 1 {
		t.Fatalf("got %d heartbeats, want 1", len(heartbeats))
	}
	hb := heartbeats[0]
	if hb["client_id"] != "client-1" || hb["agent_version"] != "0.1.0" {
		t.Errorf("client identity mismatch: %v", hb)
	}
	if hb["base_url"] != "http://client-host:8080" {
		t.Errorf("base_url = %v", hb["base_url"])
	}
	ts, _ := hb["ts"].(string)
	if !tsPattern.MatchString(ts) {
		t.Errorf("ts = %q, want UTC millisecond ISO-8601", ts)
	}

	ids, _ := hb["active_task_ids"].([]any)
	if len(ids) != 1 || ids[0] != "t1" {
		t.Errorf("active_task_ids = %v, want [t1]", ids)
	}

	devs, _ := hb["devices"].([]any)
	if len(devs) != 3 {
		t.Fatalf("devices = %v, want 3 entries (offline adb 条目被过滤)", devs)
	}
	bySerial := map[string]map[string]any{}
	for _, d := range devs {
		m, _ := d.(map[string]any)
		bySerial[m["serial"].(string)] = m
	}

	d1 := bySerial["SERIAL1"]
	if d1["state"] != "BUSY" {
		t.Errorf("SERIAL1 state = %v, want BUSY(有进行中任务)", d1["state"])
	}
	props1, _ := d1["props"].(map[string]any)
	if props1["abi"] != "arm64-v8a" || props1["android"] != "13" || props1["soc"] != "trinket" {
		t.Errorf("SERIAL1 props = %v", props1)
	}
	if mb, ok := d1["workdir_free_mb"].(float64); !ok || int64(mb) != 5029 {
		t.Errorf("SERIAL1 workdir_free_mb = %v, want 5029 (5150140KB/1024)", d1["workdir_free_mb"])
	}

	d2 := bySerial["SERIAL2"]
	if d2["state"] != "IDLE" {
		t.Errorf("SERIAL2 state = %v, want IDLE", d2["state"])
	}
	props2, _ := d2["props"].(map[string]any)
	if props2["soc"] != "msm8937" {
		t.Errorf("SERIAL2 soc = %v, want msm8937 (platform 空回退 ro.product.board)", props2["soc"])
	}
	if _, present := d2["workdir_free_mb"]; present {
		t.Errorf("SERIAL2 df 探测失败时 workdir_free_mb 应省略, got %v", d2["workdir_free_mb"])
	}

	d3 := bySerial["SERIAL3"]
	if d3["state"] != "OFFLINE" {
		t.Errorf("SERIAL3 state = %v, want OFFLINE(getprop 不可达)", d3["state"])
	}
	if _, present := d3["props"]; present {
		t.Errorf("SERIAL3 OFFLINE 不应带 props")
	}
}

func TestHeartbeatBackoffOnFailure(t *testing.T) {
	f, srv := newFakeRuntime(t)
	f.heartbeatStatus = 500
	h := newTestHeartbeat(t, srv.URL)
	h.Interval = 10 * time.Millisecond
	h.MaxWait = 40 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	time.Sleep(130 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	times := f.heartbeatAttempts()
	// 退避间隔 10→20→40→40…,130ms 窗口约 5 次;无退避应 ~13 次
	if len(times) < 3 || len(times) > 8 {
		t.Fatalf("attempts = %d, want 3..8 (退避生效)", len(times))
	}
	// Go timer 不会提前触发:第二次失败后的间隔应 ≥ 20ms 标称
	if gap := times[2].Sub(times[1]); gap < 15*time.Millisecond {
		t.Errorf("gap[1→2] = %v, want ≥ ~20ms (指数退避)", gap)
	}
}

func TestHeartbeatFailureRecoveryResetsBackoff(t *testing.T) {
	f, srv := newFakeRuntime(t)
	h := newTestHeartbeat(t, srv.URL)
	h.Interval = 10 * time.Millisecond
	h.MaxWait = 80 * time.Millisecond

	f.mu.Lock()
	f.heartbeatStatus = 500 // 先失败一次
	f.mu.Unlock()
	if err := h.once(context.Background()); err == nil {
		t.Fatal("want heartbeat failure")
	}

	// 之后全部成功且应答仅含 ok(其余字段缺失也容忍)
	f.mu.Lock()
	f.heartbeatStatus = 0
	f.heartbeatBody = `{"ok":true}`
	f.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	// 成功路径退避复位:100ms / 10ms ≈ 10 次;若仍退避最多 ~5 次
	if n := len(f.heartbeatAttempts()); n < 7 {
		t.Errorf("attempts = %d, want ≥ 7 (成功后退避复位,ok-only ack 算成功)", n)
	}
}

func TestHeartbeatStopsOnContextCancel(t *testing.T) {
	_, srv := newFakeRuntime(t)
	h := newTestHeartbeat(t, srv.URL)
	h.Interval = time.Hour // 大周期:取消必须立即返回,不等 tick

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	time.Sleep(20 * time.Millisecond) // 让第一次心跳发完进入等待
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
