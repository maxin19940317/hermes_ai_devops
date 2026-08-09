package store

import (
	"context"
	"encoding/json"
	"time"

	wf "hermes-devops/runtime/internal/workflow"
)

// EventTypeTaskResult 是终态结果事件的 event_type(docs/device-test-sequence.md
// 事件分级:Result 属关键事件,必须走事务性 Outbox)。
const EventTypeTaskResult = "task-result"

// EventTypeDeviceQuarantined 是设备被自动隔离事件的 event_type
// (设计文档 2026-08-09-device-attribution-signal-design.md §9.2)。
// ReleaseDevice 在把设备置 QUARANTINED 的同一事务内写这类事件,由 Relay
// 投递给通知端——隔离已提交后进程崩溃也不会丢通知(at-least-once)。
const EventTypeDeviceQuarantined = "device-quarantined"

// QuarantineEventPayload 是 event_type=device-quarantined 的 outbox payload
// (spec §9.2)。FailureStage 取自触发本次隔离的 task 的已持久化结果
// (results.result_json ->> 'failure_stage');该 task 无 stage 时留空——
// 通知不能因为缺一个展示字段就不发。
type QuarantineEventPayload struct {
	DeviceID     string `json:"device_id"`
	ClientID     string `json:"client_id"`
	Serial       string `json:"serial"`
	DisplayName  string `json:"display_name"`
	FailStreak   int    `json:"fail_streak"`
	TaskID       string `json:"task_id"`
	FailureStage string `json:"failure_stage"`
}

// OutboxEvent 对应 outbox 表一行(docs/device-test-sequence.md 文末表结构)。
// PublishedAt 不暴露给投递方:ClaimUnpublished 只返回未投递行,
// MarkPublished/MarkFailed 也只作用于未投递行。
type OutboxEvent struct {
	ID            int64
	AggregateType string
	AggregateID   string
	EventType     string
	EventKey      string // 幂等键,如 {task_id}:result;重复插入不产生第二行
	Payload       json.RawMessage
	Attempts      int
	LastError     string
}

// ResultEventPayload 是 event_type=task-result 的 outbox payload:
// Relay 据此向 workflow_id 投递 SignalTaskResult(signal 只是唤醒提示,
// 结果本体由 workflow 经 LoadResult 按 task_id 回读 results 表,原则 3 + 差距 #2)。
type ResultEventPayload struct {
	WorkflowID string              `json:"workflow_id"`
	Result     wf.TaskResultSignal `json:"result"`
}

// outboxRow 是 MemStore 内部的 outbox 行:事件本体 + 投递标记 + 入库时刻
// (backlog 报告要算积压时长,PG 侧用 created_at 列)。
type outboxRow struct {
	ev        OutboxEvent
	published bool
	createdAt time.Time
}

// OutboxBacklog 是 outbox 积压快照(第四批:backlog/失败监控)。
// 用途是回答两个运维问题:有没有积压、有没有卡住投不出去的行。
type OutboxBacklog struct {
	Pending     int           // 未投递行数
	Stuck       int           // 未投递且 attempts >= 阈值(重试多次仍不成功)
	OldestAge   time.Duration // 最老未投递行的年龄;无积压时为 0
	OldestID    int64         // 最老未投递行 id;无积压时为 0
	SampleError string        // 尝试次数最多的未投递行的 last_error(诊断入口)
}

// SaveResultWithOutbox 单事务写 results + outbox(原则 3:消灭"写库成功但
// signal 失败"的双写窗口)。两边均幂等:同 task_id 结果去重,同 event_key
// 事件不产生第二行;返回结果行是否实际插入。
func (s *MemStore) SaveResultWithOutbox(_ context.Context, rec wf.ResultRecord, ev OutboxEvent) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inserted := false
	if _, ok := s.results[rec.TaskID]; !ok {
		s.results[rec.TaskID] = rec
		inserted = true
	}
	if _, ok := s.outboxByKey[ev.EventKey]; !ok {
		s.outboxSeq++
		row := &outboxRow{ev: ev, createdAt: time.Now().UTC()}
		row.ev.ID = s.outboxSeq
		s.outbox = append(s.outbox, row)
		s.outboxByKey[ev.EventKey] = row
		s.outboxByID[row.ev.ID] = row
	}
	return inserted, nil
}

// ClaimUnpublished 按 id 序返回未投递事件(至多 limit 条)。
// Relay 当前单实例运行,不加行锁;多实例化时需改 FOR UPDATE SKIP LOCKED。
func (s *MemStore) ClaimUnpublished(_ context.Context, limit int) ([]OutboxEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []OutboxEvent{}
	for _, row := range s.outbox {
		if row.published {
			continue
		}
		out = append(out, row.ev)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// MarkPublished 标记投递成功;只作用于未投递行,重复标记幂等。
func (s *MemStore) MarkPublished(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if row, ok := s.outboxByID[id]; ok {
		row.published = true
	}
	return nil
}

// MarkFailed 记录投递失败(attempts+1 + last_error),留待下轮重试;
// 只作用于未投递行(已投递行是终态,迟到的失败上报不得改它)。
func (s *MemStore) MarkFailed(_ context.Context, id int64, cause string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if row, ok := s.outboxByID[id]; ok && !row.published {
		row.ev.Attempts++
		row.ev.LastError = cause
	}
	return nil
}

// OutboxBacklog 汇总未投递行(第四批:backlog/失败监控)。stuckAttempts 是
// "卡住"的判定阈值(attempts >= 该值);<=0 时按 1 处理,即任何失败过的行都算卡住。
func (s *MemStore) OutboxBacklog(_ context.Context, stuckAttempts int) (*OutboxBacklog, error) {
	if stuckAttempts <= 0 {
		stuckAttempts = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	out := &OutboxBacklog{}
	var oldest time.Time
	maxAttempts := -1
	for _, row := range s.outbox {
		if row.published {
			continue
		}
		out.Pending++
		if row.ev.Attempts >= stuckAttempts {
			out.Stuck++
		}
		if oldest.IsZero() || row.createdAt.Before(oldest) {
			oldest, out.OldestID = row.createdAt, row.ev.ID
		}
		if row.ev.Attempts > maxAttempts {
			maxAttempts, out.SampleError = row.ev.Attempts, row.ev.LastError
		}
	}
	if !oldest.IsZero() {
		out.OldestAge = now.Sub(oldest)
	}
	return out, nil
}
