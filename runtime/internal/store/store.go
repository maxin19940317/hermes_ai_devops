// Package store 是 Runtime 的 Postgres 访问层(CLAUDE.md §11)。
// Phase 1.5 只覆盖 artifacts 表;接口化以便 handler 单测用内存实现。
package store

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	wf "hermes-devops/runtime/internal/workflow"
)

// Artifact 对应 artifacts 表一行(§11)。
// CONTRACT-ISSUE: bundle v1 的 packages[] 不携带 build_type,而 artifacts 表有此列;
// Phase 1 全部构建为 Release,由 Trigger 填缺省值。若引入 Debug 构建,
// 需在 meta/bundle 增加 build_type 字段(契约只加不删)。
type Artifact struct {
	Project        string
	CommitSHA      string // short sha(bundle.commit)
	PipelineID     int    // CI_PIPELINE_IID
	Variant        string
	BuildType      string
	URL            string
	SHA256         string
	Size           int64
	ManifestDigest string // 派单时透传给 Client 核对(§8.1)
	// WorkflowAttempt 显式 retry 计数(差距 #11):>0 时 workflow ID 加 -r{N}。
	WorkflowAttempt int
}

// ArtifactStore 登记产物;实现必须幂等(同一 (commit,pipeline,variant) 重复登记无效果)。
// NextWorkflowAttempt 供显式 retry 派生 -r{N} 序号(差距 #11);
// ConclusiveWorkflowIDs 供 bundle webhook 跳过已测变体。
type ArtifactStore interface {
	RegisterArtifacts(ctx context.Context, arts []Artifact) error
	NextWorkflowAttempt(ctx context.Context, commitSHA string, pipelineID int, variant string) (int, error)
	ConclusiveWorkflowIDs(ctx context.Context, workflowIDs []string) (map[string]bool, error)
}

// MemStore 是进程内实现,供单测与无数据库的开发模式使用。
type MemStore struct {
	mu          sync.Mutex
	rows        map[string]Artifact
	clients     map[string]Client
	devices     map[string]*deviceRow
	tasks       map[string]*taskRecord
	events      map[string]TaskEvent
	results     map[string]wf.ResultRecord
	decisions   []wf.DecisionRow
	outbox      []*outboxRow
	outboxByKey map[string]*outboxRow
	outboxByID  map[int64]*outboxRow
	outboxSeq   int64
}

func NewMemStore() *MemStore {
	return &MemStore{
		rows:        map[string]Artifact{},
		clients:     map[string]Client{},
		devices:     map[string]*deviceRow{},
		tasks:       map[string]*taskRecord{},
		events:      map[string]TaskEvent{},
		results:     map[string]wf.ResultRecord{},
		outboxByKey: map[string]*outboxRow{},
		outboxByID:  map[int64]*outboxRow{},
	}
}

func (s *MemStore) RegisterArtifacts(_ context.Context, arts []Artifact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range arts {
		key := a.CommitSHA + "|" + strconv.Itoa(a.PipelineID) + "|" + a.Variant
		if _, exists := s.rows[key]; !exists {
			s.rows[key] = a
		}
	}
	return nil
}

// Artifacts 返回已登记产物(仅测试用)。
func (s *MemStore) Artifacts() []Artifact {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Artifact, 0, len(s.rows))
	for _, a := range s.rows {
		out = append(out, a)
	}
	return out
}

// NextWorkflowAttempt 把 (commit,pipeline,variant) 逻辑键的 workflow_attempt
// 原子 +1 并返回新值(显式 retry 的 -r{N} 后缀来源,差距 #11);
// 键未登记(产物尚未 RegisterArtifacts)返回错误。
func (s *MemStore) NextWorkflowAttempt(_ context.Context, commitSHA string, pipelineID int, variant string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := commitSHA + "|" + strconv.Itoa(pipelineID) + "|" + variant
	a, ok := s.rows[key]
	if !ok {
		return 0, fmt.Errorf("next workflow attempt: artifact not registered: %s", key)
	}
	a.WorkflowAttempt++
	s.rows[key] = a
	return a.WorkflowAttempt, nil
}
