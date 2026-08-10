package reporter

import (
	"context"
	"fmt"
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
		"-s SERIAL1 shell /system/bin/getprop ro.product.cpu.abi":       {Stdout: "arm64-v8a\n"},
		"-s SERIAL1 shell /system/bin/getprop ro.build.version.release": {Stdout: "13\n"},
		"-s SERIAL1 shell /system/bin/getprop ro.board.platform":        {Stdout: "trinket\n"},
		"-s SERIAL1 shell /system/bin/df -k /data/local/tmp": {Stdout: "Filesystem 1K-blocks Used Available Use% Mounted on\n" +
			"/dev/block/dm-4 11585580 6435440 5150140 56% /data\n"},
		"-s SERIAL2 shell /system/bin/getprop ro.product.cpu.abi":       {Stdout: "armeabi-v7a\n"},
		"-s SERIAL2 shell /system/bin/getprop ro.build.version.release": {Stdout: "12\n"},
		"-s SERIAL2 shell /system/bin/getprop ro.board.platform":        {Stdout: "\n"}, // 空 → 回退 board
		"-s SERIAL2 shell /system/bin/getprop ro.product.board":         {Stdout: "msm8937\n"},
		// SERIAL2 的 df 未登记:探测失败时 workdir_free_mb 应省略
		"-s SERIAL3 shell /system/bin/getprop ro.product.cpu.abi": {ExitCode: 1, Stderr: "error: device offline"},
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
	start := time.Now()
	go func() { done <- h.Run(ctx) }()
	time.Sleep(130 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	elapsed := time.Since(start)

	times := f.heartbeatAttempts()
	// 退避间隔 10→20→40→40…;至少应有 3 次(10+20ms 就够了)。
	// 上限用实际耗时计算:若无退避每 10ms 一次,上限 = elapsed/10ms+2;
	// 有退避上限远低于此;不用硬编码避免 CI 低速机器 sleep 过久导致误判。
	if len(times) < 3 {
		t.Fatalf("attempts = %d, want ≥ 3 (退避未充分触发)", len(times))
	}
	maxWithoutBackoff := int(elapsed/h.Interval) + 2
	if len(times) > maxWithoutBackoff {
		t.Fatalf("attempts = %d > %d (elapsed=%v), backoff 未生效", len(times), maxWithoutBackoff, elapsed)
	}
	// 第二次失败后的间隔应 ≥ 20ms 标称:验证指数退避真正生效
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

// 混合格式心跳(差距 #15,滚动升级关键):带租约凭据的任务发对象
// {task_id, attempt, lease_id, lease_generation}(attempt 从 task_id 的
// :a{N} 后缀解析),无凭据任务发原字符串——对旧 Runtime(任务本无凭据)
// 载荷与旧格式等价;对新 Runtime 条件续租生效。任何部署顺序都安全。
func TestHeartbeatMixedFormats(t *testing.T) {
	f, srv := newFakeRuntime(t)
	h := newTestHeartbeat(t, srv.URL)
	// newTestHeartbeat 已 seed "t1"(无凭据,旧格式);再 seed 两个带凭据任务
	seedTaskWithLease(t, h.Store, "wf1:t2:a2", "wf1:t2:a2", "SERIAL2", "wf1:t2:a2", 3)
	seedTaskWithLease(t, h.Store, "wf1:t3:a1", "wf1:t3:a1", "SERIAL2", "wf1:t3:a1", 1)

	if err := h.once(context.Background()); err != nil {
		t.Fatalf("once: %v", err)
	}
	heartbeats, _, _ := f.snapshot()
	if len(heartbeats) != 1 {
		t.Fatalf("heartbeats = %d", len(heartbeats))
	}
	ids, _ := heartbeats[0]["active_task_ids"].([]any)
	if len(ids) != 3 {
		t.Fatalf("active_task_ids = %v, want 3 项", ids)
	}
	var legacy, objects []any
	for _, id := range ids {
		if _, ok := id.(string); ok {
			legacy = append(legacy, id)
		} else {
			objects = append(objects, id)
		}
	}
	if len(legacy) != 1 || legacy[0] != "t1" {
		t.Errorf("旧格式项 = %v, want [t1]", legacy)
	}
	if len(objects) != 2 {
		t.Fatalf("对象格式项 = %v, want 2", objects)
	}
	got := map[string]map[string]any{}
	for _, o := range objects {
		m, _ := o.(map[string]any)
		got[m["task_id"].(string)] = m
	}
	t2 := got["wf1:t2:a2"]
	if t2 == nil || t2["lease_id"] != "wf1:t2:a2" ||
		t2["lease_generation"] != float64(3) || t2["attempt"] != float64(2) {
		t.Errorf("wf1:t2:a2 凭据项 = %v(attempt 应从 :a2 后缀解析)", t2)
	}
	t3 := got["wf1:t3:a1"]
	if t3 == nil || t3["lease_generation"] != float64(1) || t3["attempt"] != float64(1) {
		t.Errorf("wf1:t3:a1 凭据项 = %v", t3)
	}
}

// 故障注入:心跳应答带 not_owned(LEASE_NOT_OWNED)→ 必须逐项调 StopTask
// 停止本地执行(租约易主防线,§10);无 StopTask 钩子时只记日志不 panic。
func TestHeartbeatNotOwnedStopsTask(t *testing.T) {
	f, srv := newFakeRuntime(t)
	f.heartbeatBody = `{"ok":true,"not_owned":[{"task_id":"t1","code":"LEASE_NOT_OWNED"}]}`
	h := newTestHeartbeat(t, srv.URL)

	var stopped []string
	h.StopTask = func(taskID string) { stopped = append(stopped, taskID) }
	if err := h.once(context.Background()); err != nil {
		t.Fatalf("once: %v", err)
	}
	if len(stopped) != 1 || stopped[0] != "t1" {
		t.Errorf("stopped = %v, want [t1]", stopped)
	}

	// nil StopTask:容忍(仅记日志),不得 panic/报错
	h.StopTask = nil
	if err := h.once(context.Background()); err != nil {
		t.Fatalf("once without StopTask: %v", err)
	}
}

func TestHeartbeatDeviceDiffLogging(t *testing.T) {
	var logs []string
	logf := func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	// 场景1: 首次心跳,输出设备列表确认(不输出 diff)
	h1 := &Heartbeat{
		ClientID: "test-client",
		Logf:     logf,
	}
	devs1 := []DeviceInfo{
		{Serial: "SERIAL1"},
		{Serial: "SERIAL2"},
	}
	h1.diffDevices(devs1)
	if len(logs) != 1 {
		t.Errorf("首次心跳应输出 1 条设备列表, got %d: %v", len(logs), logs)
	}
	if h1.lastDevices == nil || !h1.lastDevices["SERIAL1"] || !h1.lastDevices["SERIAL2"] {
		t.Errorf("lastDevices 未正确初始化: %v", h1.lastDevices)
	}

	// 场景2: 设备移除
	logs = nil
	devs2 := []DeviceInfo{{Serial: "SERIAL1"}}
	h1.diffDevices(devs2)
	if len(logs) != 1 || logs[0] != "device removed: SERIAL2" {
		t.Errorf("移除设备日志错误, got %v", logs)
	}

	// 场景3: 设备新增
	logs = nil
	devs3 := []DeviceInfo{{Serial: "SERIAL1"}, {Serial: "SERIAL3"}}
	h1.diffDevices(devs3)
	if len(logs) != 1 || logs[0] != "device added: SERIAL3" {
		t.Errorf("新增设备日志错误, got %v", logs)
	}

	// 场景4: 同时新增和移除
	logs = nil
	devs4 := []DeviceInfo{{Serial: "SERIAL3"}, {Serial: "SERIAL4"}}
	h1.diffDevices(devs4)
	// 顺序不确定,检查内容
	hasRemoved := false
	hasAdded := false
	for _, l := range logs {
		if l == "device removed: SERIAL1" {
			hasRemoved = true
		}
		if l == "device added: SERIAL4" {
			hasAdded = true
		}
	}
	if !hasRemoved || !hasAdded || len(logs) != 2 {
		t.Errorf("同时新增移除设备日志错误, got %v", logs)
	}

	// 场景5: 无变化
	logs = nil
	devs5 := []DeviceInfo{{Serial: "SERIAL3"}, {Serial: "SERIAL4"}}
	h1.diffDevices(devs5)
	if len(logs) != 0 {
		t.Errorf("无变化时不应输出日志, got %v", logs)
	}
}
