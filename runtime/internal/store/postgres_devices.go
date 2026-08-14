package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"

	wf "hermes-devops/runtime/internal/workflow"
)

// UpsertClientDevices 处理心跳注册(§8.2):Agent 只可在无 Runtime 租约时
// 切换 IDLE/OFFLINE;BUSY 与 QUARANTINED 由 Runtime 保持。心跳中缺席的
// IDLE 设备置 OFFLINE,避免已拔出的设备继续被调度。
func (s *PGStore) UpsertClientDevices(ctx context.Context, c Client, devs []Device) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("upsert client devices: begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO clients (client_id, host, version, base_url, last_heartbeat)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (client_id) DO UPDATE SET
			host = EXCLUDED.host, version = EXCLUDED.version,
			base_url = EXCLUDED.base_url, last_heartbeat = EXCLUDED.last_heartbeat`,
		c.ClientID, c.Host, c.Version, c.BaseURL); err != nil {
		return fmt.Errorf("upsert client: %w", err)
	}
	for _, d := range devs {
		caps := d.Capabilities
		if caps == nil {
			// JSON 心跳省略 props.capabilities → Go nil slice;pq.Array(nil) 编码为 SQL NULL,
			// 而 devices.capabilities 是 NOT NULL(无特殊能力的板子是正常情况,不得因此整条心跳失败)。
			caps = []string{}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO devices (device_id, serial, display_name, client_id, os, soc, abi, capabilities, mem_total_mb, disk_total_mb, disk_free_mb, status, fail_streak)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 0)
			ON CONFLICT (device_id) DO UPDATE SET
				serial = EXCLUDED.serial, display_name = EXCLUDED.display_name, client_id = EXCLUDED.client_id,
				os = EXCLUDED.os, soc = EXCLUDED.soc, abi = EXCLUDED.abi, capabilities = EXCLUDED.capabilities,
				mem_total_mb = EXCLUDED.mem_total_mb,
				disk_total_mb = EXCLUDED.disk_total_mb, disk_free_mb = EXCLUDED.disk_free_mb,
				status = CASE WHEN devices.status IN ('IDLE', 'OFFLINE') THEN EXCLUDED.status ELSE devices.status END`,
			d.DeviceID, d.Serial, d.DisplayName, d.ClientID, d.OS, d.SOC, d.ABI, pq.Array(caps),
			d.MemTotalMB, d.DiskTotalMB, d.DiskFreeMB, availableState(d.ReportedState)); err != nil {
			return fmt.Errorf("upsert device %s: %w", d.DeviceID, err)
		}
	}
	deviceIDs := make([]string, 0, len(devs))
	for _, d := range devs {
		deviceIDs = append(deviceIDs, d.DeviceID)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE devices SET status = 'OFFLINE'
		WHERE client_id = $1 AND status = 'IDLE' AND NOT (device_id = ANY($2))`,
		c.ClientID, pq.Array(deviceIDs)); err != nil {
		return fmt.Errorf("mark missing devices offline: %w", err)
	}
	return tx.Commit()
}

// AcquireDevice 按 selector 选一台可租设备并租给 taskID(§11 device_leases 独占)。
// 可租 = IDLE,或 BUSY 但租约已过期(持有者失联:workflow 被 Terminate/进程死亡等
// 绕过 ReleaseDevice 的场景,§10 租约 120s 由心跳经 RenewLease 续期,过期即无人
// 认领)——懒回收,无需后台清扫。无可用设备返回 (nil, nil)。
func (s *PGStore) AcquireDevice(ctx context.Context, sel wf.DeviceSelector, taskID string, leaseSeconds int) (*wf.Lease, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("acquire device: begin: %w", err)
	}
	defer tx.Rollback()

	chosen, err := s.lockOneCandidate(ctx, tx, sel)
	if err != nil {
		return nil, err
	}
	if chosen == nil {
		return nil, nil
	}

	if _, err := tx.ExecContext(ctx, `UPDATE devices SET status = 'BUSY' WHERE device_id = $1`,
		chosen.DeviceID); err != nil {
		return nil, fmt.Errorf("acquire device: mark busy: %w", err)
	}
	expiresAt := time.Now().Add(time.Duration(leaseSeconds) * time.Second)
	// 每次授予(含懒回收)生成新 lease_id(见 newLeaseID:task_id 前缀 + 随机后缀)
	// 并递增 generation,旧持有者的续租凭据立即失效(§10/差距 #15);
	// released_at 复位(行保留作审计)。
	leaseID, err := newLeaseID(taskID)
	if err != nil {
		return nil, err
	}
	var generation int
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO device_leases (device_id, task_id, lease_id, lease_generation, lease_expires_at)
		VALUES ($1, $2, $3, 1, $4)
		ON CONFLICT (device_id) DO UPDATE SET
			task_id = EXCLUDED.task_id, lease_id = EXCLUDED.lease_id,
			lease_generation = device_leases.lease_generation + 1,
			lease_expires_at = EXCLUDED.lease_expires_at,
			released_at = NULL
		RETURNING lease_generation`,
		chosen.DeviceID, taskID, leaseID, expiresAt).Scan(&generation); err != nil {
		return nil, fmt.Errorf("acquire device: write lease: %w", err)
	}
	var baseURL string
	if err := tx.QueryRowContext(ctx, `SELECT base_url FROM clients WHERE client_id = $1`,
		chosen.ClientID).Scan(&baseURL); err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("acquire device: lookup client: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("acquire device: commit: %w", err)
	}
	return &wf.Lease{
		DeviceID: chosen.DeviceID, Serial: chosen.Serial,
		DeviceName: chosen.DisplayName,
		ClientID:   chosen.ClientID, ClientBaseURL: baseURL,
		LeaseID: leaseID, Generation: generation,
	}, nil
}

