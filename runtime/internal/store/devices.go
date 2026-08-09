package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	wf "hermes-devops/runtime/internal/workflow"
)

// leaseIDEntropyBytes 是 lease_id 后缀的随机字节数(差距 #8 final-review)。
const leaseIDEntropyBytes = 16

// newLeaseID 生成租约所有权凭据(§10/差距 #15)。
//
// 形态是 {task_id}:{32 位十六进制随机}。前半段保留 task_id 便于人工排查与日志关联;
// 后半段是真正的秘密材料——**这一点是必需的,不是加固**:凭据的用途是给
// callbacks 的 upload-requests 端点(差距 #8)做鉴权,而该端点签发的是往证据桶
// 写入的预签名 URL,callbacks 整体又没有其他鉴权(mTLS 属 Phase 3)。
// 若 lease_id 仍等于 task_id(2026-07-29 之前的实现),那么凭据的全部成分
// ——task_id、client_id、device_id(= serial)、attempt(编码在 task_id 里)、
// lease_generation(每设备小计数)——都是可猜的,同网段主机试几次就能换到写入 URL。
//
// 对所有消费方都不透明:Agent 原样回传,两套 store 只做相等比较,故加随机不影响任何调用方。
func newLeaseID(taskID string) (string, error) {
	b := make([]byte, leaseIDEntropyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("new lease id for %s: %w", taskID, err)
	}
	return taskID + ":" + hex.EncodeToString(b), nil
}

// Client 对应 clients 表一行(§11)。
type Client struct {
	ClientID string
	Host     string
	Version  string
	BaseURL  string // 派单地址(§8.1),来源于心跳注册
}

// Device 对应 devices 表一行(§11)。
type Device struct {
	DeviceID      string
	Serial        string
	DisplayName   string
	ClientID      string
	ReportedState string
	OS            string // Phase 4: android / linux; 空字符串兼容历史
	SOC           string
	ABI           string
	Capabilities  []string
	// MemTotalMB 是设备物理内存总量(MB,Agent 从 /proc/meminfo 探测上报;
	// 2026-08-07 起)。指针:旧 Agent/探测失败 → nil;展示信息,非调度必要条件。
	MemTotalMB *int64
}

// 设备状态(§11):IDLE|BUSY|OFFLINE|QUARANTINED。
const (
	DeviceIdle        = "IDLE"
	DeviceBusy        = "BUSY"
	DeviceOffline     = "OFFLINE"
	DeviceQuarantined = "QUARANTINED"
)

// deviceRow 是 MemStore 内部的设备运行时状态(props + status + fail_streak + 租约)。
// 租约所有权凭据(§10/差距 #15):LeaseID 每次授予唯一(取 task_id),
// LeaseGeneration 每设备单调递增,Released 标记 ReleaseDevice 已置 released_at
// (行保留作审计,续租必须 Released=false)。
type deviceRow struct {
	Device
	Status          string
	FailStreak      int
	LeaseTaskID     string
	LeaseID         string
	LeaseGeneration int
	LeaseExpiresAt  time.Time
	Released        bool
}

// LeaseCredential 是心跳续租携带的租约所有权凭据(§10/差距 #15):
// 全部字段精确匹配且租约未释放,才允许续期;任一失配即 LEASE_NOT_OWNED。
type LeaseCredential struct {
	DeviceID   string
	ClientID   string // 心跳信封的 client_id,必须与设备当前归属一致
	TaskID     string
	Attempt    int // 与 task_id 后缀 :a{N} 一致性校验(task_id 编码 attempt,差距 #14)
	LeaseID    string
	Generation int
}

// attemptMatches 校验 cred.Attempt 与 task_id 的 :a{N} 后缀一致
// (task_id = {workflow_id}:{test_id}:a{attempt})。
func attemptMatches(taskID string, attempt int) bool {
	i := strings.LastIndex(taskID, ":a")
	if i < 0 {
		return false
	}
	n, err := strconv.Atoi(taskID[i+2:])
	return err == nil && n == attempt
}

