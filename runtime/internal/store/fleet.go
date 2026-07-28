package store

import (
	"context"
	"fmt"
	"strconv"
)

// DeviceStatus 是设备运行时状态视图(飞书指令 status/devices 查询用)。
type DeviceStatus struct {
	DeviceID    string
	Serial      string
	SOC         string
	Status      string // IDLE|BUSY|OFFLINE|QUARANTINED
	FailStreak  int
	LeaseTaskID string // 活跃租约持有任务;空 = 无租约
}

// FleetOverview 是 status 指令的汇总视图。
type FleetOverview struct {
	InflightWorkflows int // 有非终态 task 的 workflow 数
	ActiveLeases      int // released_at IS NULL 的租约数
	Devices           []DeviceStatus
}

// 终态集合(§9):非终态 task 所在 workflow 视为"运行中"。
var terminalStatus = map[string]bool{
	"COMPLETED": true, "FAILED": true, "TIMEOUT": true, "CANCELED": true,
}

// FleetOverview 汇总 fleet 状态(只读)。
func (s *MemStore) FleetOverview(_ context.Context) (*FleetOverview, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &FleetOverview{Devices: []DeviceStatus{}}
	running := map[string]bool{}
	for _, rec := range s.tasks {
		if !terminalStatus[rec.row.Status] {
			running[rec.row.WorkflowID] = true
		}
	}
	out.InflightWorkflows = len(running)
	for _, row := range s.devices {
		leaseTask := ""
		if !row.Released {
			leaseTask = row.LeaseTaskID
		}
		if leaseTask != "" {
			out.ActiveLeases++
		}
		out.Devices = append(out.Devices, DeviceStatus{
			DeviceID: row.DeviceID, Serial: row.Serial, SOC: row.SOC,
			Status: row.Status, FailStreak: row.FailStreak, LeaseTaskID: leaseTask,
		})
	}
	return out, nil
}

// UnquarantineDevice 解隔离:status=IDLE、fail_streak=0(飞书指令 unquarantine)。
// 设备不存在返回 (false, nil);重复解隔离幂等。
func (s *MemStore) UnquarantineDevice(_ context.Context, deviceID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.devices[deviceID]
	if !ok {
		return false, nil
	}
	row.Status = DeviceIdle
	row.FailStreak = 0
	return true, nil
}

// ListArtifacts 返回 (commit,pipeline) 逻辑键下的全部产物行(飞书指令 rerun
// 重建 DeviceTestInput 用);无记录返回空切片。
func (s *MemStore) ListArtifacts(_ context.Context, commitSHA string, pipelineID int) ([]Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Artifact{}
	prefix := commitSHA + "|" + strconv.Itoa(pipelineID) + "|"
	for k, a := range s.rows {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, a)
		}
	}
	return out, nil
}

// NextWorkflowAttemptAll 把 (commit,pipeline) 键下全部产物行的 workflow_attempt
// 原子 +1,返回新的最大值(bundle 级显式 rerun 的 -r{N} 后缀来源;
// 各变体行可能被 kick retry 单独递增过,取 max 保证 ID 唯一)。
// 键下无记录返回错误。
func (s *MemStore) NextWorkflowAttemptAll(_ context.Context, commitSHA string, pipelineID int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := commitSHA + "|" + strconv.Itoa(pipelineID) + "|"
	maxN := 0
	for k, a := range s.rows {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			a.WorkflowAttempt++
			s.rows[k] = a
			if a.WorkflowAttempt > maxN {
				maxN = a.WorkflowAttempt
			}
		}
	}
	if maxN == 0 {
		return 0, fmt.Errorf("next workflow attempt: artifact not registered: %s/%d", commitSHA, pipelineID)
	}
	return maxN, nil
}
