// Package store 是 Runtime 的 Postgres 访问层(CLAUDE.md §11)。
// Phase 1.5 只覆盖 artifacts 表;接口化以便 handler 单测用内存实现。
package store

import (
	"context"
	"strconv"
	"sync"
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
}

// ArtifactStore 登记产物;实现必须幂等(同一 (commit,pipeline,variant) 重复登记无效果)。
type ArtifactStore interface {
	RegisterArtifacts(ctx context.Context, arts []Artifact) error
}

// MemStore 是进程内实现,供单测与无数据库的开发模式使用。
type MemStore struct {
	mu   sync.Mutex
	rows map[string]Artifact
}

func NewMemStore() *MemStore {
	return &MemStore{rows: map[string]Artifact{}}
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
