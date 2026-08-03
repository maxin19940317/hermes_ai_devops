package store

import (
	"context"
	"strconv"
	"time"

	wf "hermes-devops/runtime/internal/workflow"
)

// TaskEvent 对应 /callbacks/v1/task-events 一条状态迁移(§8.2),
// 按 (task_id, seq) 去重。
type TaskEvent struct {
	TaskID string
	Seq    int
	From   string
	To     string
}

// taskRecord 是 MemStore 内部的任务行:tasks 表字段 + 终态裁决(§11)。
type taskRecord struct {
	row      wf.TaskRow
	verdict  string
	category string
	reason   string
	seq      int64     // 插入序,给"最新一条"提供确定顺序
	endedAt  time.Time // FinishTask 落终态的时刻(UTC)
}

// CreateTask 登记任务;同幂等键(即 task_id)重复创建无副作用(§3 规则 7)。
func (s *MemStore) CreateTask(_ context.Context, row wf.TaskRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[row.TaskID]; ok {
		return nil
	}
	s.seq++
	s.tasks[row.TaskID] = &taskRecord{row: row, seq: s.seq}
	return nil
}

// GetTask 返回任务行副本;不存在返回 (nil, nil)。
func (s *MemStore) GetTask(_ context.Context, taskID string) (*wf.TaskRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.tasks[taskID]
	if !ok {
		return nil, nil
	}
	row := rec.row
	return &row, nil
}

// LatestTaskIDForVariant 返回某次运行中指定变体最新 attempt 的 task_id;
// 无记录返回空串(不报错)。供卡片"忽略"按钮定位裁决落点(decisions.task_id
// 有 FK 指向 tasks,不能写 workflow_id)。
func (s *MemStore) LatestTaskIDForVariant(_ context.Context, workflowID, variant string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	best := ""
	bestAttempt := -1
	var bestSeq int64
	for id, rec := range s.tasks {
		if rec.row.WorkflowID != workflowID || rec.row.TestID != variant {
			continue
		}
		if rec.row.Attempt > bestAttempt || (rec.row.Attempt == bestAttempt && rec.seq > bestSeq) {
			best, bestAttempt, bestSeq = id, rec.row.Attempt, rec.seq
		}
	}
	return best, nil
}

// SetTaskStatus 更新生命周期状态(status 与 verdict 正交,§9)。
func (s *MemStore) SetTaskStatus(_ context.Context, taskID, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.tasks[taskID]; ok {
		rec.row.Status = status
	}
	return nil
}

// FinishTask 落终态 status 与裁决(verdict/category/reason)。
func (s *MemStore) FinishTask(_ context.Context, req wf.FinishRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.tasks[req.TaskID]
	if !ok {
		return nil
	}
	rec.row.Status = req.Status
	rec.verdict, rec.category, rec.reason = req.Verdict, req.Category, req.Reason
	rec.endedAt = time.Now().UTC()
	return nil
}

// AppendTaskEvent 追加状态迁移事件;重复 (task_id, seq) 去重(回调可能重发,§8.2)。
// 返回是否实际插入。
func (s *MemStore) AppendTaskEvent(_ context.Context, ev TaskEvent) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := ev.TaskID + "|" + strconv.Itoa(ev.Seq)
	if _, ok := s.events[key]; ok {
		return false, nil
	}
	s.events[key] = ev
	return true, nil
}

// SaveResult 落 results 表;同 task_id 重复回传去重(§8.2)。返回是否实际插入。
func (s *MemStore) SaveResult(_ context.Context, rec wf.ResultRecord) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.results[rec.TaskID]; ok {
		return false, nil
	}
	s.results[rec.TaskID] = rec
	return true, nil
}

// GetResult 按 task_id 读权威结果(LoadResult 活动,差距清单 #2);
// 不存在返回 (nil, nil)。
func (s *MemStore) GetResult(_ context.Context, taskID string) (*wf.ResultRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.results[taskID]
	if !ok {
		return nil, nil
	}
	out := rec
	return &out, nil
}

// ConclusiveVerdicts 是"结论性"终态集合(§9):测试真实跑完且有确定判定。
// INFRA_ERROR/TIMEOUT(基础设施问题,测试未必真实执行)与无记录 = 非结论性。
var ConclusiveVerdicts = map[string]bool{"PASSED": true, "TEST_FAILED": true}

// ConclusiveWorkflowIDs 返回候选 workflow ID 中已存在结论性测试结果的子集:
// 该 workflow 下有 status='COMPLETED' 且 verdict ∈ ConclusiveVerdicts 的 task。
// 用于 bundle webhook 跳过已测变体(kick 变体级链路已测过的不再重测)。
func (s *MemStore) ConclusiveWorkflowIDs(_ context.Context, workflowIDs []string) (map[string]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := make(map[string]bool, len(workflowIDs))
	for _, id := range workflowIDs {
		want[id] = false
	}
	out := map[string]bool{}
	for _, rec := range s.tasks {
		id := rec.row.WorkflowID
		if _, ok := want[id]; ok && rec.row.Status == "COMPLETED" && ConclusiveVerdicts[rec.verdict] {
			out[id] = true
		}
	}
	return out, nil
}
