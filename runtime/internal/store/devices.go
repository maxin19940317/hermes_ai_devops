package store

import (
	"context"
	"strings"
	"time"

	wf "hermes-devops/runtime/internal/workflow"
)

// Client 对应 clients 表一行(§11)。
type Client struct {
	ClientID string
	Host     string
	Version  string
	BaseURL  string // 派单地址(§8.1),来源于心跳注册
}

// Device 对应 devices 表一行(§11)。
type Device struct {
	DeviceID     string
	Serial       string
	ClientID     string
	SOC          string
	ABI          string
	Capabilities []string
}

// 设备状态(§11):IDLE|BUSY|OFFLINE|QUARANTINED。
const (
	DeviceIdle        = "IDLE"
	DeviceBusy        = "BUSY"
	DeviceQuarantined = "QUARANTINED"
)

// deviceRow 是 MemStore 内部的设备运行时状态(props + status + fail_streak + 租约)。
type deviceRow struct {
	Device
	Status         string
	FailStreak     int
	LeaseTaskID    string
	LeaseExpiresAt time.Time
}

// UpsertClientDevices 处理心跳注册(§8.2):新设备以 IDLE 入库,
// 已有设备只刷新属性,不触碰 status/fail_streak(心跳不得解除隔离)。
func (s *MemStore) UpsertClientDevices(_ context.Context, c Client, devs []Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[c.ClientID] = c
	for _, d := range devs {
		if row, ok := s.devices[d.DeviceID]; ok {
			row.Device = d
			continue
		}
		s.devices[d.DeviceID] = &deviceRow{Device: d, Status: DeviceIdle}
	}
	return nil
}

// AcquireDevice 按 selector 选一台 IDLE 设备并租给 taskID(§11 device_leases 独占)。
// 无可用设备返回 (nil, nil),由 workflow 决定等待或放弃。
func (s *MemStore) AcquireDevice(_ context.Context, sel wf.DeviceSelector, taskID string, leaseSeconds int) (*wf.Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range s.devices {
		if row.Status != DeviceIdle || !matchSelector(row.Device, sel) {
			continue
		}
		row.Status = DeviceBusy
		row.LeaseTaskID = taskID
		row.LeaseExpiresAt = time.Now().Add(time.Duration(leaseSeconds) * time.Second)
		return &wf.Lease{
			DeviceID:      row.DeviceID,
			Serial:        row.Serial,
			ClientID:      row.ClientID,
			ClientBaseURL: s.clients[row.ClientID].BaseURL,
		}, nil
	}
	return nil, nil
}

// ReleaseDevice 归还租约。infraFail=true 时 fail_streak+1,
// 达到 quarantineAfter(§10 缺省 3)则 QUARANTINED;成功归还清零 fail_streak。
func (s *MemStore) ReleaseDevice(_ context.Context, deviceID, taskID string, infraFail bool, quarantineAfter int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.devices[deviceID]
	if !ok || row.LeaseTaskID != taskID {
		return nil // 重复释放/租约已易主:幂等,无副作用
	}
	row.LeaseTaskID = ""
	row.LeaseExpiresAt = time.Time{}
	if infraFail {
		row.FailStreak++
		if row.FailStreak >= quarantineAfter {
			row.Status = DeviceQuarantined
			return nil
		}
	} else {
		row.FailStreak = 0
	}
	row.Status = DeviceIdle
	return nil
}

// matchSelector:SOC 大小写不敏感命中列表任一项;Capabilities 须为设备能力子集。
// 空列表不设限。
func matchSelector(d Device, sel wf.DeviceSelector) bool {
	if len(sel.SOC) > 0 {
		hit := false
		for _, soc := range sel.SOC {
			if strings.EqualFold(soc, d.SOC) {
				hit = true
				break
			}
		}
		if !hit {
			return false
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
			return false
		}
	}
	return true
}
