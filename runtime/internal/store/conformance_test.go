package store

import (
	"context"
	"sync"
	"testing"

	wf "hermes-devops/runtime/internal/workflow"
)

var ctx = context.Background()

// fullStore 是 workflow 活动(internal/activity.Store)与回调服务
// (internal/callbacks.Store)所需持久层方法的并集;
// MemStore 与 PGStore 必须行为一致,由本套件保证。
type fullStore interface {
	UpsertClientDevices(ctx context.Context, c Client, devs []Device) error
	AcquireDevice(ctx context.Context, sel wf.DeviceSelector, taskID string, leaseSeconds int) (*wf.Lease, error)
	ReleaseDevice(ctx context.Context, deviceID, taskID string, infraFail bool, quarantineAfter int) error
	CreateTask(ctx context.Context, row wf.TaskRow) error
	GetTask(ctx context.Context, taskID string) (*wf.TaskRow, error)
	SetTaskStatus(ctx context.Context, taskID, status string) error
	FinishTask(ctx context.Context, req wf.FinishRequest) error
	AppendTaskEvent(ctx context.Context, ev TaskEvent) (bool, error)
	SaveResult(ctx context.Context, rec wf.ResultRecord) (bool, error)
}

func TestMemStoreConformance(t *testing.T) {
	runConformance(t, func(t *testing.T) fullStore { return NewMemStore() })
}

func TestPGStoreConformance(t *testing.T) {
	runConformance(t, func(t *testing.T) fullStore { return openTestPG(t) })
}

