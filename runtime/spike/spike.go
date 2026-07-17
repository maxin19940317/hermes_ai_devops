// Package spike 是 Temporal 的 go/no-go 验证(CLAUDE.md §12 Phase 1.4)。
// 三个最小示例:signal 接收、Activity 重试、杀 worker 进程后重放恢复。
// 结构刻意模仿 DeviceTestWorkflow 主干:dispatch(activity)→ await_result(signal)。
// spike 代码不进生产,验证结论记录在 README。
package spike

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const (
	TaskQueue    = "hermes-spike"
	ResultSignal = "task-result" // 未来对应 Client 回调 → Runtime signal
)

// Input 是 workflow 与 activity 共用的输入。
type Input struct {
	FailTimes   int    // FlakyDispatch 前 N 次真实执行注入失败(验证 RetryPolicy)
	CounterFile string // 跨进程持久的执行计数文件(验证重放不重复执行 activity)
}

// TaskResult 模拟 Client 回传的终态结果(经 signal 投递)。
type TaskResult struct {
	Verdict string
}

// Output 是 workflow 的最终产出。
type Output struct {
	DispatchAttempt int    // dispatch 成功那次的 attempt 序号(1-based)
	Verdict         string // 来自 signal
}

// SpikeWorkflow: dispatch(带重试的 activity)→ 阻塞等待结果 signal → 完成。
func SpikeWorkflow(ctx workflow.Context, in Input) (Output, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    200 * time.Millisecond,
			BackoffCoefficient: 1.0,
			MaximumAttempts:    5,
		},
	})
	var attempt int
	if err := workflow.ExecuteActivity(ctx, FlakyDispatch, in).Get(ctx, &attempt); err != nil {
		return Output{}, fmt.Errorf("dispatch: %w", err)
	}

	var res TaskResult
	workflow.GetSignalChannel(ctx, ResultSignal).Receive(ctx, &res)
	return Output{DispatchAttempt: attempt, Verdict: res.Verdict}, nil
}

// FlakyDispatch 每次真实执行都递增计数文件;前 FailTimes 次返回错误触发 SDK 重试。
// 计数文件在 worker 进程被杀后仍在,是"重放没有重复执行 activity"的证据。
func FlakyDispatch(ctx context.Context, in Input) (int, error) {
	n, err := bumpCounter(in.CounterFile)
	if err != nil {
		return 0, err
	}
	if n <= in.FailTimes {
		return 0, fmt.Errorf("injected failure %d/%d", n, in.FailTimes)
	}
	return int(activity.GetInfo(ctx).Attempt), nil
}

// bumpCounter 读取-递增-写回计数文件,返回递增后的值(首次为 1)。
// 单 workflow 串行执行,无并发写。
func bumpCounter(path string) (int, error) {
	n := 0
	if raw, err := os.ReadFile(path); err == nil {
		if v, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
			n = v
		}
	}
	n++
	if err := os.WriteFile(path, []byte(strconv.Itoa(n)), 0o644); err != nil {
		return 0, err
	}
	return n, nil
}

// ReadCounter 读取计数文件当前值(不存在视为 0),供测试断言。
func ReadCounter(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	return v
}
