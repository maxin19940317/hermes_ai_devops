package activity

import (
	"testing"

	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

func storeWithDevice(t *testing.T) *store.MemStore {
	t.Helper()
	s := store.NewMemStore()
	err := s.UpsertClientDevices(ctx,
		store.Client{ClientID: "c1", BaseURL: "https://client:8443"},
		[]store.Device{{DeviceID: "513cd3de", Serial: "513cd3de", ClientID: "c1",
			SOC: "QCM6125", ABI: "arm64-v8a", Capabilities: []string{"hexagon"}}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStoreActsPassConfigThrough(t *testing.T) {
	s := storeWithDevice(t)
	a := &Acts{Store: s, Cfg: Config{LeaseSeconds: 120, QuarantineAfter: 3}}

	l, err := a.AcquireDevice(ctx, wf.AcquireRequest{TaskID: "t1",
		Selector: wf.DeviceSelector{SOC: []string{"QCM6125"}}})
	if err != nil || l == nil || l.ClientBaseURL != "https://client:8443" {
		t.Fatalf("lease=%+v err=%v", l, err)
	}
	if err := a.CreateTask(ctx, wf.TaskRow{TaskID: "t1", IdempotencyKey: "t1", Status: "DISPATCHING"}); err != nil {
		t.Fatal(err)
	}
	if err := a.FinishTask(ctx, wf.FinishRequest{TaskID: "t1", Status: "COMPLETED", Verdict: "PASSED"}); err != nil {
		t.Fatal(err)
	}
	if err := a.ReleaseDevice(ctx, wf.ReleaseRequest{DeviceID: l.DeviceID, TaskID: "t1", InfraFail: false}); err != nil {
		t.Fatal(err)
	}
	// QuarantineAfter=3 生效:连续 3 次 INFRA 释放后设备隔离
	for i := 0; i < 3; i++ {
		l, _ := a.AcquireDevice(ctx, wf.AcquireRequest{TaskID: "tx"})
		if l == nil {
			t.Fatalf("第 %d 次应能获取", i+1)
		}
		_ = a.ReleaseDevice(ctx, wf.ReleaseRequest{DeviceID: l.DeviceID, TaskID: "tx", InfraFail: true})
	}
	if l, _ := a.AcquireDevice(ctx, wf.AcquireRequest{TaskID: "ty"}); l != nil {
		t.Error("连续 3 次 INFRA 后设备应 QUARANTINED(§10)")
	}
}