// lockOneCandidate 在 tx 内用 `SELECT ... FOR UPDATE OF d SKIP LOCKED LIMIT 1` 精确锁住至多一行:
// selector 过滤下推到 SQL WHERE、且 LIMIT 1,只锁住将被选中的那一台设备。
// 候选含两类:IDLE 设备,以及 BUSY 但租约已过期的设备(懒回收,见 AcquireDevice);
// 行锁只落在 devices 行上(FOR UPDATE OF d),过期租约行的覆写由后续的
// device_leases UPSERT 在同一 tx 内完成,并发回收者被 SKIP LOCKED 挡在锁外。
// 不这样做的后果:若把过滤留到 Go 侧、或不加 LIMIT,行锁会覆盖所有匹配当前 selector 的
// IDLE 设备(即便最终只取用其中一台),导致另一个并发 Acquire 明明能匹配到其他空闲设备,
// 却被这笔尚未提交的事务无谓阻塞——§11 device_leases 独占的本意是"独占被选中的设备",
// 不是"独占整个候选集合"。
func (s *PGStore) lockOneCandidate(ctx context.Context, tx *sql.Tx, sel wf.DeviceSelector) (*Device, error) {
	socs := make([]string, len(sel.SOC))
	for i, v := range sel.SOC {
		socs[i] = strings.ToLower(v)
	}
	caps := make([]string, len(sel.Capabilities))
	for i, v := range sel.Capabilities {
		caps[i] = strings.ToLower(v)
	}

	var os string
	if sel.OS != "" {
		os = strings.ToLower(sel.OS)
	}

	var d Device
	err := tx.QueryRowContext(ctx, `
		SELECT d.device_id, d.serial, d.display_name, d.client_id, d.soc, d.abi, d.capabilities, d.os
		FROM devices d
		LEFT JOIN device_leases l ON l.device_id = d.device_id
		WHERE (d.status = 'IDLE' OR (d.status = 'BUSY' AND l.lease_expires_at < now()))
		  AND ($1::text = '' OR lower(d.os) = $1)
		  AND (cardinality($2::text[]) = 0 OR lower(d.soc) = ANY($2))
		  AND (cardinality($3::text[]) = 0 OR
		       COALESCE((SELECT array_agg(lower(cap)) FROM unnest(d.capabilities) AS cap), '{}'::text[]) @> $3::text[])
		ORDER BY d.device_id
		LIMIT 1
		FOR UPDATE OF d SKIP LOCKED`,
		os, pq.Array(socs), pq.Array(caps)).Scan(
		&d.DeviceID, &d.Serial, &d.DisplayName, &d.ClientID, &d.SOC, &d.ABI, pq.Array(&d.Capabilities), &d.OS)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("acquire device: select candidate: %w", err)
	}
	return &d, nil
}

// ListFleet 返回全部已注册设备(按 device_id 排序),供 fleet-skip 原因展示。
func (s *PGStore) ListFleet(ctx context.Context) ([]FleetDevice, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT device_id, serial, display_name, os, soc, abi, mem_total_mb, disk_total_mb, disk_free_mb, capabilities, status FROM devices ORDER BY device_id`)
	if err != nil {
		return nil, fmt.Errorf("list fleet: %w", err)
	}
	defer rows.Close()
	out := []FleetDevice{}
	for rows.Next() {
		var d FleetDevice
		if err := rows.Scan(&d.DeviceID, &d.Serial, &d.DisplayName, &d.OS, &d.SOC, &d.ABI, &d.MemTotalMB, &d.DiskTotalMB, &d.DiskFreeMB, pq.Array(&d.Capabilities), &d.Status); err != nil {
			return nil, fmt.Errorf("list fleet: scan: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// HasCapableDevice 报告 fleet 中是否存在满足 sel 的设备(任意状态,含
// OFFLINE/BUSY/QUARANTINED)。语义与 MemStore 一致;设备表小,全量读出后在
// Go 侧复用 matchSelector,保证两种 store 的匹配语义不漂移。
func (s *PGStore) HasCapableDevice(ctx context.Context, sel wf.DeviceSelector) (bool, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT soc, capabilities, os FROM devices`)
	if err != nil {
		return false, fmt.Errorf("has capable device: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.SOC, pq.Array(&d.Capabilities), &d.OS); err != nil {
			return false, fmt.Errorf("has capable device: scan: %w", err)
		}
		if matchSelector(d, sel) {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("has capable device: %w", err)
	}
	return false, nil
}

