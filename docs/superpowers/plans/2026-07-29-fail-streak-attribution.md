# 失败归因分离 实施计划（差距 #10）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把单一的 `fail_streak` 拆成设备级与 client 级两个计数器，让 Runtime 自身故障不再把无辜设备推向隔离。

**Architecture:** 归因由 workflow 在每个释放点显式给出（四个取值 `ok`/`device`/`client`/`none`），经 activity 传到 store 记账。`ReleaseRequest` 只加不删并用 `workflow.GetVersion` 分支，保证在途 workflow 重放不失败。

**Tech Stack:** Go 1.22+、Temporal Go SDK、PostgreSQL 15。

**设计文档:** `docs/superpowers/specs/2026-07-29-fail-streak-attribution-design.md`（已批准）

## Global Constraints

- 四个 scope 取值固定为 `ok` / `device` / `client` / `none`，语义见设计 §4：
  `ok` 清零两个计数器；`device` 设备计数 +1（达阈值 QUARANTINED）；`client` client 计数 +1；
  `none` **两个计数器都不动**。
- `none` 与 `ok` 不可合并：Runtime 故障不是设备健康的证据（不能清零），也不是设备的错（不能加一）。
- `ReleaseRequest` **只加字段不删字段**：保留 `InfraFail bool`，新增 `FailScope`。
- workflow 侧用 `workflow.GetVersion(ctx, "release-fail-scope", workflow.DefaultVersion, 1)` 分支；
  旧分支必须产出与改动前**逐字节相同**的 activity 载荷。
- Activity 侧兼容空 `FailScope`：按旧语义（`InfraFail=true` → `device`，`false` → `ok`）。
- `devices.fail_streak` **不重命名**，语义收窄为设备级。新增 `clients.fail_streak`。
- 释放与两个计数器的更新在**同一事务**内完成；失败整体回滚由 activity 重试。
- `ReleaseDevice` 既有幂等语义不变：非持有者释放、租约已易主、重复释放一律无副作用且**不计数**。
- `device_fail_streak` 在本轮之后恒为 0 是**预期状态**（无信号源，设计 §7），代码注释必须写明。
- 隔离阈值仍取 `a.Cfg.QuarantineAfter`（§10 缺省 3），只对 `device` scope 生效。
- Go 错误用 wrapped errors；注释中文；提交信息英文。

**命令速查：**

```bash
# 本机无系统 Go,工具链在 scratchpad(见 .superpowers/sdd/task-8-report.md)
cd runtime && go build ./... && go vet ./... && go test ./...
cd /home/maxin/Code/hermes_ai_devops && .venv/bin/python -m pytest deploy/tests -q
```

---

### Task 1: store 层按 scope 记账

**Files:**
- Modify: `runtime/internal/workflow/devicetest.go`（新增 `FailScope` 类型与常量）
- Modify: `runtime/internal/store/schema.sql`（clients 加列）
- Create: `deploy/postgres/migrations/2026-07-29-client-fail-streak.sql`
- Modify: `runtime/internal/store/store.go`（MemStore 加 `clientFailStreak`）
- Modify: `runtime/internal/store/devices.go`（MemStore.ReleaseDevice）
- Modify: `runtime/internal/store/postgres_devices.go`（PGStore.ReleaseDevice）
- Modify: `runtime/internal/store/conformance_test.go`

**Interfaces:**
- Consumes: 无
- Produces:
  - `wf.FailScope` string 类型 + 常量 `wf.FailScopeOK` / `FailScopeDevice` / `FailScopeClient` / `FailScopeNone`
  - `store.ReleaseDevice(ctx, deviceID, taskID string, scope wf.FailScope, quarantineAfter int) error`（签名变更）
  - `MemStore.ClientFailStreak(clientID string) int`（仅测试与 Task 4 读取用）

- [ ] **Step 1: 定义 FailScope 类型**

在 `runtime/internal/workflow/devicetest.go` 的 `ReleaseRequest` 定义**之前**加入：