// runConformance 对一个空 store 实例跑全部行为断言;
// newStore 每个子测试调用一次,必须返回干净实例。
func runConformance(t *testing.T, newStore func(t *testing.T) fullStore) {
	seed := func(t *testing.T, s fullStore) {
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

	t.Run("AcquireMatchesSelectorAndLocks", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		// soc 不匹配 → 无设备
		if l, err := s.AcquireDevice(ctx, wf.DeviceSelector{SOC: []string{"RK3588"}}, "t1", 120); err != nil || l != nil {
			t.Fatalf("lease=%v err=%v, want nil(soc 不匹配)", l, err)
		}
		// capabilities 非子集 → 无设备
		if l, err := s.AcquireDevice(ctx, wf.DeviceSelector{Capabilities: []string{"npu"}}, "t1", 120); err != nil || l != nil {
			t.Fatalf("lease=%v err=%v, want nil(capabilities 不满足)", l, err)
		}
		// 大小写不敏感匹配 + capabilities 子集
		l, err := s.AcquireDevice(ctx, wf.DeviceSelector{SOC: []string{"TRINKET"}, Capabilities: []string{"hexagon"}}, "t1", 120)
		if err != nil || l == nil {
			t.Fatalf("lease=%v err=%v", l, err)
		}
		if l.DeviceID != "513cd3de" || l.Serial != "513cd3de" || l.ClientID != "c1" ||
			l.ClientBaseURL != "https://client:8443" {
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
	})

	t.Run("UpsertAcceptsNilCapabilities", func(t *testing.T) {
		// 心跳的 props.capabilities 可能整体缺省(JSON 省略字段 → Go nil slice);
		// 没有特殊能力的板子是完全正常的情况,不得导致整条心跳失败。
		s := newStore(t)
		err := s.UpsertClientDevices(ctx,
			Client{ClientID: "c1", BaseURL: "https://client:8443"},
			[]Device{{DeviceID: "d-nilcaps", Serial: "d-nilcaps", ClientID: "c1", SOC: "plain"}})
		if err != nil {
			t.Fatalf("nil capabilities 心跳不应报错: %v", err)
		}
		l, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t1", 120)
		if err != nil || l == nil || l.DeviceID != "d-nilcaps" {
			t.Fatalf("lease=%v err=%v", l, err)
		}
	})

	t.Run("HeartbeatMustNotResetBusyState", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		l, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t1", 120)
		if err != nil || l == nil {
			t.Fatalf("lease=%v err=%v", l, err)
		}
		seed(t, s) // 心跳重注册:只刷新属性,不得把 BUSY 刷回 IDLE
		if l2, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t2", 120); l2 != nil {
			t.Errorf("心跳后 BUSY 设备被重新出租: %+v", l2)
		}
	})

	t.Run("ReleaseIsIdempotentAndOwnerChecked", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		l, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t1", 120)
		if l == nil {
			t.Fatal("no lease")
		}
		// 非持有者释放:无副作用
		if err := s.ReleaseDevice(ctx, l.DeviceID, "other-task", true, 3); err != nil {
			t.Fatal(err)
		}
		if l2, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t2", 120); l2 != nil {
			t.Fatalf("非持有者释放不得生效: %+v", l2)
		}
		// 持有者释放 + 重复释放幂等
		if err := s.ReleaseDevice(ctx, l.DeviceID, "t1", false, 3); err != nil {
			t.Fatal(err)
		}
		if err := s.ReleaseDevice(ctx, l.DeviceID, "t1", true, 3); err != nil {
			t.Fatal(err) // 重复释放(infraFail=true)不得计入 fail_streak
		}
		l3, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t3", 120)
		if l3 == nil {
			t.Fatal("释放后应可获取")
		}
	})

	t.Run("QuarantineAfterConsecutiveInfraFailures", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
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
		seed(t, s) // 心跳不得解除隔离(§11 devices.status 语义)
		if l, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t", 120); l != nil {
			t.Error("心跳后 QUARANTINED 设备被出租")
		}
	})

	t.Run("SuccessResetsFailStreak", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
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
	})

	t.Run("ConcurrentAcquireGrantsSingleLease", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		const n = 8
		var wg sync.WaitGroup
		leases := make([]*wf.Lease, n)
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				leases[i], errs[i] = s.AcquireDevice(ctx, wf.DeviceSelector{}, "t", 120)
			}(i)
		}
		wg.Wait()
		granted := 0
		for i := 0; i < n; i++ {
			if errs[i] != nil {
				t.Fatalf("acquire #%d: %v", i, errs[i])
			}
			if leases[i] != nil {
				granted++
			}
		}
		if granted != 1 {
			t.Errorf("granted = %d, want 1(租约独占,§11 行锁)", granted)
		}
	})

	t.Run("TaskLifecycleAndEventDedup", func(t *testing.T) {
		s := newStore(t)
		row := wf.TaskRow{TaskID: "w:t1:a1", WorkflowID: "w", TestID: "t1", Attempt: 1,
			IdempotencyKey: "w:t1:a1", ClientID: "c1", DeviceID: "d1", Status: "DISPATCHING"}
		if err := s.CreateTask(ctx, row); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateTask(ctx, row); err != nil { // 同幂等键重复创建:无副作用
			t.Fatalf("重复创建应幂等: %v", err)
		}
		got, err := s.GetTask(ctx, "w:t1:a1")
		if err != nil || got == nil {
			t.Fatalf("get task = %+v, err=%v", got, err)
		}
		if got.WorkflowID != "w" || got.TestID != "t1" || got.Attempt != 1 ||
			got.ClientID != "c1" || got.DeviceID != "d1" || got.Status != "DISPATCHING" {
			t.Errorf("task row = %+v", got)
		}
		if missing, err := s.GetTask(ctx, "no-such"); err != nil || missing != nil {
			t.Errorf("未知任务应返回 (nil, nil): %+v %v", missing, err)
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
	})

	t.Run("SaveResultDedup", func(t *testing.T) {
		s := newStore(t)
		_ = s.CreateTask(ctx, wf.TaskRow{TaskID: "w:t1:a1", IdempotencyKey: "w:t1:a1"})
		rec := wf.ResultRecord{TaskID: "w:t1:a1", Result: wf.TaskResultSignal{
			TaskID: "w:t1:a1", Status: "COMPLETED", ExitCode: 0, DurationSec: 412,
			CasesTotal: 38, CasesFailed: 0, SignaturesHit: []string{},
			Metrics:     map[string]float64{"latency_ms_p50": 12.4},
			Attachments: []wf.Attachment{{Name: "logcat.txt", ObjectKey: "runs/x/logcat.txt", SHA256: "s", Size: 9}},
		}}
		ins, err := s.SaveResult(ctx, rec)
		if err != nil || !ins {
			t.Fatalf("first save: ins=%v err=%v", ins, err)
		}
		ins, err = s.SaveResult(ctx, rec) // 回调重发
		if err != nil || ins {
			t.Fatalf("重复结果应去重: ins=%v err=%v", ins, err)
		}
	})
}