// RenewLease 条件续期 DB 租约(§10/差距 #15):设备归属 client、lease_id、
// task_id、attempt(与 task_id 后缀一致)、generation 全部精确匹配且
// released_at IS NULL 才续期(影响行数=1);失配返回 false(LEASE_NOT_OWNED),
// 旧持有者不得再续已易主/已释放的租约。
func (s *PGStore) RenewLease(ctx context.Context, cred LeaseCredential, leaseSeconds int) (bool, error) {
	expiresAt := time.Now().Add(time.Duration(leaseSeconds) * time.Second)
	res, err := s.DB.ExecContext(ctx, `
		UPDATE device_leases l SET lease_expires_at = $6
		FROM devices d
		WHERE l.device_id = $1 AND d.device_id = l.device_id AND d.client_id = $2
		  AND l.task_id = $3 AND l.lease_id = $4 AND l.lease_generation = $5
		  AND l.released_at IS NULL`,
		cred.DeviceID, cred.ClientID, cred.TaskID, cred.LeaseID, cred.Generation, expiresAt)
	if err != nil {
		return false, fmt.Errorf("renew lease %s/%s: %w", cred.DeviceID, cred.TaskID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("renew lease %s/%s: rows affected: %w", cred.DeviceID, cred.TaskID, err)
	}
	// attempt 与 task_id 后缀的一致性校验在 SQL 之外(task_id 编码 attempt,差距 #14)
	return n == 1 && attemptMatches(cred.TaskID, cred.Attempt), nil
}

// VerifyLease 见 MemStore 同名方法的语义说明(差距 #8)。纯 SELECT,无副作用。
// device_leases 行只由 AcquireDevice 写入,因此从未被 acquire 过的设备在此表中
// 天然无行(n=0)——这一属性使 PG 侧不需要像 MemStore 那样另外判 status=BUSY。
func (s *PGStore) VerifyLease(ctx context.Context, cred LeaseCredential) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `
		SELECT count(*)
		FROM device_leases l JOIN devices d ON d.device_id = l.device_id
		WHERE l.device_id = $1 AND d.client_id = $2 AND l.task_id = $3
		  AND l.lease_id = $4 AND l.lease_generation = $5
		  AND l.released_at IS NULL`,
		cred.DeviceID, cred.ClientID, cred.TaskID, cred.LeaseID, cred.Generation).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("verify lease %s: %w", cred.TaskID, err)
	}
	// attempt 与 task_id 后缀的一致性校验在 SQL 之外(task_id 编码 attempt,差距 #14),
	// 与 RenewLease 做法一致。
	return n == 1 && attemptMatches(cred.TaskID, cred.Attempt), nil
}

// GetLeaseExpiry 返回 taskID 当前持有租约的到期时刻(CheckLease 活动,
// 原则 6);租约不存在/已释放返回 (nil, nil)——即"未续期"。
func (s *PGStore) GetLeaseExpiry(ctx context.Context, taskID string) (*time.Time, error) {
	var exp time.Time
	err := s.DB.QueryRowContext(ctx, `
		SELECT lease_expires_at FROM device_leases
		WHERE task_id = $1 AND released_at IS NULL`, taskID).Scan(&exp)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get lease expiry %s: %w", taskID, err)
	}
	return &exp, nil
}

