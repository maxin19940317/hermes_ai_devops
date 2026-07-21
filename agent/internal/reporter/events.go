package reporter

import (
	"context"
	"time"

	"hermes-devops/agent/internal/executor"
	"hermes-devops/agent/internal/store"
)

// DefaultEventRetryInterval 是未确认事件后台重发的默认周期。
const DefaultEventRetryInterval = 5 * time.Second

// EventReporter 把 executor 状态迁移变成 task-events 回调:
// 迁移先经 store.Transition 单事务落盘(同时取 seq,崩溃不丢),再尽力
// 即发;即发失败的事件由 Run 后台循环按 (task_id, seq) 有序补报,
// 确认后 MarkEventReported。
type EventReporter struct {
	Store  *store.Store
	Client *Client
	Logf   func(format string, args ...any) // nil → 静默

	RetryInterval time.Duration // 补报周期;0 → DefaultEventRetryInterval
}

func (r *EventReporter) retryInterval() time.Duration {
	if r.RetryInterval > 0 {
		return r.RetryInterval
	}
	return DefaultEventRetryInterval
}

func (r *EventReporter) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

// OnTransition 是与 executor 迁移钩子兼容的回调(由调用方适配器补齐
// taskID/from/detail)。落盘失败或即发失败都只记日志——事件已在
// store 中(或任务状态本身异常),不阻塞执行流水线。
func (r *EventReporter) OnTransition(taskID string, from, to executor.Status, detail string) {
	ctx := context.Background()
	seq, err := r.Store.Transition(ctx, taskID, store.State(from), store.State(to), detail)
	if err != nil {
		r.logf("event: persist transition %s %s->%s: %v", taskID, from, to, err)
		return
	}
	ev := TaskEvent{
		TaskID: taskID,
		Seq:    seq,
		From:   string(from),
		To:     string(to),
		Ts:     utcNowMs(),
		Detail: detail,
	}
	key, err := r.idempotencyKey(ctx, taskID)
	if err != nil {
		r.logf("event: %s seq=%d 待发(取幂等键失败: %v)", taskID, seq, err)
		return
	}
	ev.IdempotencyKey = key
	if err := r.Client.ReportEvent(ctx, ev); err != nil {
		r.logf("event: %s seq=%d 即发失败,待后台补报: %v", taskID, seq, err)
		return
	}
	if err := r.Store.MarkEventReported(ctx, taskID, seq); err != nil {
		r.logf("event: mark reported %s seq=%d: %v", taskID, seq, err)
	}
}

// Run 启动补报循环,阻塞至 ctx 取消(返回 nil)。每轮按 (task_id, seq)
// 顺序抽干未上报事件:单任务内严格按 seq 递增发送;某任务发送失败
// (可重试错误)时跳过该任务本轮剩余事件,不拖累其他任务;400 等永久
// 拒绝重发无意义,记日志后按已上报处理,避免毒事件堵死队列。
func (r *EventReporter) Run(ctx context.Context) error {
	for {
		r.drain(ctx)
		timer := time.NewTimer(r.retryInterval())
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// drain 抽干一轮未上报事件。
func (r *EventReporter) drain(ctx context.Context) {
	events, err := r.Store.UnreportedEvents(ctx)
	if err != nil {
		r.logf("event: load unreported: %v", err)
		return
	}
	blocked := map[string]bool{}
	keys := map[string]string{}
	for _, ev := range events {
		if ctx.Err() != nil {
			return
		}
		if blocked[ev.TaskID] {
			continue
		}
		key, ok := keys[ev.TaskID]
		if !ok {
			key, err = r.idempotencyKey(ctx, ev.TaskID)
			if err != nil {
				r.logf("event: %s seq=%d 取幂等键失败,本轮跳过: %v", ev.TaskID, ev.Seq, err)
				blocked[ev.TaskID] = true
				continue
			}
			keys[ev.TaskID] = key
		}
		te := TaskEvent{
			TaskID:         ev.TaskID,
			IdempotencyKey: key,
			Seq:            ev.Seq,
			From:           string(ev.FromState),
			To:             string(ev.ToState),
			Ts:             formatTS(ev.Ts),
			Detail:         ev.Detail,
		}
		if err := r.Client.ReportEvent(ctx, te); err != nil {
			if !Retryable(err) {
				// 永久拒绝(事件不合法):重发不可能成功,按已上报处理并记日志
				r.logf("event: %s seq=%d 被 Runtime 拒绝(不重发): %v", ev.TaskID, ev.Seq, err)
			} else {
				r.logf("event: %s seq=%d 补报失败,下轮重试: %v", ev.TaskID, ev.Seq, err)
				blocked[ev.TaskID] = true
				continue
			}
		}
		if err := r.Store.MarkEventReported(ctx, ev.TaskID, ev.Seq); err != nil {
			r.logf("event: mark reported %s seq=%d: %v", ev.TaskID, ev.Seq, err)
		}
	}
}

func (r *EventReporter) idempotencyKey(ctx context.Context, taskID string) (string, error) {
	t, err := r.Store.GetTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	return t.IdempotencyKey, nil
}
