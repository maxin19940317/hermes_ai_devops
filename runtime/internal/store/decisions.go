package store

import (
	"context"

	wf "hermes-devops/runtime/internal/workflow"
)

// SaveDecision 落 decisions 表(§11):规则引擎与 LLM 的每次裁决都落表,可回放。
// 与 MemStore 其他写路径一致,不做外键校验(task_id 不存在也放行)。
func (s *MemStore) SaveDecision(_ context.Context, row wf.DecisionRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decisions = append(s.decisions, row)
	return nil
}

// ListDecisions 按插入序返回某任务的全部裁决;无记录返回空切片。
func (s *MemStore) ListDecisions(_ context.Context, taskID string) ([]wf.DecisionRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []wf.DecisionRow{}
	for _, d := range s.decisions {
		if d.TaskID == taskID {
			out = append(out, d)
		}
	}
	return out, nil
}
