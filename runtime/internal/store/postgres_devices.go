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

// AcquireDevice 按 selector 选一台 IDLE 设备并租给 taskID(§11 device_leases 独占)。
// 无可用设备返回 (nil, nil)。
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
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO device_leases (device_id, task_id, lease_expires_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (device_id) DO UPDATE SET
			task_id = EXCLUDED.task_id, lease_expires_at = EXCLUDED.lease_expires_at`,
		chosen.DeviceID, taskID, expiresAt); err != nil {
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
	}, nil
}

// lockOneCandidate 在 tx 内用 `SELECT ... FOR UPDATE SKIP LOCKED LIMIT 1` 精确锁住至多一行:
// selector 过滤下推到 SQL WHERE、且 LIMIT 1,只锁住将被选中的那一台设备。
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
		SELECT device_id, serial, client_id, soc, abi, capabilities
		FROM devices
		WHERE status = 'IDLE'
		  AND (cardinality($1::text[]) = 0 OR lower(soc) = ANY($1))
		  AND (cardinality($2::text[]) = 0 OR
		       COALESCE((SELECT array_agg(lower(cap)) FROM unnest(capabilities) AS cap), '{}'::text[]) @> $2::text[])
		ORDER BY device_id
		LIMIT 1
		FOR UPDATE SKIP LOCKED`,
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

// ReleaseDevice 归还租约。infraFail=true 时 fail_streak+1,
// 达到 quarantineAfter(§10 缺省 3)则 QUARANTINED;成功归还清零 fail_streak。
// 非租约持有者释放/租约已易主:幂等,无副作用(WHERE 匹配不到行,语句空转)。
func (s *PGStore) ReleaseDevice(ctx context.Context, deviceID, taskID string, infraFail bool, quarantineAfter int) error {
	_, err := s.DB.ExecContext(ctx, `
		WITH lease AS (
			DELETE FROM device_leases WHERE device_id = $1 AND task_id = $2 RETURNING device_id
		)
		UPDATE devices SET
			status = CASE
				WHEN $3 AND fail_streak + 1 >= $4 THEN 'QUARANTINED'
				ELSE 'IDLE'
			END,
			fail_streak = CASE WHEN $3 THEN fail_streak + 1 ELSE 0 END
		WHERE device_id IN (SELECT device_id FROM lease)`,
		deviceID, taskID, infraFail, quarantineAfter)
	if err != nil {
		return fmt.Errorf("release device: %w", err)
	}
	return nil
}
