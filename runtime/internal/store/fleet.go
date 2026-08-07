package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	wf "hermes-devops/runtime/internal/workflow"
)

// DeviceStatus 是设备运行时状态视图(飞书指令 status/devices 查询用)。
type DeviceStatus struct {
	DeviceID    string
	Serial      string
	DisplayName string
	SOC         string
	Status      string // IDLE|BUSY|OFFLINE|QUARANTINED
	FailStreak  int
	LeaseTaskID string // 活跃租约持有任务;空 = 无租约

	ClientID         string // 归属 client
	ClientFailStreak int    // 该 client 的连续失败计数(差距 #10)
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
			DeviceID: row.DeviceID, Serial: row.Serial, DisplayName: row.DisplayName, SOC: row.SOC,
			Status: row.Status, FailStreak: row.FailStreak, LeaseTaskID: leaseTask,
			ClientID: row.ClientID, ClientFailStreak: s.clientFailStreak[row.ClientID],
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

// QuarantineDevice 手动隔离(飞书指令 quarantine):status=QUARANTINED。
// 设备不存在返回 (false, nil);已隔离幂等返回 true(状态不变)。
// 与 UnquarantineDevice 对称,用于人工把疑似故障设备摘出调度。
func (s *MemStore) QuarantineDevice(_ context.Context, deviceID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.devices[deviceID]
	if !ok {
		return false, nil
	}
	if row.Status == DeviceBusy {
		return false, nil // 运行中设备不隔离(防打断进行中的测试)
	}
	row.Status = DeviceQuarantined
	return true, nil
}

// ListArtifacts 返回指定 project/commit/pipeline 的全部产物。
func (s *MemStore) ListArtifacts(
	_ context.Context, project, commitSHA string, pipelineID int,
) ([]Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Artifact{}
	for _, a := range s.rows {
		if a.Project == project && a.CommitSHA == commitSHA && a.PipelineID == pipelineID {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Variant < out[j].Variant })
	return out, nil
}

// ListArtifactsForVariant 返回指定变体全部产物(按登记序倒序=最近优先,不限
// project)。供飞书指令 artifacts 查询构建历史;MemStore 用 rowSeq 模拟 created_at。
func (s *MemStore) ListArtifactsForVariant(
	_ context.Context, variant string, limit int,
) ([]Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type keyed struct {
		art Artifact
		seq int64
	}
	all := make([]keyed, 0, len(s.rows))
	for _, a := range s.rows {
		if a.Variant == variant {
			all = append(all, keyed{art: a, seq: s.rowSeq[artifactKey(a.Project, a.CommitSHA, a.PipelineID, a.Variant)]})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].seq != all[j].seq {
			return all[i].seq > all[j].seq
		}
		return all[i].art.Project > all[j].art.Project
	})
	if limit <= 0 || limit > len(all) {
		limit = len(all)
	}
	out := make([]Artifact, 0, limit)
	for _, k := range all[:limit] {
		out = append(out, k.art)
	}
	return out, nil
}

// LatestArtifactForVariant 返回指定变体最近登记的产物(按登记序,模拟 created_at
// 最新,不限 project);无记录返回 nil,nil。供 test 命令缺省 commit 时定位
// "最近构建"——project 从 artifact 自身带出。
func (s *MemStore) LatestArtifactForVariant(
	_ context.Context, variant string,
) (*Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *Artifact
	var bestSeq int64 = -1
	for _, a := range s.rows {
		if a.Variant != variant {
			continue
		}
		if seq := s.rowSeq[artifactKey(a.Project, a.CommitSHA, a.PipelineID, a.Variant)]; seq > bestSeq {
			bestSeq = seq
			best = &a
		}
	}
	return best, nil
}

// NextWorkflowAttemptAll 把指定 project/commit/pipeline 的全部变体推进到
// 当前最大值的下一水位，确保 bundle 与变体级 retry 共用单调序号空间。
func (s *MemStore) NextWorkflowAttemptAll(
	_ context.Context, project, commitSHA string, pipelineID int,
) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	maxN := -1
	for _, a := range s.rows {
		if a.Project == project && a.CommitSHA == commitSHA && a.PipelineID == pipelineID {
			if a.WorkflowAttempt > maxN {
				maxN = a.WorkflowAttempt
			}
		}
	}
	if maxN < 0 {
		return 0, fmt.Errorf("next workflow attempt: artifact not registered: %s/%s/%d",
			project, commitSHA, pipelineID)
	}
	target := maxN + 1
	for k, a := range s.rows {
		if a.Project == project && a.CommitSHA == commitSHA && a.PipelineID == pipelineID {
			a.WorkflowAttempt = target
			s.rows[k] = a
		}
	}
	return target, nil
}

