package spike_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"hermes-devops/runtime/internal/testtemporal"
	"hermes-devops/runtime/spike"
)

// ---- 基础设施:client / worker 进程(dev server 见 internal/testtemporal) ----

func dial(t *testing.T, addr string) client.Client {
	t.Helper()
	c, err := client.Dial(client.Options{HostPort: addr})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// buildWorkerBinary 编译 spike-worker,供杀进程场景使用。
func buildWorkerBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "spike-worker")
	cmd := exec.Command("go", "build", "-o", bin, "hermes-devops/runtime/cmd/spike-worker")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build spike-worker: %v\n%s", err, out)
	}
	return bin
}

func startWorkerProc(t *testing.T, bin, addr string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, "--address", addr)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start worker proc: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

// ---- 场景 1+2:signal 接收 与 Activity 重试 ----

func TestSignalDeliveryAndActivityRetry(t *testing.T) {
	addr := testtemporal.StartDevServer(t)
	c := dial(t, addr)

	w := worker.New(c, spike.TaskQueue, worker.Options{})
	w.RegisterWorkflow(spike.SpikeWorkflow)
	w.RegisterActivity(spike.FlakyDispatch)
	if err := w.Start(); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	t.Cleanup(w.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	counter := filepath.Join(t.TempDir(), "counter")

	run, err := c.ExecuteWorkflow(ctx,
		client.StartWorkflowOptions{ID: "spike-retry-signal", TaskQueue: spike.TaskQueue},
		spike.SpikeWorkflow, spike.Input{FailTimes: 2, CounterFile: counter})
	if err != nil {
		t.Fatalf("start workflow: %v", err)
	}

	// signal 先于 workflow 到达等待点发送:验证 signal 会被缓存不丢失
	if err := c.SignalWorkflow(ctx, run.GetID(), run.GetRunID(),
		spike.ResultSignal, spike.TaskResult{Verdict: "PASSED"}); err != nil {
		t.Fatalf("signal: %v", err)
	}

	var out spike.Output
	if err := run.Get(ctx, &out); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if out.Verdict != "PASSED" {
		t.Errorf("verdict = %q, signal 载荷未正确送达", out.Verdict)
	}
	if out.DispatchAttempt != 3 {
		t.Errorf("dispatch attempt = %d, want 3(前 2 次注入失败由 RetryPolicy 重试)", out.DispatchAttempt)
	}
	if n := spike.ReadCounter(counter); n != 3 {
		t.Errorf("activity 真实执行 %d 次, want 3", n)
	}
}

// ---- 场景 3:SIGKILL worker 后重放恢复,activity 不重复执行 ----

func TestWorkerKillThenReplayRecovery(t *testing.T) {
	addr := testtemporal.StartDevServer(t)
	c := dial(t, addr)
	bin := buildWorkerBinary(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	counter := filepath.Join(t.TempDir(), "counter")

	wk := startWorkerProc(t, bin, addr)

	run, err := c.ExecuteWorkflow(ctx,
		client.StartWorkflowOptions{ID: "spike-kill-replay", TaskQueue: spike.TaskQueue},
		spike.SpikeWorkflow, spike.Input{FailTimes: 0, CounterFile: counter})
	if err != nil {
		t.Fatalf("start workflow: %v", err)
	}

	// 等 dispatch activity 真实执行完(计数文件出现),此时 workflow 阻塞在 signal 等待点
	waitUntil(t, 30*time.Second, func() bool { return spike.ReadCounter(counter) == 1 })

	// SIGKILL worker 进程(模拟 Runtime 崩溃)
	if err := wk.Process.Kill(); err != nil {
		t.Fatalf("kill worker: %v", err)
	}
	_, _ = wk.Process.Wait()

	// workflow 必须仍存活于 server 端
	desc, err := c.DescribeWorkflowExecution(ctx, run.GetID(), run.GetRunID())
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if s := desc.WorkflowExecutionInfo.Status; s != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		t.Fatalf("worker 死后 workflow status = %v, want RUNNING", s)
	}

	// 重启 worker → 发 signal → workflow 应从历史重放并完成
	_ = startWorkerProc(t, bin, addr)
	if err := c.SignalWorkflow(ctx, run.GetID(), run.GetRunID(),
		spike.ResultSignal, spike.TaskResult{Verdict: "PASSED"}); err != nil {
		t.Fatalf("signal after restart: %v", err)
	}

	var out spike.Output
	if err := run.Get(ctx, &out); err != nil {
		t.Fatalf("workflow result after replay: %v", err)
	}
	if out.Verdict != "PASSED" || out.DispatchAttempt != 1 {
		t.Errorf("output = %+v, want verdict=PASSED attempt=1", out)
	}
	// 关键断言:重放只回放历史,activity 没有被重复执行
	if n := spike.ReadCounter(counter); n != 1 {
		t.Errorf("杀进程重启后 activity 真实执行 %d 次, want 1(禁止重复执行)", n)
	}
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("waitUntil: 条件在超时内未满足")
}
