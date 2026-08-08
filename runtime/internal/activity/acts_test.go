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
//
// none 与"旧载荷 InfraFail=false"(翻译为 ok)的差别只有在计数器非零时才
// 可观察,所以每个子用例先用 device/client scope 把两个计数器都垫到 1,
// 再对种子后的状态跑被测载荷;否则 none(不动)与被误翻译成 ok(清零)
// 在从 (0,0) 出发时都停在 (0,0),测不出回归。QuarantineAfter 维持 3:
// 种子的 1 次 device 释放 + device 用例自身再 1 次 = 2,仍低于阈值。
func TestReleaseDeviceScopeTranslation(t *testing.T) {
	cases := []struct {
		name           string
		req            wf.ReleaseRequest // DeviceID/TaskID 由用例填
		wantDeviceFail int
		wantClientFail int
	}{
		{"新载荷 client", wf.ReleaseRequest{FailScope: wf.FailScopeClient}, 1, 2},
		{"新载荷 none 不被当成空", wf.ReleaseRequest{FailScope: wf.FailScopeNone}, 1, 1},
		{"旧载荷 InfraFail=true → device", wf.ReleaseRequest{InfraFail: true}, 2, 1},
		{"旧载荷 InfraFail=false → ok", wf.ReleaseRequest{InfraFail: false}, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := storeWithDevice(t)
			a := &Acts{Store: s, Cfg: Config{LeaseSeconds: 120, QuarantineAfter: 3}}

			// 种子:设备计数、client 计数各垫到 1(用 store 自身的 scope-aware
			// release,不碰内部字段)。
			seedDev, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "seed-device", 120)
			if err != nil || seedDev == nil {
				t.Fatalf("seed device acquire: %+v %v", seedDev, err)
			}
			if err := s.ReleaseDevice(ctx, seedDev.DeviceID, "seed-device", wf.FailScopeDevice, a.Cfg.QuarantineAfter); err != nil {
				t.Fatalf("seed device release: %v", err)
			}
			seedClient, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "seed-client", 120)
			if err != nil || seedClient == nil {
				t.Fatalf("seed client acquire: %+v %v", seedClient, err)
			}
			if err := s.ReleaseDevice(ctx, seedClient.DeviceID, "seed-client", wf.FailScopeClient, a.Cfg.QuarantineAfter); err != nil {
				t.Fatalf("seed client release: %v", err)
			}

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

// storeWithClientVersion 注册一台设备并指定 client 上报的版本。
func storeWithClientVersion(t *testing.T, version string) *store.MemStore {
	t.Helper()
	s := store.NewMemStore()
	err := s.UpsertClientDevices(ctx,
		store.Client{ClientID: "c1", BaseURL: "https://client:8443", Version: version},
		[]store.Device{{DeviceID: "513cd3de", Serial: "513cd3de", ClientID: "c1",
			SOC: "QCM6125", ABI: "arm64-v8a", Capabilities: []string{"hexagon"}}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A3:CLAUDE.md §12 Phase 3 明文 "MIN_AGENT_VERSION 空=不启用,dev 永远放行"。
// compareVersion 把 dev 排在任何正式版本之下,所以门禁必须显式豁免;
// 否则未打 ldflags 的 Agent 在设了 MIN_AGENT_VERSION 的部署上每次 acquire
// 都占租约再释放,重试耗尽后全部落 INFRA_ERROR。
func TestAcquireDeviceVersionGate(t *testing.T) {
	cases := []struct {
		name      string
		clientVer string
		minVer    string
		wantLease bool
	}{
		{"dev 永远放行", "dev", "0.1.0", true},
		{"门禁未启用时放行任意版本", "0.0.1", "", true},
		{"正式版本达标放行", "0.2.0", "0.1.0", true},
		{"正式版本过低拒绝", "0.0.9", "0.1.0", false},
		// git describe --always 在无 tag 仓库上给出裸 commit hash,
		// 会被解析成 0.0.0——这不是 dev,应照常被门禁拦下。
		{"裸 commit hash 按 0.0.0 处理并拒绝", "abc1234", "0.1.0", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := storeWithClientVersion(t, tc.clientVer)
			a := &Acts{Store: s, Cfg: Config{
				LeaseSeconds: 120, QuarantineAfter: 3, MinAgentVersion: tc.minVer,
			}}
			l, err := a.AcquireDevice(ctx, wf.AcquireRequest{TaskID: "t1",
				Selector: wf.DeviceSelector{SOC: []string{"QCM6125"}}})
			if tc.wantLease {
				if err != nil || l == nil {
					t.Fatalf("client version %q / min %q 应放行, got lease=%+v err=%v",
						tc.clientVer, tc.minVer, l, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("client version %q / min %q 应被拒绝, got lease=%+v",
					tc.clientVer, tc.minVer, l)
			}
		})
	}
}