// RecentRun 是快照里的一次运行(设计文档 §4.2)。Verdict 为空表示尚无终态结论。
type RecentRun struct {
	WorkflowID    string
	Project       string
	Commit        string
	PipelineID    int
	Version       string
	RuleVersion   string
	Variant       string
	Verdict       string
	EndedAt       time.Time
	Authoritative bool
	// HasTask 该 workflow+变体是否有 task 记录。false + Verdict 空 =
	// bundle 声明但从未调度(kick 未匹配设备),展示为"待调度"而非"运行中"。
	HasTask bool
}

// RecentRuns 优先返回 workflow_runs 的权威运行记录，再补充尚未被 registry
// 覆盖的旧 artifacts。权威记录只与完全相同 workflow_id/test_id 的 task 关联。
func (s *MemStore) RecentRuns(_ context.Context, limit int) ([]RecentRun, error) {
	if limit <= 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	type keyedRun struct {
		run WorkflowRun
		seq int64
	}
	runs := make([]keyedRun, 0, len(s.workflowRuns))
	excluded := make(map[string]struct{})
	for workflowID, run := range s.workflowRuns {
		runs = append(runs, keyedRun{run: run, seq: s.runSeq[workflowID]})
		for _, variant := range run.Variants {
			excluded[artifactKey(run.Project, run.CommitSHA, run.PipelineID, variant)] = struct{}{}
		}
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].seq != runs[j].seq {
			return runs[i].seq > runs[j].seq
		}
		return runs[i].run.WorkflowID > runs[j].run.WorkflowID
	})

	out := make([]RecentRun, 0, limit)
	for _, keyed := range runs {
		run := keyed.run
		for _, variant := range run.Variants {
			var best *taskRecord
			for _, rec := range s.tasks {
				if rec.row.WorkflowID != run.WorkflowID || rec.row.TestID != variant {
					continue
				}
				if best == nil || rec.row.Attempt > best.row.Attempt ||
					(rec.row.Attempt == best.row.Attempt && rec.seq > best.seq) ||
					(rec.row.Attempt == best.row.Attempt && rec.seq == best.seq &&
						rec.row.TaskID > best.row.TaskID) {
					best = rec
				}
			}
			recent := RecentRun{
				WorkflowID: run.WorkflowID, Project: run.Project, Commit: run.CommitSHA,
				PipelineID: run.PipelineID, Version: run.Version, RuleVersion: run.RuleVersion,
				Variant: variant, Authoritative: true,
			}
			if best != nil {
				recent.Verdict, recent.EndedAt = best.verdict, best.endedAt
				recent.HasTask = true
			}
			out = append(out, recent)
			if len(out) == limit {
				return out, nil
			}
		}
	}

	type keyedArtifact struct {
		art Artifact
		seq int64
	}
	all := make([]keyedArtifact, 0, len(s.rows))
	for k, a := range s.rows {
		if _, ok := excluded[artifactKey(a.Project, a.CommitSHA, a.PipelineID, a.Variant)]; ok {
			continue
		}
		all = append(all, keyedArtifact{art: a, seq: s.rowSeq[k]})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].seq != all[j].seq {
			return all[i].seq > all[j].seq
		}
		return artifactKey(all[i].art.Project, all[i].art.CommitSHA, all[i].art.PipelineID, all[i].art.Variant) >
			artifactKey(all[j].art.Project, all[j].art.CommitSHA, all[j].art.PipelineID, all[j].art.Variant)
	})
	for _, k := range all {
		a := k.art
		run := RecentRun{
			Project: a.Project, Commit: a.CommitSHA,
			PipelineID: a.PipelineID, Variant: a.Variant,
		}
		base := wf.BaseWorkflowID(a.Project, a.CommitSHA, a.PipelineID)
		var best *taskRecord
		for _, rec := range s.tasks {
			if rec.row.TestID != a.Variant {
				continue
			}
			id := rec.row.WorkflowID
			if id != base && !strings.HasPrefix(id, base+"-") {
				continue
			}
			if best == nil || rec.seq > best.seq {
				best = rec
			}
		}
		if best != nil {
			run.Verdict, run.EndedAt = best.verdict, best.endedAt
		}
		out = append(out, run)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}
