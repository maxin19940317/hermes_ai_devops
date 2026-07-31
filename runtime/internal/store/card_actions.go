package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// 设计文档 2026-07-31-feishu-card-actions-design §3.1:飞书按钮点击必须在同步
// 应答返回之前落盘——飞书收到成功应答后不再重投,中间崩溃的点击就永久消失。
// 本文件(MemStore 侧)与 postgres_card_actions.go(PGStore 侧)只覆盖 inbox 一张表;
// card_actions / card_action_messages / snapshots 留给后续 task。

var (
	// ErrInboxNotFound: GetInbox 查询到未知 event_id。
	ErrInboxNotFound = errors.New("card action inbox event not found")
	// ErrInboxNotClaimable 覆盖三种情况:event_id 不存在、已进入终态(processed)、
	// 或租约仍被别的 worker 持有——调用方对三者一视同仁(退避重试),故只用一个
	// 哨兵值,不强行区分。
	ErrInboxNotClaimable = errors.New("card action inbox event not claimable")
)

// InboxRow 对应 card_action_inbox 一行(schema.sql,设计文档 §3.1)。
// LeaseExpiresAt/ProcessedAt 用指针表达列的可空性:首次入库的行两者皆 nil。
type InboxRow struct {
	EventID        string
	Disposition    string // accepted|rejected
	AckToast       string // 同步应答原文;重投的飞书事件必须原样重放这句话
	Action         string
	WorkflowID     string
	ActorOpenID    string
	OpenMessageID  string
	PayloadDigest  string // 同步段算出的摘要;恢复路径不得重新猜(原始 payload 已不在手上)
	State          string // received|processed
	Owner          string
	LastError      string
	LeaseExpiresAt *time.Time
	ProcessedAt    *time.Time
	Attempts       int
}

// AuditRow 对应 audit_log 一行。CardActionWorkflowID/InboxEventID 二选一
// (对应该行审计关联的是哪张表);为空字符串时写 NULL,不占用对应 UNIQUE 外键。
type AuditRow struct {
	Actor                string
	Action               string
	Target               string
	PayloadDigest        string
	CardActionWorkflowID string
	InboxEventID         string
}

// validateAuditRow 复现 audit_log 的 CHECK:actor 与 action 均不得为空串。
// MemStore 没有真实数据库约束,必须在这里手动挡:否则 Task 4 的"审计失败→整笔
// 回滚"用例在两套实现上会漂移——PGStore 靠 Postgres 的 23514 拒绝插入,
// MemStore 若不复现同一条件就会把无归属的审计行悄悄接受。
func validateAuditRow(a AuditRow) error {
	if a.Actor == "" || a.Action == "" {
		return fmt.Errorf("audit row requires non-empty actor and action")
	}
	return nil
}

// PutInbox 写入一次点击,以 event_id 幂等去重(跨进程、跨重启,飞书事件 ID
// 天然满足)。已存在的事件原样返回既有行(inserted=false)且 existing 非
// nil,供调用方对重投的飞书回调重放同一句 ack_toast,而不是重新走一遍
// 判定逻辑。
//
// disposition=rejected 的行必须在这一次调用里就以终态写入(state=processed,
// processed_at 非空):Postgres 侧 CHECK 立即生效且不可 DEFERRABLE,先插
// received 再 UPDATE 会在插入那一刻就以 23514 失败,不存在"先插后补"的路径
// (§CLAUDE.md 附带上下文)。MemStore 没有这个约束,但两套实现的调用契约必须
// 一致,所以这里同样要求调用方直接传入终态的 row。
//
// auditOnReject 只在真正发生插入时才写(重复调用不重复审计:一个事件恰好
// 一条审计,§3.5);写入失败时整个 PutInbox 必须回滚——把事件标记为已处理、
// 审计表里却查无此人,比拒绝这次写入更糟。
func (s *MemStore) PutInbox(_ context.Context, row InboxRow, auditOnReject *AuditRow) (*InboxRow, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.inbox[row.EventID]; ok {
		out := existing
		return &out, false, nil
	}

	if auditOnReject != nil {
		if err := validateAuditRow(*auditOnReject); err != nil {
			// 校验在任何 map 写入之前完成:没有状态可回滚,天然满足"整笔失败"。
			return nil, false, fmt.Errorf("put inbox %s: audit: %w", row.EventID, err)
		}
	}

	if row.State == "processed" && row.ProcessedAt == nil {
		now := time.Now().UTC()
		row.ProcessedAt = &now
	}

	s.inbox[row.EventID] = row
	if auditOnReject != nil {
		s.auditLog = append(s.auditLog, *auditOnReject)
	}
	out := row
	return &out, true, nil
}

// GetInbox 按 event_id 查询;未知事件返回 ErrInboxNotFound。
func (s *MemStore) GetInbox(_ context.Context, eventID string) (*InboxRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.inbox[eventID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrInboxNotFound, eventID)
	}
	out := row
	return &out, nil
}

// ClaimInbox 用条件更新取租约,谓词必须覆盖 lease_expires_at IS NULL(首次
// 入库的行租约天然是 NULL)与 lease_expires_at < now()(过期可回收)两支——
// 漏掉 IS NULL 那一支,新落库的行会永远抢不到租约。
// 不区分"事件不存在/已终态/租约仍被占用"三种落空原因,统一返回
// ErrInboxNotClaimable,调用方按同一策略处理(退避重试)。
func (s *MemStore) ClaimInbox(_ context.Context, eventID, token string, lease time.Duration) (*InboxRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	row, ok := s.inbox[eventID]
	now := time.Now().UTC()
	claimable := ok && row.State == "received" &&
		(row.LeaseExpiresAt == nil || row.LeaseExpiresAt.Before(now))
	if !claimable {
		return nil, fmt.Errorf("%w: %s", ErrInboxNotClaimable, eventID)
	}

	row.Owner = token
	expires := now.Add(lease)
	row.LeaseExpiresAt = &expires
	row.Attempts++
	s.inbox[eventID] = row
	out := row
	return &out, nil
}
