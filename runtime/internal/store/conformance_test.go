package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	wf "hermes-devops/runtime/internal/workflow"
)

var ctx = context.Background()

// fullStore 是 workflow 活动(internal/activity.Store)与回调服务
// (internal/callbacks.Store)所需持久层方法的并集;
// MemStore 与 PGStore 必须行为一致,由本套件保证。
type fullStore interface {
	RegisterArtifacts(ctx context.Context, arts []Artifact) error
	UpsertClientDevices(ctx context.Context, c Client, devs []Device) error
	AcquireDevice(ctx context.Context, sel wf.DeviceSelector, taskID string, leaseSeconds int) (*wf.Lease, error)
	ReleaseDevice(ctx context.Context, deviceID, taskID string, scope wf.FailScope, quarantineAfter int) error
	RenewLease(ctx context.Context, cred LeaseCredential, leaseSeconds int) (bool, error)
	VerifyLease(ctx context.Context, cred LeaseCredential) (bool, error)
	GetLeaseExpiry(ctx context.Context, taskID string) (*time.Time, error)
	HasCapableDevice(ctx context.Context, sel wf.DeviceSelector) (bool, error)
	CreateTask(ctx context.Context, row wf.TaskRow) error
	GetTask(ctx context.Context, taskID string) (*wf.TaskRow, error)
	SetTaskStatus(ctx context.Context, taskID, status string) error
	FinishTask(ctx context.Context, req wf.FinishRequest) error
	ConclusiveWorkflowIDs(ctx context.Context, workflowIDs []string) (map[string]bool, error)
	AppendTaskEvent(ctx context.Context, ev TaskEvent) (bool, error)
	SaveResult(ctx context.Context, rec wf.ResultRecord) (bool, error)
	SaveResultWithOutbox(ctx context.Context, rec wf.ResultRecord, ev OutboxEvent) (bool, error)
	GetResult(ctx context.Context, taskID string) (*wf.ResultRecord, error)
	ClaimUnpublished(ctx context.Context, limit int) ([]OutboxEvent, error)
	MarkPublished(ctx context.Context, id int64) error
	MarkFailed(ctx context.Context, id int64, cause string) error
	OutboxBacklog(ctx context.Context, stuckAttempts int) (*OutboxBacklog, error)
	SaveDecision(ctx context.Context, row wf.DecisionRow) error
	ListDecisions(ctx context.Context, taskID string) ([]wf.DecisionRow, error)
	HasDecision(ctx context.Context, taskID, actor string) (bool, error)
	NextWorkflowAttempt(ctx context.Context, commitSHA string, pipelineID int, variant string) (int, error)
	SaveEvidenceSnapshot(ctx context.Context, snap EvidenceSnapshot) error
	GetEvidenceSnapshot(ctx context.Context, evidenceID string) (*EvidenceSnapshot, error)
	FleetOverview(ctx context.Context) (*FleetOverview, error)
	UnquarantineDevice(ctx context.Context, deviceID string) (bool, error)
	ListArtifacts(ctx context.Context, commitSHA string, pipelineID int) ([]Artifact, error)
	NextWorkflowAttemptAll(ctx context.Context, commitSHA string, pipelineID int) (int, error)
	SaveCommandTranslation(ctx context.Context, row CommandTranslation) error
	ListCommandTranslations(ctx context.Context, openID string, limit int) ([]CommandTranslation, error)
	RecentRuns(ctx context.Context, limit int) ([]RecentRun, error)
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

	t.Run("HasCapableDeviceIgnoresStatus", func(t *testing.T) {
		s := newStore(t)
		// 空 fleet:任何 selector 都无匹配
		if ok, err := s.HasCapableDevice(ctx, wf.DeviceSelector{}); err != nil || ok {
			t.Fatalf("empty fleet: ok=%v err=%v, want false", ok, err)
		}
		seed(t, s)
		// 大小写不敏感 + capabilities 子集,与 AcquireDevice 同一匹配语义
		if ok, _ := s.HasCapableDevice(ctx, wf.DeviceSelector{SOC: []string{"TRINKET"}, Capabilities: []string{"hexagon"}}); !ok {
			t.Error("trinket+hexagon 应匹配")
		}
		if ok, _ := s.HasCapableDevice(ctx, wf.DeviceSelector{SOC: []string{"RK3588"}}); ok {
			t.Error("RK3588 不应匹配")
		}
		if ok, _ := s.HasCapableDevice(ctx, wf.DeviceSelector{Capabilities: []string{"npu"}}); ok {
			t.Error("npu 不应匹配(capabilities 非子集)")
		}
		// BUSY/QUARANTINED 也算 fleet 有能力("设备在但暂不可用"由 acquire 等待处理)
		l, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t1", 120)
		if err != nil || l == nil {
			t.Fatal(err)
		}
		if ok, _ := s.HasCapableDevice(ctx, wf.DeviceSelector{SOC: []string{"trinket"}}); !ok {
			t.Error("BUSY 设备仍应报告 fleet 有能力")
		}
	})

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
		if err := s.ReleaseDevice(ctx, l.DeviceID, "t1", wf.FailScopeOK, 3); err != nil {
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

	// clients.fail_streak 的姊妹陷阱(差距 #10):心跳只应刷新设备属性,
	// 不得顺带清空 client 级计数器——否则 client 侧的连续失败永远数不到 2。
	t.Run("HeartbeatMustNotResetClientFailStreak", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		l, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t1", 120)
		if err != nil || l == nil {
			t.Fatalf("lease=%v err=%v", l, err)
		}
		if err := s.ReleaseDevice(ctx, l.DeviceID, "t1", wf.FailScopeClient, 3); err != nil {
			t.Fatal(err)
		}
		seed(t, s) // 心跳重注册:只刷新属性,不得把 clients.fail_streak 清零
		ov, err := s.FleetOverview(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ov.Devices[0].ClientFailStreak != 1 {
			t.Errorf("心跳后 client fail_streak = %d, want 1(心跳不得清空该计数器)", ov.Devices[0].ClientFailStreak)
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
		if err := s.ReleaseDevice(ctx, l.DeviceID, "other-task", wf.FailScopeDevice, 3); err != nil {
			t.Fatal(err)
		}
		if l2, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t2", 120); l2 != nil {
			t.Fatalf("非持有者释放不得生效: %+v", l2)
		}
		// 持有者释放 + 重复释放幂等
		if err := s.ReleaseDevice(ctx, l.DeviceID, "t1", wf.FailScopeOK, 3); err != nil {
			t.Fatal(err)
		}
		if err := s.ReleaseDevice(ctx, l.DeviceID, "t1", wf.FailScopeDevice, 3); err != nil {
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
			_ = s.ReleaseDevice(ctx, l.DeviceID, "t", wf.FailScopeDevice, 3)
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
			_ = s.ReleaseDevice(ctx, l.DeviceID, "t", wf.FailScopeDevice, 3)
		}
		l, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t", 120)
		_ = s.ReleaseDevice(ctx, l.DeviceID, "t", wf.FailScopeOK, 3) // 成功:清零
		for i := 0; i < 2; i++ {
			l, _ = s.AcquireDevice(ctx, wf.DeviceSelector{}, "t", 120)
			if l == nil {
				t.Fatal("fail_streak 清零后 2 次 INFRA 不应隔离")
			}
			_ = s.ReleaseDevice(ctx, l.DeviceID, "t", wf.FailScopeDevice, 3)
		}
	})

	// 归因记账(差距 #10):四个 scope 各记各的账,互不串味。
	//
	// none 与 ok 的关键区别只有在计数器非零时才可观察(0 不动 vs 0 清零看起来
	// 一样),所以每个子用例先用 device/client scope 把两个计数器都垫到 1,
	// 再对种子后的状态跑被测 scope。quarantineAfter 取 5:种子的 1 次 device
	// 释放 + device 用例自身再 1 次 = 2,远低于阈值,不会把设备提前隔离而
	// 搅乱断言。
	t.Run("ReleaseDeviceFailScopes", func(t *testing.T) {
		const quarantineAfter = 5
		cases := []struct {
			name           string
			scope          wf.FailScope
			wantDeviceFail int
			wantClientFail int
			wantStatus     string
		}{
			{"device 只增设备计数", wf.FailScopeDevice, 2, 1, "IDLE"},
			{"client 只增 client 计数", wf.FailScopeClient, 1, 2, "IDLE"},
			{"none 两个都不动", wf.FailScopeNone, 1, 1, "IDLE"},
			{"ok 两个都清零", wf.FailScopeOK, 0, 0, "IDLE"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s := newStore(t)
				seed(t, s)

				// 种子:设备计数、client 计数各垫到 1。
				seedDev, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:seed-device:a1", 120)
				if err != nil || seedDev == nil {
					t.Fatalf("seed device acquire = %+v err=%v", seedDev, err)
				}
				if err := s.ReleaseDevice(ctx, seedDev.DeviceID, "w:seed-device:a1", wf.FailScopeDevice, quarantineAfter); err != nil {
					t.Fatalf("seed device release: %v", err)
				}
				seedClient, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:seed-client:a1", 120)
				if err != nil || seedClient == nil {
					t.Fatalf("seed client acquire = %+v err=%v", seedClient, err)
				}
				if err := s.ReleaseDevice(ctx, seedClient.DeviceID, "w:seed-client:a1", wf.FailScopeClient, quarantineAfter); err != nil {
					t.Fatalf("seed client release: %v", err)
				}

				lease, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", 120)
				if err != nil || lease == nil {
					t.Fatalf("acquire = %+v err=%v", lease, err)
				}
				if err := s.ReleaseDevice(ctx, lease.DeviceID, "w:t1:a1", tc.scope, quarantineAfter); err != nil {
					t.Fatalf("release: %v", err)
				}
				ov, err := s.FleetOverview(ctx)
				if err != nil {
					t.Fatal(err)
				}
				d := ov.Devices[0]
				if d.FailStreak != tc.wantDeviceFail {
					t.Errorf("device fail_streak = %d, want %d", d.FailStreak, tc.wantDeviceFail)
				}
				if d.Status != tc.wantStatus {
					t.Errorf("status = %q, want %q", d.Status, tc.wantStatus)
				}
				if d.ClientFailStreak != tc.wantClientFail {
					t.Errorf("client fail_streak = %d, want %d", d.ClientFailStreak, tc.wantClientFail)
				}
			})
		}
	})

	// 只有 device scope 触发隔离;client/none 累积再多也不隔离设备
	// ——这正是差距 #10 要消灭的误伤。
	t.Run("ReleaseDeviceOnlyDeviceScopeQuarantines", func(t *testing.T) {
		for _, scope := range []wf.FailScope{wf.FailScopeClient, wf.FailScopeNone} {
			t.Run(string(scope), func(t *testing.T) {
				s := newStore(t)
				seed(t, s)
				for i := 1; i <= 5; i++ {
					taskID := fmt.Sprintf("w:t%d:a1", i)
					lease, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, taskID, 120)
					if err != nil || lease == nil {
						t.Fatalf("acquire %d = %+v err=%v", i, lease, err)
					}
					if err := s.ReleaseDevice(ctx, lease.DeviceID, taskID, scope, 3); err != nil {
						t.Fatal(err)
					}
				}
				ov, err := s.FleetOverview(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if ov.Devices[0].Status == "QUARANTINED" {
					t.Errorf("%s 连续 5 次仍不得隔离设备(差距 #10 的误伤)", scope)
				}
			})
		}
	})

	// device scope 达阈值才隔离,且 ok 能把计数清回去。
	t.Run("ReleaseDeviceQuarantineAndReset", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		for i := 1; i <= 2; i++ {
			taskID := fmt.Sprintf("w:t%d:a1", i)
			lease, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, taskID, 120)
			if err != nil || lease == nil {
				t.Fatalf("acquire %d: %+v %v", i, lease, err)
			}
			if err := s.ReleaseDevice(ctx, lease.DeviceID, taskID, wf.FailScopeDevice, 3); err != nil {
				t.Fatal(err)
			}
		}
		// 第 3 次成功 → 清零,不该隔离
		lease, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t3:a1", 120)
		if err != nil || lease == nil {
			t.Fatalf("acquire3: %+v %v", lease, err)
		}
		if err := s.ReleaseDevice(ctx, lease.DeviceID, "w:t3:a1", wf.FailScopeOK, 3); err != nil {
			t.Fatal(err)
		}
		ov, err := s.FleetOverview(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ov.Devices[0].FailStreak != 0 || ov.Devices[0].Status == "QUARANTINED" {
			t.Errorf("ok 应清零且不隔离, got %+v", ov.Devices[0])
		}
	})

	// 幂等:重复释放/非持有者释放不得重复计数(既有语义,加 scope 后必须保持)。
	t.Run("ReleaseDeviceScopeIdempotent", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		lease, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", 120)
		if err != nil || lease == nil {
			t.Fatalf("acquire: %+v %v", lease, err)
		}
		for i := 0; i < 3; i++ {
			if err := s.ReleaseDevice(ctx, lease.DeviceID, "w:t1:a1", wf.FailScopeClient, 3); err != nil {
				t.Fatal(err)
			}
		}
		// 非持有者释放
		if err := s.ReleaseDevice(ctx, lease.DeviceID, "w:other:a1", wf.FailScopeClient, 3); err != nil {
			t.Fatal(err)
		}
		ov, err := s.FleetOverview(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ov.Devices[0].ClientFailStreak != 1 {
			t.Errorf("client fail_streak = %d, want 1(只第一次生效)", ov.Devices[0].ClientFailStreak)
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

	// 租约生命周期状态机(§10 租约 120s 心跳续期):BUSY 设备在租约过期后被
	// AcquireDevice 懒回收——这是 workflow 被 Terminate/进程死亡等绕过
	// ReleaseDevice 场景的唯一恢复路径,必须有表驱动覆盖。
	t.Run("LeaseLifecycleRenewAndReclaim", func(t *testing.T) {
		cases := []struct {
			name          string
			leaseSeconds  int  // t1 初始租约时长;0 = 立即过期(模拟持有者失联)
			renew         bool // 租约过期后持有者是否凭所有权凭据续租
			renewSeconds  int
			wantReclaimed bool // t2 随后能否取得设备
		}{
			{"有效租约不得回收", 120, false, 0, false},
			{"过期租约懒回收", 0, false, 0, true},
			{"持有者续期阻止回收", 0, true, 120, false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s := newStore(t)
				seed(t, s)
				l, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", tc.leaseSeconds)
				if err != nil || l == nil {
					t.Fatalf("t1 acquire: lease=%v err=%v", l, err)
				}
				if tc.renew {
					ok, err := s.RenewLease(ctx, LeaseCredential{
						DeviceID: l.DeviceID, ClientID: l.ClientID, TaskID: "w:t1:a1",
						Attempt: 1, LeaseID: l.LeaseID, Generation: l.Generation,
					}, tc.renewSeconds)
					if err != nil || !ok {
						t.Fatalf("持有者续租应成功: ok=%v err=%v", ok, err)
					}
				}
				l2, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t2:a1", 120)
				if err != nil {
					t.Fatalf("t2 acquire: %v", err)
				}
				if (l2 != nil) != tc.wantReclaimed {
					t.Errorf("reclaimed = %v, want %v", l2 != nil, tc.wantReclaimed)
				}
			})
		}
	})

	// 租约所有权凭据(§10/差距 #15):续租是条件更新,任何一项失配都返回
	// false(LEASE_NOT_OWNED),旧持有者不得再续已易主/已释放的租约。
	t.Run("LeaseOwnershipCredentials", func(t *testing.T) {
		cases := []struct {
			name         string
			mutate       func(c *LeaseCredential)
			releaseFirst bool
			want         bool
		}{
			{"正确凭据续租成功", func(*LeaseCredential) {}, false, true},
			{"错client不得续租", func(c *LeaseCredential) { c.ClientID = "other" }, false, false},
			{"错task不得续租", func(c *LeaseCredential) { c.TaskID = "w:t9:a1" }, false, false},
			{"错lease_id不得续租", func(c *LeaseCredential) { c.LeaseID = "forged" }, false, false},
			{"错generation不得续租", func(c *LeaseCredential) { c.Generation++ }, false, false},
			{"attempt与task_id不一致不得续租", func(c *LeaseCredential) { c.Attempt = 2 }, false, false},
			{"已释放租约不得续租", func(*LeaseCredential) {}, true, false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s := newStore(t)
				seed(t, s)
				l, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", 120)
				if err != nil || l == nil {
					t.Fatalf("acquire: lease=%v err=%v", l, err)
				}
				cred := LeaseCredential{
					DeviceID: l.DeviceID, ClientID: l.ClientID, TaskID: "w:t1:a1",
					Attempt: 1, LeaseID: l.LeaseID, Generation: l.Generation,
				}
				tc.mutate(&cred)
				if tc.releaseFirst {
					if err := s.ReleaseDevice(ctx, l.DeviceID, "w:t1:a1", wf.FailScopeOK, 3); err != nil {
						t.Fatal(err)
					}
				}
				ok, err := s.RenewLease(ctx, cred, 120)
				if err != nil {
					t.Fatalf("renew: %v", err)
				}
				if ok != tc.want {
					t.Errorf("renewed = %v, want %v", ok, tc.want)
				}
			})
		}
		// 懒回收易主:generation 递增,旧持有者凭据全部失效,新持有者可续
		s := newStore(t)
		seed(t, s)
		old, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", 0) // 立即过期
		if err != nil || old == nil {
			t.Fatalf("acquire: %v %v", old, err)
		}
		newL, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t2:a1", 120)
		if err != nil || newL == nil {
			t.Fatalf("reclaim: %v %v", newL, err)
		}
		if newL.Generation != old.Generation+1 {
			t.Errorf("generation = %d → %d, want +1", old.Generation, newL.Generation)
		}
		if ok, _ := s.RenewLease(ctx, LeaseCredential{
			DeviceID: old.DeviceID, ClientID: old.ClientID, TaskID: "w:t1:a1",
			Attempt: 1, LeaseID: old.LeaseID, Generation: old.Generation,
		}, 120); ok {
			t.Error("旧持有者凭据在易主后不得续租")
		}
		if ok, _ := s.RenewLease(ctx, LeaseCredential{
			DeviceID: newL.DeviceID, ClientID: newL.ClientID, TaskID: "w:t2:a1",
			Attempt: 1, LeaseID: newL.LeaseID, Generation: newL.Generation,
		}, 120); !ok {
			t.Error("新持有者凭据应可续租")
		}
	})

	// GetLeaseExpiry:CheckLease 活动的数据源(原则 6)——持有中返回到期时刻,
	// 释放后/未知任务返回 nil(未续期)。
	t.Run("GetLeaseExpiryLifecycle", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		if exp, err := s.GetLeaseExpiry(ctx, "w:t1:a1"); err != nil || exp != nil {
			t.Errorf("未知任务: exp=%v err=%v, want (nil, nil)", exp, err)
		}
		l, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", 120)
		if err != nil || l == nil {
			t.Fatalf("acquire: %v %v", l, err)
		}
		exp, err := s.GetLeaseExpiry(ctx, "w:t1:a1")
		if err != nil || exp == nil {
			t.Fatalf("持有中: exp=%v err=%v", exp, err)
		}
		if time.Until(*exp) < 100*time.Second {
			t.Errorf("expiry = %v, want ~120s 后", exp)
		}
		if err := s.ReleaseDevice(ctx, l.DeviceID, "w:t1:a1", wf.FailScopeOK, 3); err != nil {
			t.Fatal(err)
		}
		if exp, err := s.GetLeaseExpiry(ctx, "w:t1:a1"); err != nil || exp != nil {
			t.Errorf("释放后: exp=%v err=%v, want (nil, nil)", exp, err)
		}
	})

	// lease_id 必须含不可猜的秘密材料(差距 #8 final-review)。它是 upload-requests
	// 端点唯一的鉴权依据,而该端点签发往证据桶写入的 URL、callbacks 又无其他鉴权。
	// 若 lease_id 等于 task_id(旧实现),凭据的全部成分都可猜:task_id 有规律、
	// client_id 可猜、device_id 就是 serial、attempt 编码在 task_id 里、
	// lease_generation 是每设备小计数——同网段主机试几次就能换到写入 URL。
	t.Run("LeaseIDCarriesEntropy", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		const taskID = "w:t1:a1"
		lease, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, taskID, 120)
		if err != nil || lease == nil {
			t.Fatalf("acquire: %+v %v", lease, err)
		}
		if lease.LeaseID == taskID {
			t.Fatal("lease_id 等于 task_id:凭据没有任何秘密材料,端点鉴权形同虚设")
		}
		// 前缀保留 task_id 便于排查;后缀是随机十六进制。
		suffix, ok := strings.CutPrefix(lease.LeaseID, taskID+":")
		if !ok {
			t.Fatalf("lease_id = %q, want %q 前缀", lease.LeaseID, taskID+":")
		}
		if len(suffix) != 32 { // 16 字节 hex
			t.Errorf("随机后缀长度 = %d, want 32(16 字节 hex)", len(suffix))
		}
		if _, err := hex.DecodeString(suffix); err != nil {
			t.Errorf("随机后缀不是十六进制: %q", suffix)
		}
		// 同一 task 重新获取(懒回收/重试)必须换新值,否则旧凭据仍然有效。
		if err := s.ReleaseDevice(ctx, lease.DeviceID, taskID, wf.FailScopeOK, 3); err != nil {
			t.Fatal(err)
		}
		again, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, taskID, 120)
		if err != nil || again == nil {
			t.Fatalf("re-acquire: %+v %v", again, err)
		}
		if again.LeaseID == lease.LeaseID {
			t.Error("同一 task 重新获取应生成新 lease_id,否则旧凭据不失效")
		}
	})

	// 只读租约校验(差距 #8 的签发端点鉴权依据):校验通过不得有任何副作用,
	// 尤其不得像 RenewLease 那样续期——签一次 URL 不等于任务还活着。
	t.Run("VerifyLeaseIsReadOnly", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		lease, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", 120)
		if err != nil || lease == nil {
			t.Fatalf("acquire: %+v %v", lease, err)
		}
		cred := LeaseCredential{
			DeviceID: lease.DeviceID, ClientID: lease.ClientID, TaskID: "w:t1:a1",
			Attempt: 1, LeaseID: lease.LeaseID, Generation: lease.Generation,
		}
		before, err := s.GetLeaseExpiry(ctx, "w:t1:a1")
		if err != nil || before == nil {
			t.Fatalf("expiry before: %v %v", before, err)
		}
		ok, err := s.VerifyLease(ctx, cred)
		if err != nil || !ok {
			t.Fatalf("VerifyLease = %v, %v; want true, nil", ok, err)
		}
		after, err := s.GetLeaseExpiry(ctx, "w:t1:a1")
		if err != nil || after == nil {
			t.Fatalf("expiry after: %v %v", after, err)
		}
		if !after.Equal(*before) {
			t.Errorf("校验不得续期: %v → %v", before, after)
		}
	})

	// 凭据任一项失配都必须判否——这是端点唯一的鉴权依据。
	t.Run("VerifyLeaseRejectsMismatch", func(t *testing.T) {
		base := func(l *wf.Lease) LeaseCredential {
			return LeaseCredential{
				DeviceID: l.DeviceID, ClientID: l.ClientID, TaskID: "w:t1:a1",
				Attempt: 1, LeaseID: l.LeaseID, Generation: l.Generation,
			}
		}
		cases := []struct {
			name   string
			mutate func(c *LeaseCredential)
		}{
			{"错 lease_id", func(c *LeaseCredential) { c.LeaseID = "bogus" }},
			{"错 generation", func(c *LeaseCredential) { c.Generation += 1 }},
			{"错 client_id", func(c *LeaseCredential) { c.ClientID = "other" }},
			{"错 task_id", func(c *LeaseCredential) { c.TaskID = "w:other:a1" }},
			{"错 device_id", func(c *LeaseCredential) { c.DeviceID = "no-such-device" }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s := newStore(t)
				seed(t, s)
				lease, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", 120)
				if err != nil || lease == nil {
					t.Fatalf("acquire: %+v %v", lease, err)
				}
				cred := base(lease)
				tc.mutate(&cred)
				ok, err := s.VerifyLease(ctx, cred)
				if err != nil {
					t.Fatalf("VerifyLease err = %v", err)
				}
				if ok {
					t.Errorf("%s 应判否", tc.name)
				}
			})
		}
	})

	// 已释放的租约不再是持有者(任务结束后不得继续换 URL)。
	t.Run("VerifyLeaseRejectsReleased", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		lease, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", 120)
		if err != nil || lease == nil {
			t.Fatalf("acquire: %+v %v", lease, err)
		}
		cred := LeaseCredential{
			DeviceID: lease.DeviceID, ClientID: lease.ClientID, TaskID: "w:t1:a1",
			Attempt: 1, LeaseID: lease.LeaseID, Generation: lease.Generation,
		}
		if err := s.ReleaseDevice(ctx, lease.DeviceID, "w:t1:a1", wf.FailScopeOK, 3); err != nil {
			t.Fatal(err)
		}
		ok, err := s.VerifyLease(ctx, cred)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Error("已释放的租约不得通过校验")
		}
	})

	// 从未被 AcquireDevice 过的设备(仅心跳注册)不得通过零值凭据校验——
	// MemStore 的 UpsertClientDevices 为新设备写入零值 deviceRow(LeaseTaskID/
	// LeaseID/Generation 均为 Go 零值),若 VerifyLease 不要求 status=BUSY,
	// 零值凭据会与零值行"巧合匹配"而被判真,陌生人即可对任意从未跑过任务的
	// 设备换到写入 URL。
	t.Run("VerifyLeaseRejectsNeverLeasedDevice", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		ok, err := s.VerifyLease(ctx, LeaseCredential{
			DeviceID: "513cd3de", ClientID: "c1", TaskID: "", LeaseID: "", Generation: 0,
		})
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Error("从未 AcquireDevice 过的设备不得通过零值凭据校验")
		}
	})

	// attempt 是端点唯一的鉴权依据之一:即便 device/client/task/lease_id/
	// generation 全部匹配一个真实活跃的租约,attempt 对不上也必须判否。
	t.Run("VerifyLeaseRejectsAttemptMismatch", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		lease, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", 120)
		if err != nil || lease == nil {
			t.Fatalf("acquire: %+v %v", lease, err)
		}
		ok, err := s.VerifyLease(ctx, LeaseCredential{
			DeviceID: lease.DeviceID, ClientID: lease.ClientID, TaskID: "w:t1:a1",
			Attempt: 99, LeaseID: lease.LeaseID, Generation: lease.Generation,
		})
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Error("attempt 与 task_id 后缀不一致应判否")
		}
	})

	// NextWorkflowAttempt(差距 #11):显式 retry 计数按逻辑键原子单调递增;
	// 未登记的键报错。
	t.Run("NextWorkflowAttemptMonotonic", func(t *testing.T) {
		s := newStore(t)
		art := Artifact{Project: "grp/p", CommitSHA: "abcd1234", PipelineID: 42,
			Variant: "v1", BuildType: "Release", URL: "u", SHA256: "s", Size: 1, ManifestDigest: "m"}
		if err := s.RegisterArtifacts(ctx, []Artifact{art}); err != nil {
			t.Fatal(err)
		}
		for want := 1; want <= 3; want++ {
			n, err := s.NextWorkflowAttempt(ctx, "abcd1234", 42, "v1")
			if err != nil || n != want {
				t.Fatalf("attempt = %d err=%v, want %d", n, err, want)
			}
		}
		if _, err := s.NextWorkflowAttempt(ctx, "abcd1234", 42, "ghost"); err == nil {
			t.Error("未登记的键应报错")
		}
	})

	// 懒回收后租约易主:旧持有者的 ReleaseDevice 必须幂等空转,
	// 不得把新持有者的设备释放掉(§3 规则 7 幂等)。
	t.Run("ReclaimTransfersOwnership", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		l, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "dead-task", 0) // 立即过期
		if err != nil || l == nil {
			t.Fatalf("acquire: lease=%v err=%v", l, err)
		}
		l2, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t2", 120) // 回收
		if err != nil || l2 == nil {
			t.Fatalf("reclaim: lease=%v err=%v", l2, err)
		}
		if err := s.ReleaseDevice(ctx, l.DeviceID, "dead-task", wf.FailScopeDevice, 3); err != nil {
			t.Fatal(err)
		}
		if l3, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "t3", 120); l3 != nil {
			t.Errorf("旧持有者释放不得影响新租约: %+v", l3)
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

	// 事务性 Outbox(原则 3):results + outbox 单事务写入,两侧各自幂等。
	t.Run("SaveResultWithOutboxIdempotent", func(t *testing.T) {
		s := newStore(t)
		_ = s.CreateTask(ctx, wf.TaskRow{TaskID: "w:t1:a1", IdempotencyKey: "w:t1:a1"})
		rec := wf.ResultRecord{TaskID: "w:t1:a1", Result: wf.TaskResultSignal{
			TaskID: "w:t1:a1", Status: "COMPLETED", ExitCode: 0, CasesTotal: 38,
		}}
		payload := json.RawMessage(`{"workflow_id":"w","result":{"task_id":"w:t1:a1"}}`)
		ev := OutboxEvent{AggregateType: "task", AggregateID: "w:t1:a1",
			EventType: EventTypeTaskResult, EventKey: "w:t1:a1:result", Payload: payload}
		ins, err := s.SaveResultWithOutbox(ctx, rec, ev)
		if err != nil || !ins {
			t.Fatalf("first save: ins=%v err=%v", ins, err)
		}
		// 回调重发:同 task_id 结果去重、同 event_key 不产生第二行、不报错
		ins, err = s.SaveResultWithOutbox(ctx, rec, ev)
		if err != nil || ins {
			t.Fatalf("重复写入应去重: ins=%v err=%v", ins, err)
		}
		rows, err := s.ClaimUnpublished(ctx, 10)
		if err != nil || len(rows) != 1 {
			t.Fatalf("claim = %+v err=%v, want 单行", rows, err)
		}
		got := rows[0]
		if got.AggregateType != "task" || got.AggregateID != "w:t1:a1" ||
			got.EventType != EventTypeTaskResult || got.EventKey != "w:t1:a1:result" ||
			got.Attempts != 0 || got.ID == 0 {
			t.Errorf("outbox row = %+v", got)
		}
		// GetResult 权威读(LoadResult 活动,差距 #2)
		loaded, err := s.GetResult(ctx, "w:t1:a1")
		if err != nil || loaded == nil {
			t.Fatalf("get result = %+v err=%v", loaded, err)
		}
		if loaded.Result.Status != "COMPLETED" || loaded.Result.CasesTotal != 38 {
			t.Errorf("loaded result = %+v", loaded.Result)
		}
		if missing, err := s.GetResult(ctx, "no-such"); err != nil || missing != nil {
			t.Errorf("未知任务应返回 (nil, nil): %+v %v", missing, err)
		}
	})

	// outbox 投递生命周期状态机:unpublished →(MarkFailed 累计 attempts,行保持
	// 未投递)→ MarkPublished 终态;两个 Mark* 都只作用于未投递行(表驱动)。
	t.Run("OutboxLifecycle", func(t *testing.T) {
		type op struct {
			fail    string // 非空 → MarkFailed(id, 该错误)
			publish bool   // → MarkPublished(id)
		}
		cases := []struct {
			name          string
			ops           []op
			wantPending   bool // 最终仍待投递
			wantAttempts  int
			wantLastError string
		}{
			{"直接投递成功", []op{{publish: true}}, false, 0, ""},
			{"失败后重投成功", []op{{fail: "boom"}, {publish: true}}, false, 1, "boom"},
			{"连续失败累积attempts", []op{{fail: "e1"}, {fail: "e2"}}, true, 2, "e2"},
			{"重复MarkPublished幂等", []op{{publish: true}, {publish: true}}, false, 0, ""},
			{"已投递后MarkFailed无副作用", []op{{publish: true}, {fail: "late"}}, false, 0, ""},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s := newStore(t)
				_ = s.CreateTask(ctx, wf.TaskRow{TaskID: "w:t1:a1", IdempotencyKey: "w:t1:a1"})
				_, err := s.SaveResultWithOutbox(ctx,
					wf.ResultRecord{TaskID: "w:t1:a1", Result: wf.TaskResultSignal{TaskID: "w:t1:a1"}},
					OutboxEvent{AggregateType: "task", AggregateID: "w:t1:a1",
						EventType: EventTypeTaskResult, EventKey: "w:t1:a1:result",
						Payload: json.RawMessage(`{}`)})
				if err != nil {
					t.Fatal(err)
				}
				rows, err := s.ClaimUnpublished(ctx, 10)
				if err != nil || len(rows) != 1 {
					t.Fatalf("claim = %+v err=%v", rows, err)
				}
				id := rows[0].ID
				for _, o := range tc.ops {
					if o.fail != "" {
						if err := s.MarkFailed(ctx, id, o.fail); err != nil {
							t.Fatalf("mark failed: %v", err)
						}
					}
					if o.publish {
						if err := s.MarkPublished(ctx, id); err != nil {
							t.Fatalf("mark published: %v", err)
						}
					}
				}
				rows, err = s.ClaimUnpublished(ctx, 10)
				if err != nil {
					t.Fatal(err)
				}
				if (len(rows) == 1) != tc.wantPending {
					t.Fatalf("pending rows = %d, wantPending=%v", len(rows), tc.wantPending)
				}
				if tc.wantPending {
					if rows[0].Attempts != tc.wantAttempts || rows[0].LastError != tc.wantLastError {
						t.Errorf("row = %+v, want attempts=%d last_error=%q",
							rows[0], tc.wantAttempts, tc.wantLastError)
					}
				}
			})
		}
	})

	// 积压监控(第四批):pending/stuck 计数、最老行定位、诊断用 last_error 采样。
	// 两套实现必须给出同样的结论,否则内存模式下的告警演练不能代表生产。
	t.Run("OutboxBacklog", func(t *testing.T) {
		s := newStore(t)
		seed := func(taskID string) int64 {
			t.Helper()
			if err := s.CreateTask(ctx, wf.TaskRow{TaskID: taskID, IdempotencyKey: taskID}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SaveResultWithOutbox(ctx,
				wf.ResultRecord{TaskID: taskID, Result: wf.TaskResultSignal{TaskID: taskID}},
				OutboxEvent{AggregateType: "task", AggregateID: taskID,
					EventType: EventTypeTaskResult, EventKey: taskID + ":result",
					Payload: json.RawMessage(`{}`)}); err != nil {
				t.Fatal(err)
			}
			rows, err := s.ClaimUnpublished(ctx, 100)
			if err != nil {
				t.Fatal(err)
			}
			return rows[len(rows)-1].ID // 最新插入的一行
		}

		// 空 outbox:全零,且不得把"没有行"报成年龄非零。
		b, err := s.OutboxBacklog(ctx, 3)
		if err != nil {
			t.Fatal(err)
		}
		if b.Pending != 0 || b.Stuck != 0 || b.OldestAge != 0 || b.OldestID != 0 {
			t.Fatalf("空 outbox backlog = %+v, want 全零", b)
		}

		oldest := seed("w:t1:a1")
		newest := seed("w:t2:a1")

		// 两行待投,都没失败过 → 不算卡住;最老行是先插入的那条。
		b, err = s.OutboxBacklog(ctx, 3)
		if err != nil {
			t.Fatal(err)
		}
		if b.Pending != 2 || b.Stuck != 0 {
			t.Errorf("backlog = %+v, want pending=2 stuck=0", b)
		}
		if b.OldestID != oldest {
			t.Errorf("OldestID = %d, want %d(先入库的那行)", b.OldestID, oldest)
		}

		// 让新的那行失败 3 次 → 达到阈值算卡住;采样的 last_error 取尝试最多的行。
		for i := 0; i < 3; i++ {
			if err := s.MarkFailed(ctx, newest, "boom"); err != nil {
				t.Fatal(err)
			}
		}
		b, err = s.OutboxBacklog(ctx, 3)
		if err != nil {
			t.Fatal(err)
		}
		if b.Pending != 2 || b.Stuck != 1 {
			t.Errorf("backlog = %+v, want pending=2 stuck=1", b)
		}
		if b.SampleError != "boom" {
			t.Errorf("SampleError = %q, want boom(尝试次数最多那行的 last_error)", b.SampleError)
		}
		// 阈值可调:抬到 4 就不该再算卡住。
		b, err = s.OutboxBacklog(ctx, 4)
		if err != nil {
			t.Fatal(err)
		}
		if b.Stuck != 0 {
			t.Errorf("阈值 4 时 stuck = %d, want 0", b.Stuck)
		}

		// 投递掉最老一行 → pending 递减,最老行前移。
		if err := s.MarkPublished(ctx, oldest); err != nil {
			t.Fatal(err)
		}
		b, err = s.OutboxBacklog(ctx, 3)
		if err != nil {
			t.Fatal(err)
		}
		if b.Pending != 1 || b.OldestID != newest {
			t.Errorf("backlog = %+v, want pending=1 oldest=%d", b, newest)
		}
	})

	// 结论性判定边界(bundle webhook 跳过已测变体):
	// status='COMPLETED' 且 verdict ∈ {PASSED, TEST_FAILED} 才算结论;
	// INFRA_ERROR/TIMEOUT(测试未必真实执行)、非终态、无记录均需重测。
	t.Run("ConclusiveWorkflowIDsBoundary", func(t *testing.T) {
		cases := []struct {
			name       string
			status     string // FinishTask 落库的 status
			verdict    string
			conclusive bool
		}{
			{"PASSED 结论", "COMPLETED", "PASSED", true},
			{"TEST_FAILED 结论(测试真实跑完)", "COMPLETED", "TEST_FAILED", true},
			{"INFRA_ERROR 非结论", "FAILED", "INFRA_ERROR", false},
			{"TIMEOUT 非结论", "TIMEOUT", "INFRA_ERROR", false},
			{"status 非 COMPLETED 即使 verdict 通过也不算", "FAILED", "PASSED", false},
			{"COMPLETED 但 verdict 未判定", "COMPLETED", "", false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s := newStore(t)
				const wfID = "device-test-grp/p-gabcd1234-p42-v1"
				if err := s.CreateTask(ctx, wf.TaskRow{TaskID: wfID + ":t1:a1",
					WorkflowID: wfID, IdempotencyKey: wfID + ":t1:a1"}); err != nil {
					t.Fatal(err)
				}
				if err := s.FinishTask(ctx, wf.FinishRequest{TaskID: wfID + ":t1:a1",
					Status: tc.status, Verdict: tc.verdict}); err != nil {
					t.Fatal(err)
				}
				got, err := s.ConclusiveWorkflowIDs(ctx, []string{wfID, "device-test-grp/p-gabcd1234-p42-v2"})
				if err != nil {
					t.Fatal(err)
				}
				if got[wfID] != tc.conclusive {
					t.Errorf("conclusive = %v, want %v", got[wfID], tc.conclusive)
				}
				if got["device-test-grp/p-gabcd1234-p42-v2"] {
					t.Error("无记录的 workflow 不得判结论")
				}
			})
		}
	})

	// evidence_snapshots(差距 #6,决策可回放):幂等登记 + 读回;未知 id (nil,nil)。
	t.Run("EvidenceSnapshotIdempotentRoundTrip", func(t *testing.T) {
		s := newStore(t)
		snap := EvidenceSnapshot{
			EvidenceID: "w:t1:a1", TaskID: "w:t1:a1", Attempt: 1,
			ObjectKey: "evidence/w:t1:a1/evidence.json",
			SHA256:    "deadbeef", ExtractorVersion: "1",
		}
		if err := s.SaveEvidenceSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
		// 重复登记(activity 重试/重复提取):无副作用,保留首次内容
		dup := snap
		dup.SHA256 = "changed"
		if err := s.SaveEvidenceSnapshot(ctx, dup); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetEvidenceSnapshot(ctx, "w:t1:a1")
		if err != nil || got == nil {
			t.Fatalf("get = %+v err=%v", got, err)
		}
		if *got != snap {
			t.Errorf("snapshot = %+v, want %+v(首次内容,幂等)", *got, snap)
		}
		if missing, err := s.GetEvidenceSnapshot(ctx, "no-such"); err != nil || missing != nil {
			t.Errorf("未知 id 应返回 (nil, nil): %+v %v", missing, err)
		}
	})

	// 飞书指令查询面:FleetOverview 汇总 + UnquarantineDevice 解隔离。
	t.Run("FleetOverviewAndUnquarantine", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		// 一台 BUSY 带活跃租约 + 一台任务非终态的 workflow
		l, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", 120)
		if err != nil || l == nil {
			t.Fatalf("acquire: %v %v", l, err)
		}
		_ = s.CreateTask(ctx, wf.TaskRow{TaskID: "w:t1:a1", WorkflowID: "w",
			IdempotencyKey: "w:t1:a1", Status: "RUNNING"})
		ov, err := s.FleetOverview(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ov.InflightWorkflows != 1 || ov.ActiveLeases != 1 || len(ov.Devices) != 1 {
			t.Fatalf("overview = %+v", ov)
		}
		d := ov.Devices[0]
		if d.DeviceID != "513cd3de" || d.Status != "BUSY" || d.LeaseTaskID != "w:t1:a1" ||
			d.SOC != "trinket" {
			t.Errorf("device = %+v", d)
		}
		// 任务终态后运行中数归零(租约释放后活跃租约归零)
		if err := s.FinishTask(ctx, wf.FinishRequest{TaskID: "w:t1:a1", Status: "COMPLETED", Verdict: "PASSED"}); err != nil {
			t.Fatal(err)
		}
		if err := s.ReleaseDevice(ctx, l.DeviceID, "w:t1:a1", wf.FailScopeOK, 3); err != nil {
			t.Fatal(err)
		}
		ov, _ = s.FleetOverview(ctx)
		if ov.InflightWorkflows != 0 || ov.ActiveLeases != 0 {
			t.Errorf("终态后 overview = %+v", ov)
		}
		// 隔离 → 解隔离循环(3 次 INFRA → QUARANTINED,§10)
		for i := 0; i < 3; i++ {
			l2, _ := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t2:a1", 120)
			if l2 == nil {
				t.Fatalf("第 %d 次 acquire", i+1)
			}
			_ = s.ReleaseDevice(ctx, l2.DeviceID, "w:t2:a1", wf.FailScopeDevice, 3)
		}
		ok, err := s.UnquarantineDevice(ctx, l.DeviceID)
		if err != nil || !ok {
			t.Fatalf("unquarantine: ok=%v err=%v", ok, err)
		}
		ov, _ = s.FleetOverview(ctx)
		if ov.Devices[0].Status != "IDLE" || ov.Devices[0].FailStreak != 0 {
			t.Errorf("解隔离后 device = %+v", ov.Devices[0])
		}
		if ok, _ := s.UnquarantineDevice(ctx, "ghost"); ok {
			t.Error("未知设备应返回 false")
		}
	})

	// 飞书指令 rerun 的数据面:ListArtifacts 按逻辑键取包,
	// NextWorkflowAttemptAll 全键递增取 max(变体行可能因 kick retry 发散)。
	t.Run("ListArtifactsAndAttemptAll", func(t *testing.T) {
		s := newStore(t)
		arts := []Artifact{
			{Project: "grp/p", CommitSHA: "abcd1234", PipelineID: 42, Variant: "v1",
				BuildType: "Release", URL: "u1", SHA256: "s1", Size: 1, ManifestDigest: "m1"},
			{Project: "grp/p", CommitSHA: "abcd1234", PipelineID: 42, Variant: "v2",
				BuildType: "Release", URL: "u2", SHA256: "s2", Size: 2, ManifestDigest: "m2"},
			{Project: "grp/p", CommitSHA: "abcd1234", PipelineID: 43, Variant: "v1",
				BuildType: "Release", URL: "u3", SHA256: "s3", Size: 3, ManifestDigest: "m3"},
		}
		if err := s.RegisterArtifacts(ctx, arts); err != nil {
			t.Fatal(err)
		}
		got, err := s.ListArtifacts(ctx, "abcd1234", 42)
		if err != nil || len(got) != 2 {
			t.Fatalf("list = %+v err=%v, want 2 行", got, err)
		}
		if got[0].Project != "grp/p" || got[0].URL == "" || got[0].ManifestDigest == "" {
			t.Errorf("artifact 字段不全: %+v", got[0])
		}
		if none, _ := s.ListArtifacts(ctx, "abcd1234", 99); len(none) != 0 {
			t.Errorf("无记录键应返回空: %+v", none)
		}
		// 变体级 retry 使 v1 行先发散到 1;bundle 级递增后应取 max=2
		if n, err := s.NextWorkflowAttempt(ctx, "abcd1234", 42, "v1"); err != nil || n != 1 {
			t.Fatalf("variant attempt = %d err=%v", n, err)
		}
		n, err := s.NextWorkflowAttemptAll(ctx, "abcd1234", 42)
		if err != nil || n != 2 {
			t.Fatalf("attempt all = %d err=%v, want 2(max 发散值+1)", n, err)
		}
		if _, err := s.NextWorkflowAttemptAll(ctx, "abcd1234", 99); err == nil {
			t.Error("无记录键应报错")
		}
	})

	t.Run("DecisionsRoundTripInOrder", func(t *testing.T) {
		s := newStore(t)
		_ = s.CreateTask(ctx, wf.TaskRow{TaskID: "w:t1:a1", IdempotencyKey: "w:t1:a1"})
		rule := wf.DecisionRow{TaskID: "w:t1:a1", Actor: "rule",
			Output: json.RawMessage(`{"verdict":"PASS","rule":"exit_code"}`)}
		llm := wf.DecisionRow{TaskID: "w:t1:a1", Actor: "hermes",
			InputDigest: "sha256:abc123", Model: "kimi-for-coding", PromptVersion: "analyzer-v3",
			Output:             json.RawMessage(`{"category":"PRODUCT","confidence":0.9}`),
			EvidenceSnapshotID: "w:t1:a1"}
		if err := s.SaveDecision(ctx, rule); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveDecision(ctx, llm); err != nil {
			t.Fatal(err)
		}
		got, err := s.ListDecisions(ctx, "w:t1:a1")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("decisions = %d, want 2", len(got))
		}
		if got[0].Actor != "rule" || got[1].Actor != "hermes" {
			t.Errorf("顺序应为 rule → hermes: %+v", got)
		}
		// JSONB 回读会做规范化(key 排序/空白),output 按语义比较而非字节比较
		assertJSONEqual := func(want json.RawMessage, got json.RawMessage) {
			t.Helper()
			var w, g any
			if err := json.Unmarshal(want, &w); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(got, &g); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(w, g) {
				t.Errorf("output = %s, want %s(语义)", got, want)
			}
		}
		assertJSONEqual(rule.Output, got[0].Output)
		if got[1].InputDigest != "sha256:abc123" || got[1].Model != "kimi-for-coding" ||
			got[1].PromptVersion != "analyzer-v3" || got[1].EvidenceSnapshotID != "w:t1:a1" {
			t.Errorf("hermes decision 字段不完整: %+v", got[1])
		}
		// rule 裁决不带快照引用(基于 result,不基于 evidence)
		if got[0].EvidenceSnapshotID != "" {
			t.Errorf("rule decision 不应带 evidence_snapshot_id: %+v", got[0])
		}
		assertJSONEqual(llm.Output, got[1].Output)
		if none, err := s.ListDecisions(ctx, "no-such"); err != nil || len(none) != 0 {
			t.Errorf("未知任务应返回空: %v %v", none, err)
		}
		// HasDecision(升级判重,设计 §5 门槛 3):按 task_id+actor 精确匹配
		if has, err := s.HasDecision(ctx, "w:t1:a1", "escalation"); err != nil || has {
			t.Errorf("未升级时应为 false: %v %v", has, err)
		}
		if err := s.SaveDecision(ctx, wf.DecisionRow{TaskID: "w:t1:a1", Actor: "escalation",
			Output: json.RawMessage(`{"kanban_task_id":"t_1","result":"created"}`)}); err != nil {
			t.Fatal(err)
		}
		if has, _ := s.HasDecision(ctx, "w:t1:a1", "escalation"); !has {
			t.Error("升级后应为 true")
		}
		if has, _ := s.HasDecision(ctx, "w:t1:a1", "human"); has {
			t.Error("不同 actor 不得误判")
		}
		if has, _ := s.HasDecision(ctx, "other-task", "escalation"); has {
			t.Error("不同 task 不得误判")
		}
	})

	// 飞书指令层自然语言翻译审计(设计文档 §4.3):追加式,确认流程不更新
	// 已有行,只追加新行;同 open_id 按时序读就是完整证据链,最新在前。
	t.Run("CommandTranslationsAppendOnly", func(t *testing.T) {
		s := newStore(t)
		rows := []CommandTranslation{
			{OpenID: "ou_1", RawText: "看下设备状态", PromptVersion: "cmd_translate_v1",
				Model: "m", ContextDigest: "abc", Output: []byte(`{"command":"devices"}`),
				Rendered: "devices", Outcome: OutcomeExecuted},
			{OpenID: "ou_1", RawText: "重跑昨天那个", ContextDigest: "def",
				Output: []byte(`{"command":"rerun"}`), Rendered: "rerun 9da3b9d9 56",
				Outcome: OutcomePendingConfirm},
			{OpenID: "ou_1", RawText: "y", Rendered: "rerun 9da3b9d9 56", Outcome: OutcomeConfirmed},
		}
		for _, r := range rows {
			if err := s.SaveCommandTranslation(ctx, r); err != nil {
				t.Fatalf("SaveCommandTranslation: %v", err)
			}
		}
		// 追加式审计:三行都在,顺序即时序
		got, err := s.ListCommandTranslations(ctx, "ou_1", 10)
		if err != nil {
			t.Fatalf("ListCommandTranslations: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("len = %d, want 3", len(got))
		}
		if got[0].Outcome != OutcomeConfirmed {
			t.Errorf("最新一行 outcome = %q, want %q", got[0].Outcome, OutcomeConfirmed)
		}
	})

	t.Run("CommandTranslationTruncatesOutput", func(t *testing.T) {
		s := newStore(t)
		big := append([]byte(`{"junk":"`), bytes.Repeat([]byte("x"), 8000)...)
		big = append(big, []byte(`"}`)...)
		if err := s.SaveCommandTranslation(ctx, CommandTranslation{
			OpenID: "ou_2", RawText: "x", Output: big, Outcome: OutcomeRejectedSchema,
		}); err != nil {
			t.Fatalf("SaveCommandTranslation: %v", err)
		}
		got, err := s.ListCommandTranslations(ctx, "ou_2", 1)
		if err != nil {
			t.Fatalf("ListCommandTranslations: %v", err)
		}
		// 断言"确实截断了",而非仅仅"没有超过某个宽松上限"(原始 8011 字节本身
		// 就合法 JSON 且小于 outputLimit*2,松散上限无法区分"正确截断"与"完全
		// 没截断")。两个后端都应满足:落库字节数严格小于原始输入,且带尾标记。
		stored := string(got[0].Output)
		if len(stored) >= len(big) {
			t.Errorf("output 未截断: %d 字节(原始 %d)", len(stored), len(big))
		}
		if !strings.Contains(stored, truncatedMark) {
			n := 80
			if len(stored) < n {
				n = len(stored)
			}
			t.Errorf("截断后应带尾标记 %q, got %q...", truncatedMark, stored[:n])
		}
	})

	t.Run("RecentRunsFiltersByTestID", func(t *testing.T) {
		s := newStore(t)
		const proj, sha, iid = "Algo_Super_SDK", "9da3b9d9", 56 // 项目名含下划线:通配符地雷
		v1, v2, v3 := "aarch64_Android_SNPE_1.68", "aarch64_Android_SNPE_2.21", "aarch64_Android_RKNN_2.3.2"
		base := wf.BaseWorkflowID(proj, sha, iid)

		if err := s.RegisterArtifacts(ctx, []Artifact{
			{Project: proj, CommitSHA: sha, PipelineID: iid, Variant: v1, URL: "u1", SHA256: "s1"},
			{Project: proj, CommitSHA: sha, PipelineID: iid, Variant: v2, URL: "u2", SHA256: "s2"},
			{Project: proj, CommitSHA: sha, PipelineID: iid, Variant: v3, URL: "u3", SHA256: "s3"},
		}); err != nil {
			t.Fatalf("RegisterArtifacts: %v", err)
		}

		// bundle workflow:两个变体的 task 挂在同一个 workflow_id 上,靠 test_id 区分
		for _, tc := range []struct{ variant, verdict string }{{v1, "TEST_FAILED"}, {v2, "PASSED"}} {
			taskID := base + ":" + tc.variant + ":a1"
			if err := s.CreateTask(ctx, wf.TaskRow{
				TaskID: taskID, WorkflowID: base, TestID: tc.variant, Attempt: 1,
				IdempotencyKey: taskID, Status: "RUNNING",
			}); err != nil {
				t.Fatalf("CreateTask: %v", err)
			}
			if err := s.FinishTask(ctx, wf.FinishRequest{
				TaskID: taskID, Status: "COMPLETED", Verdict: tc.verdict,
			}); err != nil {
				t.Fatalf("FinishTask: %v", err)
			}
		}

		// 纯变体级 workflow(无 bundle、无 retry 后缀):kick 单变体触发的最常见形态,
		// Attempt=0 且 workflow_id = base-{variant},不带 -r{N}。
		plainWF := base + "-" + v3
		plainTask := plainWF + ":" + v3 + ":a1"
		if err := s.CreateTask(ctx, wf.TaskRow{
			TaskID: plainTask, WorkflowID: plainWF, TestID: v3, Attempt: 0,
			IdempotencyKey: plainTask, Status: "RUNNING",
		}); err != nil {
			t.Fatalf("CreateTask plain variant-level: %v", err)
		}
		if err := s.FinishTask(ctx, wf.FinishRequest{
			TaskID: plainTask, Status: "COMPLETED", Verdict: "PASSED",
		}); err != nil {
			t.Fatalf("FinishTask plain variant-level: %v", err)
		}

		runs, err := s.RecentRuns(ctx, 10)
		if err != nil {
			t.Fatalf("RecentRuns: %v", err)
		}
		byVariant := map[string]RecentRun{}
		for _, r := range runs {
			byVariant[r.Variant] = r
		}
		if got := byVariant[v1].Verdict; got != "TEST_FAILED" {
			t.Errorf("%s verdict = %q, want TEST_FAILED(bundle 下必须按 test_id 过滤,不得串变体)", v1, got)
		}
		if got := byVariant[v2].Verdict; got != "PASSED" {
			t.Errorf("%s verdict = %q, want PASSED", v2, got)
		}
		if got := byVariant[v3].Verdict; got != "PASSED" {
			t.Errorf("%s verdict = %q, want PASSED(纯变体级 workflow,无 bundle/retry 后缀)", v3, got)
		}

		// 变体级 retry workflow:更晚的行应覆盖 bundle 的结论
		retryWF := base + "-" + v1 + "-r2"
		retryTask := retryWF + ":" + v1 + ":a1"
		if err := s.CreateTask(ctx, wf.TaskRow{
			TaskID: retryTask, WorkflowID: retryWF, TestID: v1, Attempt: 1,
			IdempotencyKey: retryTask, Status: "RUNNING",
		}); err != nil {
			t.Fatalf("CreateTask retry: %v", err)
		}
		if err := s.FinishTask(ctx, wf.FinishRequest{
			TaskID: retryTask, Status: "COMPLETED", Verdict: "PASSED",
		}); err != nil {
			t.Fatalf("FinishTask retry: %v", err)
		}
		runs, err = s.RecentRuns(ctx, 10)
		if err != nil {
			t.Fatalf("RecentRuns after retry: %v", err)
		}
		for _, r := range runs {
			if r.Variant == v1 && r.Verdict != "PASSED" {
				t.Errorf("retry 后 %s verdict = %q, want PASSED(应取最新一条)", v1, r.Verdict)
			}
		}

		// 对抗性项目:与 proj 等长,仅在下划线位置替换为普通字符
		// (Algo_Super_SDK → AlgoXSuper_SDK)。若查询实现从 starts_with 退化为
		// LIKE,base 拼出的模式里那个下划线会退化成单字符通配符,adversary 的
		// 变体级 workflow 就会被误判为 proj 的前缀匹配,顶掉 proj/v1 刚判定
		// 出的 PASSED。
		const advProj = "AlgoXSuper_SDK"
		if len(advProj) != len(proj) {
			t.Fatalf("adversary 项目名长度必须与 proj 一致才能对齐下划线位置: %d vs %d", len(advProj), len(proj))
		}
		advBase := wf.BaseWorkflowID(advProj, sha, iid)
		// 复用与 proj 完全相同的 (commit, pipeline, variant) 三元组注册:
		// artifacts 的唯一键不含 project(schema.sql: UNIQUE(commit_sha, pipeline_id,
		// variant)),这里必然是空操作(proj 的 v1 行已占住该键),不影响下方断言,
		// 保留调用只是如实反映"两个项目撞在同一逻辑键上"的场景。
		if err := s.RegisterArtifacts(ctx, []Artifact{
			{Project: advProj, CommitSHA: sha, PipelineID: iid, Variant: v1, URL: "adv", SHA256: "adv"},
		}); err != nil {
			t.Fatalf("RegisterArtifacts adversary: %v", err)
		}
		advWF := advBase + "-" + v1
		advTask := advWF + ":" + v1 + ":a1"
		if err := s.CreateTask(ctx, wf.TaskRow{
			TaskID: advTask, WorkflowID: advWF, TestID: v1, Attempt: 1,
			IdempotencyKey: advTask, Status: "RUNNING",
		}); err != nil {
			t.Fatalf("CreateTask adversary: %v", err)
		}
		if err := s.FinishTask(ctx, wf.FinishRequest{
			TaskID: advTask, Status: "COMPLETED", Verdict: "INFRA_ERROR",
		}); err != nil {
			t.Fatalf("FinishTask adversary: %v", err)
		}
		runs, err = s.RecentRuns(ctx, 10)
		if err != nil {
			t.Fatalf("RecentRuns after adversary: %v", err)
		}
		byVariant = map[string]RecentRun{}
		for _, r := range runs {
			byVariant[r.Variant] = r
		}
		if got := byVariant[v1].Verdict; got != "PASSED" {
			t.Errorf("%s verdict = %q, want PASSED(不得被下划线位置不同的对抗项目 %s 抢走结论)", v1, got, advProj)
		}
	})

	t.Run("RecentRunsRespectsLimit", func(t *testing.T) {
		s := newStore(t)
		arts := []Artifact{}
		for i := 0; i < 5; i++ {
			arts = append(arts, Artifact{
				// 每个产物的 variant 各异,避免与 (commit_sha, pipeline_id, variant)
				// 唯一键无关地互相覆盖——同时也让"是哪一行"可以只凭 Commit 辨认。
				Project: "p", CommitSHA: fmt.Sprintf("sha%d", i), PipelineID: i + 1,
				Variant: fmt.Sprintf("v%d", i), URL: "u", SHA256: "s",
			})
		}
		if err := s.RegisterArtifacts(ctx, arts); err != nil {
			t.Fatalf("RegisterArtifacts: %v", err)
		}
		runs, err := s.RecentRuns(ctx, 3)
		if err != nil {
			t.Fatalf("RecentRuns: %v", err)
		}
		if len(runs) != 3 {
			t.Fatalf("len = %d, want 3", len(runs))
		}
		// 断言具体是"哪" 3 条、且新到旧排序——只查 count 时,从错误的一端截断
		// (返回最旧的 3 条,或返回正确数量但顺序颠倒)也能通过。
		wantCommits := []string{"sha4", "sha3", "sha2"} // 最后注册的 3 条,新→旧
		for i, want := range wantCommits {
			if runs[i].Commit != want {
				t.Errorf("runs[%d].Commit = %q, want %q(应为最近注册的 3 条,新→旧排序)",
					i, runs[i].Commit, want)
			}
		}
	})
}
