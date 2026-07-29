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

// 在途 workflow 重放会送来没有 FailScope 的旧载荷,活动必须按旧语义翻译,
// 否则重放期间的记账与当初不一致(设计文档 §5)。
func TestReleaseDeviceScopeTranslation(t *testing.T) {
	cases := []struct {
		name           string
		req            wf.ReleaseRequest // DeviceID/TaskID 由用例填
		wantDeviceFail int
		wantClientFail int
	}{
		{"新载荷 client", wf.ReleaseRequest{FailScope: wf.FailScopeClient}, 0, 1},
		{"新载荷 none 不被当成空", wf.ReleaseRequest{FailScope: wf.FailScopeNone}, 0, 0},
		{"旧载荷 InfraFail=true → device", wf.ReleaseRequest{InfraFail: true}, 1, 0},
		{"旧载荷 InfraFail=false → ok", wf.ReleaseRequest{InfraFail: false}, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := storeWithDevice(t)
			a := &Acts{Store: s, Cfg: Config{LeaseSeconds: 120, QuarantineAfter: 3}}
			l, err := a.AcquireDevice(ctx, wf.AcquireRequest{TaskID: "t1"})
			if err != nil || l == nil {
				t.Fatalf("acquire: %+v %v", l, err)
			}
			req := tc.req
			req.DeviceID, req.TaskID = l.DeviceID, "t1"
			if err := a.ReleaseDevice(ctx, req); err != nil {
				t.Fatalf("ReleaseDevice: %v", err)
			}
			ov, err := s.FleetOverview(ctx)
			if err != nil {
				t.Fatal(err)
			}
			d := ov.Devices[0]
			if d.FailStreak != tc.wantDeviceFail || d.ClientFailStreak != tc.wantClientFail {
				t.Errorf("device=%d client=%d, want device=%d client=%d",
					d.FailStreak, d.ClientFailStreak, tc.wantDeviceFail, tc.wantClientFail)
			}
		})
	}
}