```go
// FailScope 是一次设备释放的失败归因(设计文档 §4)。四个取值互斥:
//
//	ok     终态且非 INFRA 类判定 → 两个计数器都清零
//	device 设备级失败           → devices.fail_streak+1,达阈值 QUARANTINED
//	client Client Agent 或与它之间的网络 → clients.fail_streak+1
//	none   Runtime 自身故障/取消/成因两可 → 两个计数器都不动
//
// none 与 ok 不可合并:Runtime 挂了既不是设备健康的证据(不能清零),
// 也不是设备的错(不能加一)。改动前这两种情况都被记成"设备又坏了一次"。
type FailScope string

const (
	FailScopeOK     FailScope = "ok"
	FailScopeDevice FailScope = "device"
	FailScopeClient FailScope = "client"
	FailScopeNone   FailScope = "none"
)
```

- [ ] **Step 2: 写失败的 conformance 子测试**

`conformance_test.go` 的形态是 `fullStore` 接口 + `runConformance` 内的 `t.Run` 子测试，
`ctx` 是包级变量。先把接口里 `ReleaseDevice` 的签名改成新的：

```go
	ReleaseDevice(ctx context.Context, deviceID, taskID string, scope wf.FailScope, quarantineAfter int) error
```

再在 `runConformance` 内追加子测试（既有调用 `ReleaseDevice(ctx, ..., true/false, ...)` 的
子测试同步改成 `wf.FailScopeDevice` / `wf.FailScopeOK`，保持原断言不变）：

```go
	// 归因记账(差距 #10):四个 scope 各记各的账,互不串味。
	t.Run("ReleaseDeviceFailScopes", func(t *testing.T) {
		cases := []struct {
			name            string
			scope           wf.FailScope
			wantDeviceFail  int
			wantClientFail  int
			wantStatus      string
		}{
			{"device 只增设备计数", wf.FailScopeDevice, 1, 0, "IDLE"},
			{"client 只增 client 计数", wf.FailScopeClient, 0, 1, "IDLE"},
			{"none 两个都不动", wf.FailScopeNone, 0, 0, "IDLE"},
			{"ok 两个都清零", wf.FailScopeOK, 0, 0, "IDLE"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s := newStore(t)
				seed(t, s)
				lease, err := s.AcquireDevice(ctx, wf.DeviceSelector{}, "w:t1:a1", 120)
				if err != nil || lease == nil {
					t.Fatalf("acquire = %+v err=%v", lease, err)
				}
				if err := s.ReleaseDevice(ctx, lease.DeviceID, "w:t1:a1", tc.scope, 3); err != nil {
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
```

`DeviceStatus.ClientFailStreak` 由 Task 4 补齐——本任务先让它编译通过：在
`runtime/internal/store/fleet.go` 的 `DeviceStatus` 结构体加两个字段并在两套
`FleetOverview` 里填好（Task 4 只负责把它接到飞书输出）：

```go
	ClientID         string // 归属 client
	ClientFailStreak int    // 该 client 的连续失败计数(差距 #10)
```

- [ ] **Step 3: 跑测试确认失败**

Run: `cd runtime && go test ./internal/store/ -run Conformance/ReleaseDevice`
Expected: 编译失败 —— `undefined: wf.FailScopeDevice` / `DeviceStatus` 无 `ClientFailStreak`

- [ ] **Step 4: 加 DDL 与迁移**

`runtime/internal/store/schema.sql` 的 `clients` 表定义里加一列：

```sql
    fail_streak    INTEGER     NOT NULL DEFAULT 0,   -- client 级连续失败(差距 #10)
```

创建 `deploy/postgres/migrations/2026-07-29-client-fail-streak.sql`：

```sql
-- 失败归因分离(差距 #10)。clients 加 client 级计数;
-- devices.fail_streak 保留原名,语义收窄为"设备级"。
-- 幂等:IF NOT EXISTS + 幂等 UPDATE,重复执行无副作用。

BEGIN;

ALTER TABLE clients ADD COLUMN IF NOT EXISTS fail_streak INTEGER NOT NULL DEFAULT 0;

-- 历史值按旧(错误)语义累计:client 侧失败、超时、Runtime 自身故障都记在设备头上。
-- 语义既然收窄为"设备级",旧值就不该带进新语义——归零重新开始计。
UPDATE devices SET fail_streak = 0;

COMMIT;
```