// ReleaseDevice 归还租约并按归因记账(差距 #10,设计文档 §4)。语义见 MemStore 同名方法。
// lease 实际释放了才会有下游行,因此重复释放/非持有者释放天然不计数(WHERE 匹配不到行,
// dev/cli CTE 空转,最终 SELECT 零行)。
//
// 置 QUARANTINED 时在**同一事务内**追加写 outbox + audit_log(spec §9.2):"隔离已提交、
// 进程在发通知前崩溃" 是让 activity 靠返回值另发通知的致命缺陷——activity 重试时本函数
// 因 released_at 已非 NULL 而空转,通知会永远发不出去;写进同一事务就没有这个窗口。
func (s *PGStore) ReleaseDevice(ctx context.Context, deviceID, taskID string, scope wf.FailScope, quarantineAfter int) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("release device %s: begin: %w", deviceID, err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		clientID, status, serial, displayName string
		failStreak                            int
	)
	err = tx.QueryRowContext(ctx, `
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
			RETURNING client_id, status, fail_streak, serial, display_name
		), cli AS (
			UPDATE clients SET fail_streak = CASE
				WHEN $3 = 'client' THEN fail_streak + 1
				WHEN $3 = 'ok'     THEN 0
				ELSE fail_streak
			END
			WHERE client_id IN (SELECT client_id FROM dev)
		)
		SELECT client_id, status, fail_streak, serial, display_name FROM dev`,
		deviceID, taskID, string(scope), quarantineAfter).
		Scan(&clientID, &status, &failStreak, &serial, &displayName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // 重复释放/非持有者释放:lease 未匹配,幂等无副作用
	}
	if err != nil {
		return fmt.Errorf("release device %s scope=%s: %w", deviceID, scope, err)
	}

	if status == DeviceQuarantined {
		if err := s.emitQuarantineEventTx(ctx, tx, deviceID, clientID, serial, displayName, taskID, failStreak); err != nil {
			return fmt.Errorf("release device %s: quarantine event: %w", deviceID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("release device %s: commit: %w", deviceID, err)
	}
	return nil
}

// emitQuarantineEventTx 在 tx 内写 outbox(spec §9.2)+ audit_log,与置 QUARANTINED
// 视为同一事务。event_key 用**触发本次隔离的 task_id**,不能用 fail_streak:
// UnquarantineDevice 会把它清零(postgres_fleet.go 的 UnquarantineDevice 注释
// 「status='IDLE'、fail_streak=0」),于是"隔离 → 解除 → 再次隔离"第二次仍在
// streak=3 触发,生成与第一次完全相同的键,第二条 outbox 行被 UNIQUE 挡掉——第二次
// 隔离永远不通知。task_id 不会有这个问题:同一次 Release 的重试 task_id 不变
// (天然幂等),再次隔离必然是另一个 task。
//
// failure_stage 按 task_id 从权威的 results.result_json 读(差距 #2 同款回读)。
// 不改 ReleaseRequest 加字段——那是进 Temporal workflow history 的载荷变更,需要
// 新开一个 workflow.GetVersion 门,成本远高于事务内多读一行。读不到留空,事件照常
// 产生——通知不能因为缺一个展示字段就不发。
func (s *PGStore) emitQuarantineEventTx(
	ctx context.Context, tx *sql.Tx,
	deviceID, clientID, serial, displayName, taskID string, failStreak int,
) error {
	var stage sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT result_json ->> 'failure_stage' FROM results WHERE task_id = $1`, taskID,
	).Scan(&stage); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read failure_stage for %s: %w", taskID, err)
	}
	payload, err := json.Marshal(QuarantineEventPayload{
		DeviceID: deviceID, ClientID: clientID, Serial: serial, DisplayName: displayName,
		FailStreak: failStreak, TaskID: taskID, FailureStage: stage.String,
	})
	if err != nil {
		return fmt.Errorf("marshal quarantine payload: %w", err)
	}
	eventKey := deviceID + ":quarantined:" + taskID
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO outbox (aggregate_type, aggregate_id, event_type, event_key, payload)
		VALUES ('device', $1, $2, $3, $4)
		ON CONFLICT (event_key) DO NOTHING`,
		deviceID, EventTypeDeviceQuarantined, eventKey, payload); err != nil {
		return fmt.Errorf("insert outbox %s: %w", eventKey, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audit_log (actor, action, target) VALUES ($1, $2, $3)`,
		"activity:release_device", "device_quarantined", deviceID); err != nil {
		return fmt.Errorf("insert audit_log for %s: %w", deviceID, err)
	}
	return nil
}

// GetClientVersion reads a client's version from the clients table.
func (s *PGStore) GetClientVersion(ctx context.Context, clientID string) (string, error) {
	var version string
	err := s.DB.QueryRowContext(ctx,
		`SELECT version FROM clients WHERE client_id=$1`, clientID,
	).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("client %s not found", clientID)
	}
	if err != nil {
		return "", fmt.Errorf("get client version %s: %w", clientID, err)
	}
	return version, nil
}

// WriteAudit appends a row to the audit_log.
func (s *PGStore) WriteAudit(ctx context.Context, entry AuditEntry) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO audit_log (actor, action, target, payload_digest) VALUES ($1, $2, $3, $4)`,
		entry.Actor, entry.Action, entry.Target, entry.PayloadDigest,
	)
	return err
}
