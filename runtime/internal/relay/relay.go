// Package relay 实现事务性 Outbox 的独立投递进程(docs/device-test-sequence.md
// 设计原则 3,差距清单 #1):claim 未投递 outbox 行 → 发 Temporal Signal
// (至少一次)→ MarkPublished;失败 attempts+1 记 last_error 留待下轮重试。
// Signal 成功与 MarkPublished 之间允许崩溃,重投由接收端幂等兜底。
package relay

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/rs/zerolog"
	"go.temporal.io/api/serviceerror"

	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

// Store 是 Relay 依赖的持久层子集。
type Store interface {
	ClaimUnpublished(ctx context.Context, limit int) ([]store.OutboxEvent, error)
	MarkPublished(ctx context.Context, id int64) error
	MarkFailed(ctx context.Context, id int64, cause string) error
}

// Signaler 是 temporal client.Client 的 signal 子集(与 callbacks.Signaler 同形)。
type Signaler interface {
	SignalWorkflow(ctx context.Context, workflowID, runID, signalName string, arg interface{}) error
}

// Relay 是 outbox 投递循环。BatchSize/PollInterval/MaxBackoff 由 cmd/relay 配置注入。
type Relay struct {
	Store    Store
	Signaler Signaler
	Log      *zerolog.Logger // 可选;nil 用 Nop

	BatchSize    int           // 每轮 claim 上限
	PollInterval time.Duration // 空转(无待投递行)时的间隔
	MaxBackoff   time.Duration // claim 连续失败时的退避上限
}

func (r *Relay) log() zerolog.Logger {
	if r.Log != nil {
		return *r.Log
	}
	return zerolog.Nop()
}

// Run 进入投递循环直到 ctx 取消(优雅退出:正在投递的行投递完本轮再返回)。
// claim 失败按指数退避(上限 MaxBackoff),不退出进程——DB 抖动必须可自愈。
func (r *Relay) Run(ctx context.Context) error {
	log := r.log()
	backoff := r.PollInterval
	for ctx.Err() == nil {
		evs, err := r.Store.ClaimUnpublished(ctx, r.BatchSize)
		if err != nil {
			log.Error().Err(err).Dur("backoff", backoff).Msg("claim unpublished failed")
			if !sleepCtx(ctx, backoff) {
				break
			}
			if backoff < r.MaxBackoff {
				backoff *= 2
				if backoff > r.MaxBackoff {
					backoff = r.MaxBackoff
				}
			}
			continue
		}
		backoff = r.PollInterval
		if len(evs) == 0 {
			if !sleepCtx(ctx, r.PollInterval) {
				break
			}
			continue
		}
		for _, ev := range evs {
			if ctx.Err() != nil {
				return nil // 剩余行保持未投递,下轮(或下一实例)继续
			}
			r.deliver(ctx, ev)
		}
	}
	return nil
}

// deliver 投递单行。三种归宿:
//   - Signal 成功 → MarkPublished;
//   - workflow not found → 视为已消费,直接 MarkPublished(workflow 已结束,
//     结果本体已落 results 表,重投无意义;否则 Relay 会对该行死循环重试);
//   - 其他错误 → MarkFailed(attempts+1 + last_error),下轮重试。
func (r *Relay) deliver(ctx context.Context, ev store.OutboxEvent) {
	log := r.log().With().Int64("outbox_id", ev.ID).Str("event_key", ev.EventKey).Logger()
	switch ev.EventType {
	case store.EventTypeTaskResult:
		var p store.ResultEventPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			// payload 坏行重投无意义,但语义上属"未知事件"类:记 failed 供监控介入
			r.fail(ctx, ev, "decode payload: "+err.Error())
			return
		}
		err := r.Signaler.SignalWorkflow(ctx, p.WorkflowID, "", wf.SignalTaskResult, p.Result)
		if err == nil {
			r.publish(ctx, ev)
			return
		}
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			log.Info().Str("workflow_id", p.WorkflowID).
				Msg("workflow not found, treat as consumed (workflow already finished)")
			r.publish(ctx, ev)
			return
		}
		log.Error().Err(err).Msg("signal failed, will retry")
		r.fail(ctx, ev, err.Error())
	default:
		// 未知事件类型是部署/版本错配:重试无法自愈,记 failed 供 backlog 监控
		// 发现(第四批:Outbox backlog 和失败监控),不阻塞其他行投递。
		log.Error().Str("event_type", ev.EventType).Msg("unsupported event type")
		r.fail(ctx, ev, "unsupported event_type: "+ev.EventType)
	}
}

func (r *Relay) publish(ctx context.Context, ev store.OutboxEvent) {
	if err := r.Store.MarkPublished(ctx, ev.ID); err != nil {
		// 标记失败:行保持未投递,下轮重投(接收端幂等兜底,docs 文末说明)
		log := r.log()
		log.Error().Err(err).Int64("outbox_id", ev.ID).Msg("mark published failed")
	}
}

func (r *Relay) fail(ctx context.Context, ev store.OutboxEvent, cause string) {
	if err := r.Store.MarkFailed(ctx, ev.ID, cause); err != nil {
		log := r.log()
		log.Error().Err(err).Int64("outbox_id", ev.ID).Msg("mark failed failed")
	}
}

// sleepCtx 睡眠 d 或直到 ctx 取消;返回 false 表示被取消(应退出循环)。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
