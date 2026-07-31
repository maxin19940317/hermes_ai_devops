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

// ArtifactStore 登记产物;实现必须幂等(同一 (project,commit,pipeline,variant)
// 重复登记无效果)。NextWorkflowAttempt 供显式 retry 派生 -r{N} 序号(差距 #11);
// ConclusiveWorkflowIDs 供 bundle webhook 跳过已测变体。
type ArtifactStore interface {
	RegisterArtifacts(ctx context.Context, arts []Artifact) error
	NextWorkflowAttempt(ctx context.Context, project, commitSHA string, pipelineID int, variant string) (int, error)
	ConclusiveWorkflowIDs(ctx context.Context, workflowIDs []string) (map[string]bool, error)
}

// MemStore 是进程内实现,供单测与无数据库的开发模式使用。
type MemStore struct {
	mu           sync.Mutex
	rows         map[string]Artifact
	clients      map[string]Client
	devices      map[string]*deviceRow
	tasks        map[string]*taskRecord
	events       map[string]TaskEvent
	results      map[string]wf.ResultRecord
	decisions    []wf.DecisionRow
	outbox       []*outboxRow
	outboxByKey  map[string]*outboxRow
	outboxByID   map[int64]*outboxRow
	outboxSeq    int64
	workflowRuns map[string]WorkflowRun
	runSeq       map[string]int64
	// evidenceSnaps 是 evidence_snapshots 表(差距 #6)的内存视图。
	evidenceSnaps map[string]EvidenceSnapshot
	// translations 是 command_translations 表(设计文档 §4.3)的内存视图。
	translations []CommandTranslation
	// clientFailStreak 是 clients.fail_streak 的内存视图(差距 #10)。
	// 不放进 Client 结构体:那是心跳载荷,UpsertClientDevices 整体覆写,
	// 放进去会被每 10s 一次的心跳清零。
	clientFailStreak map[string]int
	// inbox 是 card_action_inbox 表(设计文档 §3.1)的内存视图。
	inbox map[string]InboxRow
	// auditLog 是 audit_log 表(设计文档 §3.5)的内存视图。
	auditLog []AuditRow
	// cardActions/cardActionMessages 分别是设计文档 §3.2/§3.3 的内存视图。
	cardActions        map[string]CardAction
	cardActionMessages map[string]cardActionMessage
	// seq 是插入序计数器,给 artifacts/tasks 提供确定的"最近"排序
	// (内存实现无 created_at 列)。
	seq    int64
	rowSeq map[string]int64 // artifacts key → 插入序
}

func NewMemStore() *MemStore {
	return &MemStore{
		rows:               map[string]Artifact{},
		clients:            map[string]Client{},
		devices:            map[string]*deviceRow{},
		tasks:              map[string]*taskRecord{},
		events:             map[string]TaskEvent{},
		results:            map[string]wf.ResultRecord{},
		outboxByKey:        map[string]*outboxRow{},
		outboxByID:         map[int64]*outboxRow{},
		workflowRuns:       map[string]WorkflowRun{},
		runSeq:             map[string]int64{},
		evidenceSnaps:      map[string]EvidenceSnapshot{},
		rowSeq:             map[string]int64{},
		clientFailStreak:   map[string]int{},
		inbox:              map[string]InboxRow{},
		cardActions:        map[string]CardAction{},
		cardActionMessages: map[string]cardActionMessage{},
	}
}

func (s *MemStore) RegisterArtifacts(_ context.Context, arts []Artifact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range arts {
		key := artifactKey(a.Project, a.CommitSHA, a.PipelineID, a.Variant)
		if _, exists := s.rows[key]; !exists {
			s.rows[key] = a
			s.seq++
			s.rowSeq[key] = s.seq
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

func artifactKey(project, commit string, pipelineID int, variant string) string {
	return project + "|" + commit + "|" + strconv.Itoa(pipelineID) + "|" + variant
}

// NextWorkflowAttempt 原子递增指定 project 的 workflow_attempt。
func (s *MemStore) NextWorkflowAttempt(
	_ context.Context, project, commitSHA string, pipelineID int, variant string,
) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := artifactKey(project, commitSHA, pipelineID, variant)
	matched, found := s.rows[key]
	if !found {
		return 0, fmt.Errorf("next workflow attempt: artifact not registered: %s/%s/%d/%s",
			project, commitSHA, pipelineID, variant)
	}
	matched.WorkflowAttempt++
	s.rows[key] = matched
	return matched.WorkflowAttempt, nil
}
