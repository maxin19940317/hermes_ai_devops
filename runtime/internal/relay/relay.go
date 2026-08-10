// Package relay 实现事务性 Outbox 的独立投递进程(docs/device-test-sequence.md
// 设计原则 3,差距清单 #1):claim 未投递 outbox 行 → 发 Temporal Signal
// (至少一次)→ MarkPublished;失败 attempts+1 记 last_error 留待下轮重试。
// Signal 成功与 MarkPublished 之间允许崩溃,重投由接收端幂等兜底。
package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	OutboxBacklog(ctx context.Context, stuckAttempts int) (*store.OutboxBacklog, error)
}

// Signaler 是 temporal client.Client 的 signal 子集(与 callbacks.Signaler 同形)。
type Signaler interface {
	SignalWorkflow(ctx context.Context, workflowID, runID, signalName string, arg interface{}) error
}

// Notifier 是设备隔离通知的发送端(与 internal/feishu.Sender 同形,
// 使 cmd/relay 可直接传入既有的飞书发送器实现,无需另写一套)。
type Notifier interface {
	SendText(ctx context.Context, text string) error
}

// Relay 是 outbox 投递循环。BatchSize/PollInterval/MaxBackoff 由 cmd/relay 配置注入。
type Relay struct {
	Store    Store
	Signaler Signaler
	Log      *zerolog.Logger // 可选;nil 用 Nop

	// 设备隔离通知(spec §9.2/§9.3)。Notifier 为 nil 且 DeviceNotifyDisabled
	// 为 false 时,event_type=device-quarantined 的行保持 pending 不被
	// MarkPublished——未配置绝不能等价于静默丢弃。DeviceNotifyDisabled=true
	// 是运维显式选择关闭通知("RELAY_DEVICE_NOTIFY=off"),此时标记已投递,
	// 避免永久占用 backlog。
	Notifier             Notifier
	DeviceNotifyDisabled bool

	BatchSize    int           // 每轮 claim 上限
	PollInterval time.Duration // 空转(无待投递行)时的间隔
	MaxBackoff   time.Duration // claim 连续失败时的退避上限

	// 积压监控(第四批)。BacklogInterval <= 0 关闭定期报告。
	BacklogInterval time.Duration // 报告间隔
	StuckAttempts   int           // attempts >= 此值算"卡住"
	BacklogWarnAge  time.Duration // 最老未投递行超过此年龄升级为 warn

	// Now 可注入,便于测试定期报告的触发时机;nil 用 time.Now。
	Now func() time.Time
}

func (r *Relay) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
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
	nextBacklog := r.now() // 首轮立即报一次:进程刚起来时的积压最值得看见
	for ctx.Err() == nil {
		if r.BacklogInterval > 0 && !r.now().Before(nextBacklog) {
			r.reportBacklog(ctx)
			nextBacklog = r.now().Add(r.BacklogInterval)
		}
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
	case store.EventTypeDeviceQuarantined:
		var p store.QuarantineEventPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			// payload 坏行重投无意义,但语义上属"未知事件"类:记 failed 供监控介入
			r.fail(ctx, ev, "decode payload: "+err.Error())
			return
		}
		if r.DeviceNotifyDisabled {
			// 有意关闭(RELAY_DEVICE_NOTIFY=off):标记已投递,避免永久占用 backlog
			log.Info().Str("device_id", p.DeviceID).Msg("device notify disabled, event consumed")
			r.publish(ctx, ev)
			return
		}
		if r.Notifier == nil {
			// 不得静默 MarkPublished:那会把"绝不丢"退化成静默丢弃,
			// 丢的还正是最需要人知道的事件(spec §9.3)
			r.fail(ctx, ev, "notifier not configured; set FEISHU_* or RELAY_DEVICE_NOTIFY=off")
			return
		}
		text := fmt.Sprintf("[hermes-devops] 设备已自动隔离\n设备: %s (%s)\nclient: %s\n连续失败: %d 次\n触发任务: %s\n失败阶段: %s\n解除: 发送 unquarantine %s",
			p.DisplayName, p.DeviceID, p.ClientID, p.FailStreak, p.TaskID, p.FailureStage, p.DeviceID)
		if err := r.Notifier.SendText(ctx, text); err != nil {
			log.Error().Err(err).Msg("notify failed, will retry")
			r.fail(ctx, ev, err.Error())
			return
		}
		r.publish(ctx, ev)
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

// reportBacklog 定期把 outbox 积压打进日志(第四批:backlog/失败监控)。
// 分级的用意是让日志本身可作告警条件,而不是刷一条永远是 pending=0 的流水:
//   - 有卡住的行,或最老一行超过 BacklogWarnAge → warn(该有人看了)
//   - 只是有积压 → info(投递在进行中,正常繁忙)
//   - 完全没有积压 → debug(健康时保持安静)
//
// 查询失败只记日志,绝不影响投递循环——监控挂了不该拖垮被监控的东西。
func (r *Relay) reportBacklog(ctx context.Context) {
	log := r.log()
	b, err := r.Store.OutboxBacklog(ctx, r.StuckAttempts)
	if err != nil {
		log.Error().Err(err).Msg("outbox backlog query failed")
		return
	}
	ev := log.Debug()
	switch {
	case b.Stuck > 0 || (r.BacklogWarnAge > 0 && b.OldestAge >= r.BacklogWarnAge):
		ev = log.Warn()
	case b.Pending > 0:
		ev = log.Info()
	}
	ev = ev.Int("pending", b.Pending).Int("stuck", b.Stuck).
		Dur("oldest_age", b.OldestAge.Truncate(time.Second))
	if b.OldestID > 0 {
		ev = ev.Int64("oldest_id", b.OldestID)
	}
	if b.SampleError != "" {
		ev = ev.Str("sample_error", b.SampleError)
	}
	ev.Msg("outbox backlog")
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
