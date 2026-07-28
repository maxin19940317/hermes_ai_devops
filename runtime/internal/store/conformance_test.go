package store

import (
	"bytes"
	"context"
	"encoding/json"
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
	ReleaseDevice(ctx context.Context, deviceID, taskID string, infraFail bool, quarantineAfter int) error
	RenewLease(ctx context.Context, cred LeaseCredential, leaseSeconds int) (bool, error)
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
	SaveDecision(ctx context.Context, row wf.DecisionRow) error
	ListDecisions(ctx context.Context, taskID string) ([]wf.DecisionRow, error)
	NextWorkflowAttempt(ctx context.Context, commitSHA string, pipelineID int, variant string) (int, error)
	SaveEvidenceSnapshot(ctx context.Context, snap EvidenceSnapshot) error
	GetEvidenceSnapshot(ctx context.Context, evidenceID string) (*EvidenceSnapshot, error)
	FleetOverview(ctx context.Context) (*FleetOverview, error)
	UnquarantineDevice(ctx context.Context, deviceID string) (bool, error)
	ListArtifacts(ctx context.Context, commitSHA string, pipelineID int) ([]Artifact, error)
	NextWorkflowAttemptAll(ctx context.Context, commitSHA string, pipelineID int) (int, error)
	SaveCommandTranslation(ctx context.Context, row CommandTranslation) error
	ListCommandTranslations(ctx context.Context, openID string, limit int) ([]CommandTranslation, error)
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
					if err := s.ReleaseDevice(ctx, l.DeviceID, "w:t1:a1", false, 3); err != nil {
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
		if err := s.ReleaseDevice(ctx, l.DeviceID, "w:t1:a1", false, 3); err != nil {
			t.Fatal(err)
		}
		if exp, err := s.GetLeaseExpiry(ctx, "w:t1:a1"); err != nil || exp != nil {
			t.Errorf("释放后: exp=%v err=%v, want (nil, nil)", exp, err)
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
		if err := s.ReleaseDevice(ctx, l.DeviceID, "dead-task", true, 3); err != nil {
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
		if err := s.ReleaseDevice(ctx, l.DeviceID, "w:t1:a1", false, 3); err != nil {
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
			_ = s.ReleaseDevice(ctx, l2.DeviceID, "w:t2:a1", true, 3)
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
}