// UpsertClientDevices 处理心跳注册(§8.2):Agent 只可在无 Runtime 租约时
// 切换 IDLE/OFFLINE;BUSY 与 QUARANTINED 由 Runtime 保持。心跳中缺席的
// IDLE 设备置 OFFLINE,避免已拔出的设备继续被调度。
func (s *MemStore) UpsertClientDevices(_ context.Context, c Client, devs []Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[c.ClientID] = c
	present := make(map[string]struct{}, len(devs))
	for _, d := range devs {
		present[d.DeviceID] = struct{}{}
		if row, ok := s.devices[d.DeviceID]; ok {
			row.Device = d
			if row.Status == DeviceIdle || row.Status == DeviceOffline {
				row.Status = availableState(d.ReportedState)
			}
			continue
		}
		s.devices[d.DeviceID] = &deviceRow{Device: d, Status: availableState(d.ReportedState)}
	}
	for deviceID, row := range s.devices {
		if row.ClientID == c.ClientID && row.Status == DeviceIdle {
			if _, ok := present[deviceID]; !ok {
				row.Status = DeviceOffline
			}
		}
	}
	return nil
}

func availableState(reported string) string {
	if reported == "" || reported == DeviceIdle {
		return DeviceIdle
	}
	return DeviceOffline
}

// AcquireDevice 按 selector 选一台可租设备并租给 taskID(§11 device_leases 独占)。
// 可租 = IDLE,或 BUSY 但租约已过期(持有者失联:workflow 被 Terminate/进程死亡等
// 绕过 ReleaseDevice 的场景,§10 租约 120s 由心跳续期,过期即无人认领)——
// 懒回收,无需后台清扫。无可用设备返回 (nil, nil),由 workflow 决定等待或放弃。
func (s *MemStore) AcquireDevice(_ context.Context, sel wf.DeviceSelector, taskID string, leaseSeconds int) (*wf.Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range s.devices {
		if !leasable(row, time.Now()) || !matchSelector(row.Device, sel) {
			continue
		}
		row.Status = DeviceBusy
		row.LeaseTaskID = taskID
		leaseID, err := newLeaseID(taskID)
		if err != nil {
			return nil, err
		}
		row.LeaseID = leaseID
		row.LeaseGeneration++
		row.Released = false
		row.LeaseExpiresAt = time.Now().Add(time.Duration(leaseSeconds) * time.Second)
		return &wf.Lease{
			DeviceID:      row.DeviceID,
			Serial:        row.Serial,
			ClientID:      row.ClientID,
			ClientBaseURL: s.clients[row.ClientID].BaseURL,
			LeaseID:       row.LeaseID,
			Generation:    row.LeaseGeneration,
		}, nil
	}
	return nil, nil
}

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
// 置 QUARANTINED 时在同一临界区(等价于 PGStore 的同一事务)内经 emitQuarantineEvent
// 写 outbox + audit_log(spec §9.2):"隔离已提交、进程在发通知前崩溃" 是让 activity
// 靠返回值另发通知的致命缺陷——本方法幂等早返回时无法再补发,写进临界区内就没有这个窗口。
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
			s.emitQuarantineEvent(row, taskID)
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

