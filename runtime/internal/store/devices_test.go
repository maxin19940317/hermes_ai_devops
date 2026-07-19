package store

import (
	"context"
	"testing"

	wf "hermes-devops/runtime/internal/workflow"
)

var ctx = context.Background()

func heartbeatClient(t *testing.T, s *MemStore) {
	t.Helper()
	err := s.UpsertClientDevices(ctx,
		Client{ClientID: "c1", Host: "SH-D-007631A", Version: "0.1.0", BaseURL: "https://client:8443"},
		[]Device{
			{DeviceID: "513cd3de", Serial: "513cd3de", ClientID: "c1", SOC: "trinket",
				ABI: "arm64-v8a", Capabilities: []string{"hexagon"}},
		})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAcquireMatchesSelectorAndLocks(t *testing.T) {
	s := NewMemStore()
	heartbeatClient(t, s)

	// soc 不匹配 → 无设备
	if l, err := s.AcquireDevice(ctx, wf.DeviceSelector{SOC: []string{"RK3588"}}, "t1", 120); err != nil || l != nil {
		t.Fatalf("lease=%v err=%v, want nil(soc 不匹配)", l, err)
	}
	// 大小写不敏感匹配 + capabilities 子集
	l, err := s.AcquireDevice(ctx, wf.DeviceSelector{SOC: []string{"TRINKET"}, Capabilities: []string{"hexagon"}}, "t1", 120)
	if err != nil || l == nil {
		t.Fatalf("lease=%v err=%v", l, err)
	}
	if l.Serial != "513cd3de" || l.ClientBaseURL != "https://client:8443" {
		t.Errorf("lease = %+v", l)
	}
	// 已占用 → 二次获取无设备
	if l2, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t2", 120); l2 != nil {
		t.Errorf("BUSY 设备不得重复出租: %+v", l2)
	}
	// 释放后可再获取
	if err := s.ReleaseDevice(ctx, l.DeviceID, "t1", false, 3); err != nil {
		t.Fatal(err)
	}
	if l3, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t3", 120); l3 == nil {
		t.Error("释放后应可再次获取")
	}
}

func TestQuarantineAfterConsecutiveInfraFailures(t *testing.T) {
	s := NewMemStore()
	heartbeatClient(t, s)
	for i := 0; i < 3; i++ { // §10:连续 3 次 INFRA → QUARANTINED
		l, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t", 120)
		if l == nil {
			t.Fatalf("第 %d 次应能获取", i+1)
		}
		_ = s.ReleaseDevice(ctx, l.DeviceID, "t", true, 3)
	}
	if l, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t", 120); l != nil {
		t.Error("QUARANTINED 设备不得出租")
	}
}

func TestSuccessResetsFailStreak(t *testing.T) {
	s := NewMemStore()
	heartbeatClient(t, s)
	for i := 0; i < 2; i++ {
		l, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t", 120)
		_ = s.ReleaseDevice(ctx, l.DeviceID, "t", true, 3)
	}
	l, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t", 120)
	_ = s.ReleaseDevice(ctx, l.DeviceID, "t", false, 3) // 成功:清零
	for i := 0; i < 2; i++ {
		l, _ = s.AcquireDevice(ctx, wf.DeviceSelector{}, "t", 120)
		if l == nil {
			t.Fatal("fail_streak 清零后 2 次 INFRA 不应隔离")
		}
		_ = s.ReleaseDevice(ctx, l.DeviceID, "t", true, 3)
	}
}

func TestTaskLifecycleAndEventDedup(t *testing.T) {
	s := NewMemStore()
	row := wf.TaskRow{TaskID: "w:t1:a1", WorkflowID: "w", TestID: "t1", Attempt: 1,
		IdempotencyKey: "w:t1:a1", ClientID: "c1", DeviceID: "d1", Status: "DISPATCHING"}
	if err := s.CreateTask(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, row); err != nil { // 同幂等键重复创建:无副作用
		t.Fatalf("重复创建应幂等: %v", err)
	}
	got, err := s.GetTask(ctx, "w:t1:a1")
	if err != nil || got == nil || got.WorkflowID != "w" {
		t.Fatalf("get task = %+v, err=%v", got, err)
	}

	ins, err := s.AppendTaskEvent(ctx, TaskEvent{TaskID: "w:t1:a1", Seq: 1, From: "ACCEPTED", To: "RUNNING"})
	if err != nil || !ins {
		t.Fatalf("first event: ins=%v err=%v", ins, err)
	}
	ins, err = s.AppendTaskEvent(ctx, TaskEvent{TaskID: "w:t1:a1", Seq: 1, From: "ACCEPTED", To: "RUNNING"})
	if err != nil || ins {
		t.Fatalf("重复 seq 应去重: ins=%v err=%v", ins, err)
	}
	if err := s.SetTaskStatus(ctx, "w:t1:a1", "RUNNING"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTask(ctx, wf.FinishRequest{TaskID: "w:t1:a1", Status: "COMPLETED",
		Verdict: "PASSED", Category: "", Reason: "ok"}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTask(ctx, "w:t1:a1")
	if got.Status != "COMPLETED" {
		t.Errorf("status = %s", got.Status)
	}
}

func TestSaveResultDedup(t *testing.T) {
	s := NewMemStore()
	_ = s.CreateTask(ctx, wf.TaskRow{TaskID: "w:t1:a1", IdempotencyKey: "w:t1:a1"})
	rec := wf.ResultRecord{TaskID: "w:t1:a1", Result: wf.TaskResultSignal{TaskID: "w:t1:a1", Status: "COMPLETED"}}
	ins, err := s.SaveResult(ctx, rec)
	if err != nil || !ins {
		t.Fatalf("first save: ins=%v err=%v", ins, err)
	}
	ins, err = s.SaveResult(ctx, rec) // 回调重发
	if err != nil || ins {
		t.Fatalf("重复结果应去重: ins=%v err=%v", ins, err)
	}
}
