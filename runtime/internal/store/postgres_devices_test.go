package store

import (
	"testing"

	wf "hermes-devops/runtime/internal/workflow"
)

// TestLockOneCandidateLocksAtMostOneRow 验证 AcquireDevice 的候选锁定只锁住
// 将被选中的那一行,不连带锁住同样匹配但不会使用的候选行(§11 device_leases 独占的
// 本意是"独占被选中的设备",不是"独占整个候选集合")。否则一次未提交的 Acquire 会
// 无谓阻塞另一次原本可以立即成功的并发 Acquire。
func TestLockOneCandidateLocksAtMostOneRow(t *testing.T) {
	s := openTestPG(t)
	if err := s.UpsertClientDevices(ctx, Client{ClientID: "c1", BaseURL: "https://client:8443"},
		[]Device{
			{DeviceID: "dev-a", Serial: "dev-a", ClientID: "c1", SOC: "SOC_A"},
			{DeviceID: "dev-b", Serial: "dev-b", ClientID: "c1", SOC: "SOC_A"},
		}); err != nil {
		t.Fatal(err)
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	// 两台设备都匹配这个 selector,只应取用其中一台(按 device_id 排序,取 dev-a)。
	chosen, err := s.lockOneCandidate(ctx, tx, wf.DeviceSelector{SOC: []string{"SOC_A"}})
	if err != nil {
		t.Fatalf("lockOneCandidate: %v", err)
	}
	if chosen == nil || chosen.DeviceID != "dev-a" {
		t.Fatalf("chosen = %+v, want dev-a", chosen)
	}

	// 用独立连接以 NOWAIT 直接探测锁状态:被选中的行必须锁住(NOWAIT 立即报错),
	// 未被选中但同样匹配的候选行必须仍然可锁(证明没有被连带锁住)。
	tx2, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback()

	if _, err := tx2.ExecContext(ctx, `SELECT device_id FROM devices WHERE device_id = 'dev-b' FOR UPDATE NOWAIT`); err != nil {
		t.Errorf("dev-b 未被选中,不应被连带锁住: %v", err)
	}
	if _, err := tx2.ExecContext(ctx, `SELECT device_id FROM devices WHERE device_id = 'dev-a' FOR UPDATE NOWAIT`); err == nil {
		t.Error("dev-a 是被选中的行,应当已被锁住(NOWAIT 应立即报错)")
	}
}

func TestLockOneCandidateFiltersBySelectorInSQL(t *testing.T) {
	s := openTestPG(t)
	if err := s.UpsertClientDevices(ctx, Client{ClientID: "c1", BaseURL: "https://client:8443"},
		[]Device{
			{DeviceID: "dev-a", Serial: "dev-a", ClientID: "c1", SOC: "SOC_A"},
		}); err != nil {
		t.Fatal(err)
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	d, err := s.lockOneCandidate(ctx, tx, wf.DeviceSelector{SOC: []string{"SOC_OTHER"}})
	if err != nil {
		t.Fatalf("lockOneCandidate: %v", err)
	}
	if d != nil {
		t.Fatalf("d = %+v, want nil(soc 不匹配不应被锁定/返回)", d)
	}
}