// emitQuarantineEvent 写 outbox(spec §9.2)+ audit_log,调用方(ReleaseDevice)已持锁,
// 与置 QUARANTINED 视为同一原子操作。event_key 用**触发本次隔离的 task_id**,
// 不能用 fail_streak:UnquarantineDevice 会把它清零(fleet.go 的
// UnquarantineDevice 注释「status=IDLE、fail_streak=0」),于是"隔离 → 解除 →
// 再次隔离"第二次仍在 streak=3 触发,生成与第一次完全相同的键,第二条 outbox 行
// 会被幂等挡掉——第二次隔离永远不通知。task_id 不会有这个问题:同一次 Release
// 的重试 task_id 不变(天然幂等),再次隔离必然是另一个 task。
//
// failure_stage 按 task_id 从已持久化的 results 读(权威读,差距 #2 同款);
// 读不到留空,事件照常产生——通知不能因为缺一个展示字段就不发。
func (s *MemStore) emitQuarantineEvent(row *deviceRow, taskID string) {
	stage := ""
	if rec, ok := s.results[taskID]; ok {
		stage = rec.Result.FailureStage
	}
	payload, err := json.Marshal(QuarantineEventPayload{
		DeviceID: row.DeviceID, ClientID: row.ClientID, Serial: row.Serial,
		DisplayName: row.DisplayName, FailStreak: row.FailStreak,
		TaskID: taskID, FailureStage: stage,
	})
	if err != nil {
		// QuarantineEventPayload 全是字符串/int 字段,理论上不会序列化失败;
		// 即便失败也不能让隔离本身失败,只跳过本次事件与审计。
		return
	}
	eventKey := row.DeviceID + ":quarantined:" + taskID
	if _, exists := s.outboxByKey[eventKey]; !exists {
		s.outboxSeq++
		ev := &outboxRow{
			ev: OutboxEvent{
				ID: s.outboxSeq, AggregateType: "device", AggregateID: row.DeviceID,
				EventType: EventTypeDeviceQuarantined, EventKey: eventKey, Payload: payload,
			},
			createdAt: time.Now().UTC(),
		}
		s.outbox = append(s.outbox, ev)
		s.outboxByKey[eventKey] = ev
		s.outboxByID[ev.ev.ID] = ev
	}
	s.auditLog = append(s.auditLog, AuditEntry{
		Actor: "activity:release_device", Action: "device_quarantined", Target: row.DeviceID,
	})
}

// leasable 判定设备当前是否可出租:IDLE 可租;BUSY 仅当租约已过期
// (lease_expires_at 由心跳经 RenewLease 续期,过期 = 持有者失联)可懒回收;
// QUARANTINED 永不可租(只能由人工/后续机制解除)。
func leasable(row *deviceRow, now time.Time) bool {
	switch row.Status {
	case DeviceIdle:
		return true
	case DeviceBusy:
		return now.After(row.LeaseExpiresAt)
	default:
		return false
	}
}

// RenewLease 条件续期 DB 租约(§10/差距 #15):设备归属 client、lease_id、
// task_id、attempt(与 task_id 后缀一致)、generation 全部精确匹配且租约未释放,
// 才允许续期并返回 true;任一失配(租约已易主/已释放/凭据伪造或过期)返回
// false——调用方据此向 Client 报告 LEASE_NOT_OWNED,Client 必须停止操作该任务。
func (s *MemStore) RenewLease(_ context.Context, cred LeaseCredential, leaseSeconds int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.devices[cred.DeviceID]
	if !ok || row.Status != DeviceBusy || row.Released {
		return false, nil
	}
	if row.ClientID != cred.ClientID || row.LeaseTaskID != cred.TaskID ||
		row.LeaseID != cred.LeaseID || row.LeaseGeneration != cred.Generation ||
		!attemptMatches(cred.TaskID, cred.Attempt) {
		return false, nil
	}
	row.LeaseExpiresAt = time.Now().Add(time.Duration(leaseSeconds) * time.Second)
	return true, nil
}

// VerifyLease 只读校验凭据是否为该任务当前的租约持有者(差距 #8 的签发端点鉴权)。
// 与 RenewLease 的区别:**不续期**——签发一次 URL 不构成"任务仍然活着"的证据,
// 续期只能由心跳做。校验项与 RenewLease 完全一致(status=BUSY 即"确有一个活跃
// 租约"/device/client/task/attempt/lease_id/generation 全匹配且未释放),失配
// 返回 (false, nil) 而非错误。
//
// 注:UpsertClientDevices 为每台新心跳上来、从未 AcquireDevice 过的设备写入
// Status=DeviceIdle 的零值行(LeaseTaskID/LeaseID/Generation 均为零值)。若省略
// Status==DeviceBusy 这一条,零值凭据(TaskID:"", LeaseID:"", Generation:0)会
// 与该零值行"匹配"而被判真——必须要求设备当前确实处于 BUSY(即存在一个被
// AcquireDevice 授予过的活跃租约),零值行天然不满足。
func (s *MemStore) VerifyLease(_ context.Context, cred LeaseCredential) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.devices[cred.DeviceID]
	if !ok || row.Status != DeviceBusy || row.Released {
		return false, nil
	}
	if row.ClientID != cred.ClientID || row.LeaseTaskID != cred.TaskID ||
		row.LeaseID != cred.LeaseID || row.LeaseGeneration != cred.Generation ||
		!attemptMatches(cred.TaskID, cred.Attempt) {
		return false, nil
	}
	return true, nil
}