- [ ] **Step 5: MemStore 实现**

`runtime/internal/store/store.go` 的 `MemStore` 结构体加字段：

```go
	// clientFailStreak 是 clients.fail_streak 的内存视图(差距 #10)。
	// 不放进 Client 结构体:那是心跳载荷,UpsertClientDevices 整体覆写,
	// 放进去会被每 10s 一次的心跳清零。
	clientFailStreak map[string]int
```

`NewMemStore()` 里初始化 `clientFailStreak: map[string]int{}`。

`runtime/internal/store/devices.go` 的 `ReleaseDevice` 整体替换为：

```go
// ReleaseDevice 归还租约(置 released_at,行保留作审计;§10/差距 #15)并按归因记账
// (差距 #10,设计文档 §4):
//
//	device → 设备计数 +1,达 quarantineAfter 则 QUARANTINED
//	client → 该设备所属 client 的计数 +1,设备计数不动
//	none   → 两个计数器都不动(Runtime 自身故障不是设备的错,也不是它健康的证据)
//	ok     → 两个计数器都清零
//
// 非租约持有者释放/租约已易主/重复释放:幂等,无副作用,且不计数。
//
// 注:device scope 目前无信号源(rules.CategoryDevice 无人产出,见设计 §7),
// 因此 devices.fail_streak 恒为 0、隔离不再触发——这是预期状态,不是缺陷。
func (s *MemStore) ReleaseDevice(_ context.Context, deviceID, taskID string, scope wf.FailScope, quarantineAfter int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.devices[deviceID]
	if !ok || row.LeaseTaskID != taskID || row.Released {
		return nil // 重复释放/租约已易主:幂等,无副作用
	}
	row.Released = true
	row.LeaseTaskID = ""
	row.LeaseExpiresAt = time.Time{}
	switch scope {
	case wf.FailScopeDevice:
		row.FailStreak++
		if row.FailStreak >= quarantineAfter {
			row.Status = DeviceQuarantined
			return nil
		}
	case wf.FailScopeClient:
		s.clientFailStreak[row.ClientID]++
	case wf.FailScopeOK:
		row.FailStreak = 0
		s.clientFailStreak[row.ClientID] = 0
	case wf.FailScopeNone:
		// 两个计数器都不动
	}
	row.Status = DeviceIdle
	return nil
}

// ClientFailStreak 返回某 client 的连续失败计数(差距 #10);无记录返回 0。
func (s *MemStore) ClientFailStreak(clientID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clientFailStreak[clientID]
}
```

在 `fleet.go` 的 `MemStore.FleetOverview` 里填新字段（在构造 `DeviceStatus` 处）：

```go
			ClientID: row.ClientID, ClientFailStreak: s.clientFailStreak[row.ClientID],
```

- [ ] **Step 6: PGStore 实现**

`runtime/internal/store/postgres_devices.go` 的 `ReleaseDevice` 整体替换为：

```go
// ReleaseDevice 归还租约并按归因记账(差距 #10,设计文档 §4)。语义见 MemStore 同名方法。
// 三段 CTE 一条语句 = 单事务:lease 实际释放了才会有下游行,因此重复释放/
// 非持有者释放天然不计数(WHERE 匹配不到行,整条语句空转)。
func (s *PGStore) ReleaseDevice(ctx context.Context, deviceID, taskID string, scope wf.FailScope, quarantineAfter int) error {
	_, err := s.DB.ExecContext(ctx, `
		WITH lease AS (
			UPDATE device_leases SET released_at = now()
			WHERE device_id = $1 AND task_id = $2 AND released_at IS NULL
			RETURNING device_id
		), dev AS (
			UPDATE devices SET
				status = CASE
					WHEN $3 = 'device' AND fail_streak + 1 >= $4 THEN 'QUARANTINED'
					ELSE 'IDLE'
				END,
				fail_streak = CASE
					WHEN $3 = 'device' THEN fail_streak + 1
					WHEN $3 = 'ok'     THEN 0
					ELSE fail_streak
				END
			WHERE device_id IN (SELECT device_id FROM lease)
			RETURNING client_id
		)
		UPDATE clients SET fail_streak = CASE
			WHEN $3 = 'client' THEN fail_streak + 1
			WHEN $3 = 'ok'     THEN 0
			ELSE fail_streak
		END
		WHERE client_id IN (SELECT client_id FROM dev)`,
		deviceID, taskID, string(scope), quarantineAfter)
	if err != nil {
		return fmt.Errorf("release device %s scope=%s: %w", deviceID, scope, err)
	}
	return nil
}
```

