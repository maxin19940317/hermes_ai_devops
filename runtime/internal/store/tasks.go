package store

import (
	"context"
	"strconv"

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
}

// CreateTask 登记任务;同幂等键(即 task_id)重复创建无副作用(§3 规则 7)。
func (s *MemStore) CreateTask(_ context.Context, row wf.TaskRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[row.TaskID]; ok {
		return nil
	}
	s.tasks[row.TaskID] = &taskRecord{row: row}
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