// GetLeaseExpiry 返回 taskID 当前持有租约的到期时刻(CheckLease 活动,
// 原则 6);租约不存在/已释放/已易主返回 (nil, nil)——即"未续期"。
func (s *MemStore) GetLeaseExpiry(_ context.Context, taskID string) (*time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range s.devices {
		if row.LeaseTaskID == taskID && !row.Released {
			exp := row.LeaseExpiresAt
			return &exp, nil
		}
	}
	return nil, nil
}

// SelectorMismatch 返回设备不满足 selector 的约束项(人类可读,如
// "os=android"、"缺 hexagon"),供 fleet-skip 原因展示;空切片 = 完全匹配。
// 匹配语义与 matchSelector 同源(唯一事实,防漂移)。
func SelectorMismatch(d Device, sel wf.DeviceSelector) []string {
	var misses []string
	if sel.OS != "" && !strings.EqualFold(sel.OS, d.OS) {
		misses = append(misses, "os="+d.OS)
	}
	if len(sel.SOC) > 0 {
		hit := false
		for _, soc := range sel.SOC {
			if strings.EqualFold(soc, d.SOC) {
				hit = true
				break
			}
		}
		if !hit {
			misses = append(misses, "soc="+d.SOC)
		}
	}
	for _, want := range sel.Capabilities {
		has := false
		for _, have := range d.Capabilities {
			if strings.EqualFold(want, have) {
				has = true
				break
			}
		}
		if !has {
			misses = append(misses, "缺 "+want)
		}
	}
	return misses
}

// FleetDevice 是 ListFleet 的返回项:设备属性 + 当前状态
// (供 fleet-skip 原因区分在线/离线/隔离)。
type FleetDevice struct {
	Device
	Status string
}

// matchSelector:OS 非空时大小写不敏感精确匹配;Soc 命中列表任一项;
// Capabilities 须为设备能力子集。空字段不设限。
func matchSelector(d Device, sel wf.DeviceSelector) bool {
	return len(SelectorMismatch(d, sel)) == 0
}

// ListFleet 返回全部已注册设备(按 device_id 排序,输出确定性),
// 供 fleet-skip 原因展示(差异说明 + 在线/离线区分)。
func (s *MemStore) ListFleet(_ context.Context) ([]FleetDevice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]FleetDevice, 0, len(s.devices))
	for _, row := range s.devices {
		out = append(out, FleetDevice{Device: row.Device, Status: row.Status})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeviceID < out[j].DeviceID })
	return out, nil
}

// HasCapableDevice 报告 fleet 中是否存在满足 sel 的设备(任意状态,含
// OFFLINE/BUSY/QUARANTINED)。语义边界:"设备在但暂不可用"由 acquire 的
// 有限等待处理;"fleet 从无匹配设备"才值得跳过,避免每变体白等
// DeviceWaitRounds×DeviceWaitSeconds(§12 变体级触发后此等待会被放大)。
func (s *MemStore) HasCapableDevice(_ context.Context, sel wf.DeviceSelector) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range s.devices {
		if matchSelector(row.Device, sel) {
			return true, nil
		}
	}
	return false, nil
}

// GetClientVersion reads a client's version from the clients table.
func (s *MemStore) GetClientVersion(_ context.Context, clientID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients[clientID]
	if !ok {
		return "", fmt.Errorf("client %s not found", clientID)
	}
	return c.Version, nil
}

// AuditEntry is a single audit_log row (§11 Phase 3).
type AuditEntry struct {
	Actor         string // activity:dispatch / activity:acquire_device / ...
	Action        string // dispatched / device_leased / device_released / escalated / device_quarantined
	Target        string // task_id / device_id
	PayloadDigest string // sha256 of the marshal of the operation payload (empty = not applicable)
}

// WriteAudit appends a row to the audit_log (fire-and-forget, never blocks).
func (s *MemStore) WriteAudit(_ context.Context, entry AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auditLog = append(s.auditLog, entry)
	return nil
}
