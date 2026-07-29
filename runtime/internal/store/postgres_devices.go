package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"

	wf "hermes-devops/runtime/internal/workflow"
)

// UpsertClientDevices 处理心跳注册(§8.2):新设备以 IDLE 入库,
// 已有设备只刷新属性,不触碰 status/fail_streak(心跳不得解除隔离)。
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
			INSERT INTO devices (device_id, serial, client_id, soc, abi, capabilities, status, fail_streak)
			VALUES ($1, $2, $3, $4, $5, $6, 'IDLE', 0)
			ON CONFLICT (device_id) DO UPDATE SET
				serial = EXCLUDED.serial, client_id = EXCLUDED.client_id,
				soc = EXCLUDED.soc, abi = EXCLUDED.abi, capabilities = EXCLUDED.capabilities`,
			d.DeviceID, d.Serial, d.ClientID, d.SOC, d.ABI, pq.Array(caps)); err != nil {
			return fmt.Errorf("upsert device %s: %w", d.DeviceID, err)
		}
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
	// 每次授予(含懒回收)生成新 lease_id(取 task_id,本身含 attempt,全链路唯一)
	// 并递增 generation,旧持有者的续租凭据立即失效(§10/差距 #15);
	// released_at 复位(行保留作审计)。
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
		chosen.DeviceID, taskID, taskID, expiresAt).Scan(&generation); err != nil {
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
		ClientID: chosen.ClientID, ClientBaseURL: baseURL,
		LeaseID: taskID, Generation: generation,
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

	var d Device
	err := tx.QueryRowContext(ctx, `
		SELECT d.device_id, d.serial, d.client_id, d.soc, d.abi, d.capabilities
		FROM devices d
		LEFT JOIN device_leases l ON l.device_id = d.device_id
		WHERE (d.status = 'IDLE' OR (d.status = 'BUSY' AND l.lease_expires_at < now()))
		  AND (cardinality($1::text[]) = 0 OR lower(d.soc) = ANY($1))
		  AND (cardinality($2::text[]) = 0 OR
		       COALESCE((SELECT array_agg(lower(cap)) FROM unnest(d.capabilities) AS cap), '{}'::text[]) @> $2::text[])
		ORDER BY d.device_id
		LIMIT 1
		FOR UPDATE OF d SKIP LOCKED`,
		pq.Array(socs), pq.Array(caps)).Scan(
		&d.DeviceID, &d.Serial, &d.ClientID, &d.SOC, &d.ABI, pq.Array(&d.Capabilities))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("acquire device: select candidate: %w", err)
	}
	return &d, nil
}

// HasCapableDevice 报告 fleet 中是否存在满足 sel 的设备(任意状态,含
// OFFLINE/BUSY/QUARANTINED)。语义与 MemStore 一致;设备表小,全量读出后在
// Go 侧复用 matchSelector,保证两种 store 的匹配语义不漂移。
func (s *PGStore) HasCapableDevice(ctx context.Context, sel wf.DeviceSelector) (bool, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT soc, capabilities FROM devices`)
	if err != nil {
		return false, fmt.Errorf("has capable device: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.SOC, pq.Array(&d.Capabilities)); err != nil {
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