`runtime/internal/store/postgres_fleet.go` 的 `FleetOverview` 查询加 client 两列：

```go
	rows, err := s.DB.QueryContext(ctx, `
		SELECT d.device_id, d.serial, d.soc, d.status, d.fail_streak,
		       COALESCE(l.task_id, ''), d.client_id, COALESCE(c.fail_streak, 0)
		FROM devices d
		LEFT JOIN device_leases l ON l.device_id = d.device_id AND l.released_at IS NULL
		LEFT JOIN clients c ON c.client_id = d.client_id
		ORDER BY d.device_id`)
```

对应 `Scan` 加两个目标：

```go
		if err := rows.Scan(&d.DeviceID, &d.Serial, &d.SOC, &d.Status, &d.FailStreak,
			&d.LeaseTaskID, &d.ClientID, &d.ClientFailStreak); err != nil {
```

- [ ] **Step 7: 跑测试**

Run: `cd runtime && go test ./internal/store/ -run Conformance -v 2>&1 | tail -30`
Expected: 全部 PASS，MemStore 与 PGStore 两套都过

- [ ] **Step 8: 验证隔离用例真的会失败**

把 `MemStore.ReleaseDevice` 的 `case wf.FailScopeClient:` 临时改成 `row.FailStreak++`
（模拟归因串味的回归），跑：

Run: `cd runtime && go test ./internal/store/ -run Conformance/ReleaseDeviceOnlyDeviceScopeQuarantines`
Expected: **FAIL**，报 "client 连续 5 次仍不得隔离设备"

改回来，确认恢复 PASS。把两次输出记进报告。

- [ ] **Step 9: Commit**

```bash
git add runtime/internal/store/ runtime/internal/workflow/devicetest.go deploy/postgres/migrations/
git commit -m "feat(store): attribute device release failures by scope"
```

---

### Task 2: activity 载荷与旧语义兼容

**Files:**
- Modify: `runtime/internal/workflow/devicetest.go`（`ReleaseRequest` 加字段）
- Modify: `runtime/internal/activity/acts.go`（Store 接口 + `ReleaseDevice` 活动）
- Modify: `runtime/internal/activity/acts_test.go`（若无此文件则创建）

**Interfaces:**
- Consumes: Task 1 的 `wf.FailScope` 常量、`store.ReleaseDevice` 新签名
- Produces: `wf.ReleaseRequest{DeviceID, TaskID, InfraFail, FailScope}`；
  `Acts.ReleaseDevice` 对空 `FailScope` 的旧语义回退

- [ ] **Step 1: 写失败的测试**

该包的测试用真 `store.MemStore`（见 `acts_test.go:23` 的 `storeWithDevice`），不用 fake。
沿用同样的写法，通过 `FleetOverview` 观察记账结果：

```go
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
```

顺带留意：`acts_test.go` 里既有的 `TestStoreActsPassConfigThrough` 用的正是
`wf.ReleaseRequest{..., InfraFail: ...}` 的旧载荷形态，并断言"连续 3 次 INFRA 释放后
设备隔离"。改动后它会走空 `FailScope` → `device` 的兼容分支，**必须继续通过**——
它是旧语义未被破坏的现成回归测试，不要改写它。

- [ ] **Step 2: 跑测试确认失败**

Run: `cd runtime && go test ./internal/activity/ -run ReleaseDeviceLegacy`
Expected: 编译失败 —— `ReleaseRequest` 无 `FailScope` 字段

- [ ] **Step 3: 扩展 ReleaseRequest**

`runtime/internal/workflow/devicetest.go`：

```go
type ReleaseRequest struct {
	DeviceID string `json:"device_id"`
	TaskID   string `json:"task_id"`
	// InfraFail 是改动前的归因字段。**保留不删**:它进过 workflow history,
	// 在途 workflow 重放时会原样送回来(设计文档 §5)。
	InfraFail bool `json:"infra_fail"`
	// FailScope 是新的四值归因(差距 #10)。为空 = 旧载荷,活动按 InfraFail 翻译。
	FailScope FailScope `json:"fail_scope,omitempty"`
}
```

- [ ] **Step 4: 活动侧翻译**

`runtime/internal/activity/acts.go` 的 Store 接口改签名：

```go
	ReleaseDevice(ctx context.Context, deviceID, taskID string, scope wf.FailScope, quarantineAfter int) error
```

`Acts.ReleaseDevice` 改为：

```go
// ReleaseDevice 归还设备并按归因记账。空 FailScope 表示载荷来自改动前的
// workflow(重放场景,设计文档 §5):按旧语义翻译,保持当初的记账行为。
func (a *Acts) ReleaseDevice(ctx context.Context, req wf.ReleaseRequest) error {
	scope := req.FailScope
	if scope == "" {
		scope = wf.FailScopeOK
		if req.InfraFail {
			scope = wf.FailScopeDevice
		}
	}
	return a.Store.ReleaseDevice(ctx, req.DeviceID, req.TaskID, scope, a.Cfg.QuarantineAfter)
}
```

- [ ] **Step 5: 跑测试**

Run: `cd runtime && go test ./internal/activity/ -v -run ReleaseDeviceLegacy`
Expected: 4 个子用例全 PASS

- [ ] **Step 6: Commit**

```bash
git add runtime/internal/activity/ runtime/internal/workflow/devicetest.go
git commit -m "feat(activity): carry fail scope with legacy payload fallback"
```

---

### Task 3: workflow 归因表与版本分支

**Files:**
- Modify: `runtime/internal/workflow/devicetest.go`
- Modify: `runtime/internal/workflow/devicetest_test.go`

**Interfaces:**
- Consumes: Task 1 的 `FailScope` 常量、Task 2 的 `ReleaseRequest.FailScope`
- Produces:
  - `type releaseSite string` + 常量（见 Step 3）
  - `func failScope(site releaseSite, category rules.Category, resultStatus string) FailScope`
  - `awaitResult` 签名变为 `(releaseSite, string)`

- [ ] **Step 1: 写失败的归因表测试**

`runtime/internal/workflow/devicetest_test.go` 追加：

```go
// 归因表(设计文档 §4)。每一行一个用例;特别钉住两条改动前会误伤设备的:
// check lease 失败 → none(不是 device),终态 INFRA+FAILED → client(不是 device)。
func TestFailScope(t *testing.T) {
	cases := []struct {
		name     string
		site     releaseSite
		category rules.Category
		status   string
		want     FailScope
	}{
		{"CreateTask 失败是 Runtime 侧", siteCreateTaskFailed, "", "", FailScopeNone},
		{"Dispatch 失败连不上 agent", siteDispatchFailed, "", "", FailScopeClient},
		{"租约过期即 agent 失联", siteLeaseExpired, "", "", FailScopeClient},
		{"CheckLease 自身失败是 Runtime 侧", siteCheckLeaseFailed, "", "", FailScopeNone},
		{"hard deadline 成因两可", siteHardDeadline, "", "", FailScopeNone},
		{"人为取消", siteCanceled, "", "", FailScopeNone},
		{"LoadResult 失败是 outbox/DB", siteLoadResultFailed, "", "", FailScopeNone},
		{"终态 DEVICE 类", siteTerminal, rules.CategoryDevice, "FAILED", FailScopeDevice},
		{"终态 INFRA+FAILED 是 client 流水线", siteTerminal, rules.CategoryInfra, "FAILED", FailScopeClient},
		{"终态 INFRA+TIMEOUT 是工作负载属性", siteTerminal, rules.CategoryInfra, "TIMEOUT", FailScopeNone},
		{"终态 PASSED", siteTerminal, "", "COMPLETED", FailScopeOK},
		{"终态 CODE 类测试失败", siteTerminal, rules.CategoryCode, "COMPLETED", FailScopeOK},
		{"未覆盖组合保守取 none", releaseSite("unknown"), "", "", FailScopeNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := failScope(tc.site, tc.category, tc.status); got != tc.want {
				t.Errorf("failScope(%q, %q, %q) = %q, want %q",
					tc.site, tc.category, tc.status, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd runtime && go test ./internal/workflow/ -run TestFailScope`
Expected: 编译失败 —— `undefined: failScope` / `undefined: siteCreateTaskFailed`

- [ ] **Step 3: 实现归因函数**

在 `devicetest.go` 的 `runAttempt` 之前加入：

```go
// releaseSite 标识释放发生在 workflow 的哪个失败分支(设计文档 §4)。
// 用枚举而不是解析 reason 字符串:workflow 本来就知道自己站在哪个分支上。
type releaseSite string

const (
	siteCreateTaskFailed releaseSite = "create_task_failed"
	siteDispatchFailed   releaseSite = "dispatch_failed"
	siteLeaseExpired     releaseSite = "lease_expired"
	siteCheckLeaseFailed releaseSite = "check_lease_failed"
	siteHardDeadline     releaseSite = "hard_deadline"
	siteCanceled         releaseSite = "canceled"
	siteLoadResultFailed releaseSite = "load_result_failed"
	siteTerminal         releaseSite = "terminal"
)

// failScope 决定一次释放该记在谁头上(设计文档 §4 归因表)。纯函数,表驱动单测。
//
// 终态分支需要 resultStatus:d.Category 单独不足以区分 FAILED(client 侧流水线
// 失败)与 TIMEOUT(工作负载属性)——两者都是 CategoryInfra。
func failScope(site releaseSite, category rules.Category, resultStatus string) FailScope {
	switch site {
	case siteDispatchFailed, siteLeaseExpired:
		// 已知盲区(设计文档 §4.1):callbacks 进程自身宕机 ≥120s 时,心跳送不达、
		// 租约照样过期,这里会把 Runtime 的故障记成 client 失联。workflow 视角内
		// 无法区分。本轮无代价(计数不驱动行为);若将来用它做自动处置,必须先解决,
		// 否则 Runtime 重启一次就会把整个 fleet 的 client 全停掉。
		// 判别特征:callbacks 宕机时全 fleet 的 client 计数同时上涨。
		return FailScopeClient
	case siteCreateTaskFailed, siteCheckLeaseFailed, siteHardDeadline,
		siteCanceled, siteLoadResultFailed:
		return FailScopeNone
	case siteTerminal:
		switch {
		case category == rules.CategoryDevice:
			return FailScopeDevice
		case category == rules.CategoryInfra && resultStatus == "FAILED":
			return FailScopeClient
		case category == rules.CategoryInfra:
			return FailScopeNone // TIMEOUT 及其余 INFRA:不归任何一方
		default:
			return FailScopeOK
		}
	}
	return FailScopeNone // 未覆盖组合保守处理:不加不减
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `cd runtime && go test ./internal/workflow/ -run TestFailScope -v`
Expected: 13 个子用例全 PASS

- [ ] **Step 5: awaitResult 返回释放点**

把 `awaitResult` 的签名与 5 处 `return` 改为同时返回 site：

```go
func awaitResult(ctx workflow.Context, taskID string, spec TestSpec, resultCh workflow.ReceiveChannel) (releaseSite, string) {
```

各 return 依次改为：

```go
			return siteHardDeadline, "hard deadline exceeded"
...
				return siteCheckLeaseFailed, "check lease: " + err.Error()
...
				return siteLeaseExpired, "lease expired (no heartbeat)"
...
		if matched {
			return "", ""
		}
		if ctx.Err() != nil {
			return siteCanceled, "workflow canceled"
		}
```

- [ ] **Step 6: release 闭包带版本分支**

`release` 闭包改为同时接收旧布尔与新 scope。**旧分支必须产出与改动前逐字节相同的载荷**，
所以旧布尔由调用方原样给出，不从 scope 反推（`none` 在 CreateTask 处对应 false、
在 await 处对应 true，反推会错）：

```go
	released := false
	// 清理动作的错误不改变 workflow 结论,但绝不能静默(见各分支日志)。
	// legacyInfraFail 是改动前传给 activity 的原值;scope 是新归因(差距 #10)。
	// 两者并存是为了让 workflow.GetVersion 的旧分支能重放出一模一样的载荷(设计 §5)。
	release := func(legacyInfraFail bool, scope FailScope) {
		if released {
			return
		}
		released = true
		req := ReleaseRequest{DeviceID: lease.DeviceID, TaskID: taskID}
		if workflow.GetVersion(dctx, "release-fail-scope", workflow.DefaultVersion, 1) == workflow.DefaultVersion {
			req.InfraFail = legacyInfraFail // 在途 workflow:原样重放
		} else {
			req.FailScope = scope
		}
		if err := workflow.ExecuteActivity(dctx, "ReleaseDevice", req).Get(dctx, nil); err != nil {
			workflow.GetLogger(dctx).Error("release device failed, lease will expire on its own",
				"task", taskID, "device", lease.DeviceID, "scope", scope, "error", err)
		}
	}
```

- [ ] **Step 7: 各调用点传归因**

四处调用依次改为：

```go
		release(false, failScope(siteCreateTaskFailed, "", ""))       // CreateTask 失败
...
		release(true, failScope(siteDispatchFailed, "", ""))          // Dispatch 失败
```

`awaitResult` 处（注意接收两个返回值）：

```go
	if site, infraReason := awaitResult(ctx, taskID, spec, resultCh); infraReason != "" {
		cancel(infraReason)
		finish("FAILED", string(rules.VerdictInfraError), string(rules.CategoryInfra), infraReason)
		release(true, failScope(site, "", ""))
		return infra(infraReason, true)
	}
```

LoadResult 处：

```go
		release(true, failScope(siteLoadResultFailed, "", ""))
```

终态处：

```go
	release(d.Category == rules.CategoryInfra, failScope(siteTerminal, d.Category, res.Status))
```

- [ ] **Step 8: 跑全包测试**

Run: `cd runtime && go build ./... && go test ./internal/workflow/ ./internal/activity/ -v 2>&1 | tail -25`
Expected: 全部 PASS（既有 workflow 用例不得回归）

- [ ] **Step 9: 断言 workflow 送出的 scope**

`devicetest_test.go:97` 的 `fakeActs.ReleaseDevice` 已经把每次释放记进 `f.released`，
直接用它断言归因确实被送到了 activity（而不是只测了纯函数）。挑一个既有的
end-to-end workflow 用例（跑到终态 PASSED 的那个），在末尾追加：

```go
	if len(f.released) != 1 {
		t.Fatalf("released = %d 次, want 1", len(f.released))
	}
	if got := f.released[0].FailScope; got != FailScopeOK {
		t.Errorf("PASSED 终态的 FailScope = %q, want %q", got, FailScopeOK)
	}
	if f.released[0].InfraFail {
		t.Error("新版本分支不该再填 InfraFail")
	}
```

分工说明写进报告：新分支（`version == 1`）由此用例覆盖；旧分支（空 `FailScope`
→ 旧语义）由 Task 2 的 activity 兼容测试覆盖。`workflow.GetVersion` 在测试框架里
对新 workflow 恒返回最大版本，所以旧分支无法在 workflow 层直接触发——这正是把
兼容性断言放在 activity 层的原因。

- [ ] **Step 10: Commit**

```bash
git add runtime/internal/workflow/
git commit -m "feat(workflow): attribute release failures per site with version gate"
```

---

### Task 4: 可观测性与文档

**Files:**
- Modify: `runtime/internal/feishucmd/executor.go`
- Modify: `runtime/internal/feishucmd/executor_test.go`
- Modify: `docs/device-test-sequence.md`

**Interfaces:**
- Consumes: Task 1 的 `DeviceStatus.ClientID` / `DeviceStatus.ClientFailStreak`
- Produces: 无新导出符号

- [ ] **Step 1: 写失败的测试**

`runtime/internal/feishucmd/executor_test.go` 追加：

```go
// 归因拆分后,client 计数必须在飞书输出里可见——否则"这个 client 是不是在
// 持续出问题"无处可查(设计文档决策 2:只计数与展示)。
func TestStatusAndDevicesShowClientFailStreak(t *testing.T) {
	st := store.NewMemStore()
	seedQuarantinedDevice(t, st, "dev-1")
	sender := &fakeSender{}
	e := newExec(st, &fakeStarter{}, sender)
	for _, cmd := range []string{"status", "devices"} {
		sender.texts = nil
		e.HandleMessage(context.Background(), whitelistedOpenID, cmd)
		got := lastText(sender)
		if !strings.Contains(got, "client_fail=") {
			t.Errorf("%s 输出应含 client_fail=, got %q", cmd, got)
		}
	}
}
```

`whitelistedOpenID` 用该文件既有用例里的白名单 id（若既有测试是内联字面量，
照抄同一个值）。`seedQuarantinedDevice` / `newExec` / `fakeSender` / `lastText`
均已存在于该文件。

- [ ] **Step 2: 跑测试确认失败**

Run: `cd runtime && go test ./internal/feishucmd/ -run ShowClientFailStreak`
Expected: FAIL —— 输出里没有 `client_fail=`

- [ ] **Step 3: 改飞书输出**

`executor.go` 的 `status`：

```go
		fmt.Fprintf(&b, "\n  %s %s %s fail_streak=%d client=%s client_fail=%d",
			d.Serial, d.SOC, d.Status, d.FailStreak, d.ClientID, d.ClientFailStreak)
```

`devices`：

```go
		fmt.Fprintf(&b, "%s  soc=%s status=%s fail_streak=%d client=%s client_fail=%d\n",
			d.Serial, d.SOC, d.Status, d.FailStreak, d.ClientID, d.ClientFailStreak)
```

- [ ] **Step 4: 跑测试**

Run: `cd runtime && go test ./internal/feishucmd/ -v 2>&1 | tail -15`
Expected: 全部 PASS（既有 status/devices 用例断言的是子串，不应回归；若既有用例
断言了完整输出行，同步更新其期望值）

- [ ] **Step 5: 更新差距清单**

`docs/device-test-sequence.md` 第 277 行的差距 #10 那一行改为：

```markdown
| 10 | 失败归因 device/client 分离 + 明确重置规则 | **已实现**(2026-07-29):四值归因 ok/device/client/none,Runtime 自身故障不再计入任何一方 | 遗留:`device` 无信号源(rules.CategoryDevice 无人产出),故设备隔离暂不触发,恢复路径见 `docs/superpowers/specs/2026-07-29-fail-streak-attribution-design.md` §7 |
```

- [ ] **Step 6: 全量回归**

Run: `cd runtime && go build ./... && go vet ./... && go test ./...`
Expected: 全部 PASS

Run: `cd /home/maxin/Code/hermes_ai_devops && .venv/bin/python -m pytest deploy/tests contracts/tests -q`
Expected: 全部 PASS

- [ ] **Step 7: Commit**

```bash
git add runtime/internal/feishucmd/ docs/device-test-sequence.md
git commit -m "feat(feishu): surface client fail streak; close gap #10"
```

---

## 完成后的手工验收

1. 跑迁移：`psql "$DATABASE_URL" -f deploy/postgres/migrations/2026-07-29-client-fail-streak.sql`，
   确认 `\d clients` 有 `fail_streak` 列且 `SELECT fail_streak FROM devices` 全为 0；
   并确认 `SELECT count(*) FROM devices WHERE status = 'QUARANTINED'` 为 0
   （迁移会把遗留的 QUARANTINED 一并归位到 IDLE，避免设备在新语义下永久悬挂）。
2. 飞书发 `devices`，确认每行带 `client=… client_fail=…`。
3. 制造一次 client 级失败（停掉 Client Agent 后触发一次 kick），确认：
   `clients.fail_streak` +1、`devices.fail_streak` 仍为 0、设备状态回到 `IDLE` 而非 `QUARANTINED`。
   这一条是差距 #10 的核心验收：改动前同样的操作会把设备推向隔离。
