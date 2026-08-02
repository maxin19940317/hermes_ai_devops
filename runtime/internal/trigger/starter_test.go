package trigger

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/mocks"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"hermes-devops/runtime/internal/testtemporal"
	wf "hermes-devops/runtime/internal/workflow"
)

const inspectableTaskQueue = "inspectable-workflow"

var inspectableOutput = wf.DeviceTestOutput{Tasks: []wf.TaskSummary{
	{Variant: "v1", Verdict: "TEST_FAILED"},
	{Variant: "v2", Verdict: wf.VerdictSkipped},
}}

func inspectableWorkflow(ctx workflow.Context) (*wf.DeviceTestOutput, error) {
	workflow.GetSignalChannel(ctx, "finish").Receive(ctx, nil)
	return &inspectableOutput, nil
}

type inspectableRun struct {
	ctx     context.Context
	client  client.Client
	run     client.WorkflowRun
	starter *TemporalStarter
}

func startInspectableWorkflow(t *testing.T, suffix string) inspectableRun {
	t.Helper()

	addr := testtemporal.StartDevServer(t)
	c, err := client.Dial(client.Options{HostPort: addr})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(c.Close)

	taskQueue := inspectableTaskQueue + "-" + suffix
	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(inspectableWorkflow)
	if err := w.Start(); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	t.Cleanup(w.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: "inspectable-" + suffix, TaskQueue: taskQueue,
	}, inspectableWorkflow)
	if err != nil {
		t.Fatalf("execute workflow: %v", err)
	}
	waitForWorkflowStatus(t, ctx, c, run.GetID(), enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING)

	return inspectableRun{
		ctx: ctx, client: c, run: run,
		starter: &TemporalStarter{Client: c, TaskQueue: taskQueue},
	}
}

func waitForWorkflowStatus(
	t *testing.T,
	ctx context.Context,
	c client.Client,
	workflowID string,
	want enumspb.WorkflowExecutionStatus,
) {
	t.Helper()

	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		desc, err := c.DescribeWorkflowExecution(ctx, workflowID, "")
		if err == nil && desc.GetWorkflowExecutionInfo().GetStatus() == want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for workflow %q status %v: %v", workflowID, want, ctx.Err())
		case <-deadline.C:
			t.Fatalf("workflow %q did not reach status %v", workflowID, want)
		case <-ticker.C:
		}
	}
}

func describeResponse(status enumspb.WorkflowExecutionStatus) *workflowservice.DescribeWorkflowExecutionResponse {
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: status},
	}
}

func TestTemporalStarterWorkflowClosed(t *testing.T) {
	ir := startInspectableWorkflow(t, "closed")

	closed, err := ir.starter.WorkflowClosed(ir.ctx, ir.run.GetID())
	if err != nil || closed {
		t.Fatalf("running WorkflowClosed = (%v, %v), want (false, nil)", closed, err)
	}
	if err := ir.client.SignalWorkflow(ir.ctx, ir.run.GetID(), ir.run.GetRunID(), "finish", nil); err != nil {
		t.Fatalf("signal finish: %v", err)
	}
	var got wf.DeviceTestOutput
	if err := ir.run.Get(ir.ctx, &got); err != nil {
		t.Fatalf("get workflow result: %v", err)
	}
	if !reflect.DeepEqual(got, inspectableOutput) {
		t.Fatalf("workflow result = %+v, want %+v", got, inspectableOutput)
	}

	closed, err = ir.starter.WorkflowClosed(ir.ctx, ir.run.GetID())
	if err != nil || !closed {
		t.Fatalf("completed WorkflowClosed = (%v, %v), want (true, nil)", closed, err)
	}

	_, err = ir.starter.WorkflowClosed(ir.ctx, "missing-workflow")
	var notFound *serviceerror.NotFound
	if !errors.As(err, &notFound) {
		t.Fatalf("missing WorkflowClosed error = %v, want wrapped NotFound", err)
	}
}

func TestTemporalStarterWorkflowClosedRejectsUnspecified(t *testing.T) {
	for _, status := range []enumspb.WorkflowExecutionStatus{
		enumspb.WORKFLOW_EXECUTION_STATUS_UNSPECIFIED,
		enumspb.WorkflowExecutionStatus(99),
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			c := mocks.NewClient(t)
			c.On("DescribeWorkflowExecution", mock.Anything, "wf-unknown", "").
				Return(describeResponse(status), nil).Once()
			s := &TemporalStarter{Client: c}

			if _, err := s.WorkflowClosed(context.Background(), "wf-unknown"); err == nil {
				t.Fatalf("WorkflowClosed status %v returned nil error", status)
			}
		})
	}
}

func TestTemporalStarterWorkflowClosedStatusMatrix(t *testing.T) {
	for _, status := range []enumspb.WorkflowExecutionStatus{
		enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED,
		enumspb.WORKFLOW_EXECUTION_STATUS_FAILED,
		enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED,
		enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED,
		enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT,
		enumspb.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW,
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			c := mocks.NewClient(t)
			c.On("DescribeWorkflowExecution", mock.Anything, "wf-closed", "").
				Return(describeResponse(status), nil).Once()
			s := &TemporalStarter{Client: c}

			closed, err := s.WorkflowClosed(context.Background(), "wf-closed")
			if err != nil || !closed {
				t.Fatalf("WorkflowClosed status %v = (%v, %v), want (true, nil)", status, closed, err)
			}
		})
	}
}

func TestTemporalStarterWorkflowPausedIsNotClosed(t *testing.T) {
	c := mocks.NewClient(t)
	c.On("DescribeWorkflowExecution", mock.Anything, "wf-paused", "").
		Return(describeResponse(enumspb.WORKFLOW_EXECUTION_STATUS_PAUSED), nil).Once()
	s := &TemporalStarter{Client: c}

	closed, err := s.WorkflowClosed(context.Background(), "wf-paused")
	if err != nil || closed {
		t.Fatalf("WorkflowClosed = (%v, %v), want (false, nil)", closed, err)
	}
}

func TestTemporalStarterWorkflowResult(t *testing.T) {
	ir := startInspectableWorkflow(t, "result")
	if err := ir.client.SignalWorkflow(ir.ctx, ir.run.GetID(), ir.run.GetRunID(), "finish", nil); err != nil {
		t.Fatalf("signal finish: %v", err)
	}

	got, err := ir.starter.WorkflowResult(ir.ctx, ir.run.GetID())
	if err != nil {
		t.Fatalf("WorkflowResult: %v", err)
	}
	if !reflect.DeepEqual(*got, inspectableOutput) {
		t.Fatalf("WorkflowResult = %+v, want %+v", *got, inspectableOutput)
	}
}

func TestTemporalStarterWorkflowResultUnavailable(t *testing.T) {
	addr := testtemporal.StartDevServer(t)
	c, err := client.Dial(client.Options{HostPort: addr})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(c.Close)
	s := &TemporalStarter{Client: c}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = s.WorkflowResult(ctx, "missing-workflow")
	if err == nil {
		t.Fatal("WorkflowResult missing workflow returned nil error")
	}
	var notFound *serviceerror.NotFound
	if !errors.As(err, &notFound) {
		t.Fatalf("WorkflowResult error = %v, want wrapped NotFound", err)
	}
	if got := err.Error(); got == "" || !containsAll(got, "missing-workflow", "get workflow result") {
		t.Fatalf("WorkflowResult error = %q, want workflow ID and operation", got)
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}

// 真实 Temporal dev server 上验证:按名启动、ID 确定性去重(重复投递不重跑)。
func TestTemporalStarterStartAndDedup(t *testing.T) {
	addr := testtemporal.StartDevServer(t)
	c, err := client.Dial(client.Options{HostPort: addr})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(c.Close)

	s := &TemporalStarter{Client: c, TaskQueue: "device-test"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	in := wf.DeviceTestInput{
		Project: "grp/algo-super-sdk", Commit: "abcd1234", PipelineID: 42, Version: "1.2.3",
	}

	id1, started1, err := s.StartDeviceTest(ctx, in)
	if err != nil || !started1 {
		t.Fatalf("first start: id=%q started=%v err=%v", id1, started1, err)
	}
	if id1 != in.WorkflowID() {
		t.Errorf("workflow id = %q, want %q", id1, in.WorkflowID())
	}
	// workflow 已在 server 端登记(无 worker,任务积压属预期)
	desc, err := c.DescribeWorkflowExecution(ctx, id1, "")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if st := desc.WorkflowExecutionInfo.Status; st != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		t.Errorf("status = %v, want RUNNING", st)
	}

	// 同一 bundle 重复投递:不报错、不重跑
	id2, started2, err := s.StartDeviceTest(ctx, in)
	if err != nil {
		t.Fatalf("duplicate start 应幂等成功: %v", err)
	}
	if started2 || id2 != id1 {
		t.Errorf("duplicate: id=%q started=%v, want id=%q started=false", id2, started2, id1)
	}
}

// 差距 #11:RejectDuplicate——上次失败/终止的 workflow 也不得被普通重放
// 自动重启;只有显式 retry(ID 加 -r{N})能起新 run。
func TestTemporalStarterRejectDuplicateAfterTerminate(t *testing.T) {
	addr := testtemporal.StartDevServer(t)
	c, err := client.Dial(client.Options{HostPort: addr})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(c.Close)

	s := &TemporalStarter{Client: c, TaskQueue: "device-test"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	in := wf.DeviceTestInput{
		Project: "grp/algo-super-sdk", Commit: "abcd1234", PipelineID: 42, Version: "1.2.3",
	}
	id1, started1, err := s.StartDeviceTest(ctx, in)
	if err != nil || !started1 {
		t.Fatalf("first start: id=%q started=%v err=%v", id1, started1, err)
	}
	// 终止(等价于失败终态)后普通重放:必须拒绝重启,幂等返回 started=false
	if err := c.TerminateWorkflow(ctx, id1, "", "test terminate"); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	_, started2, err := s.StartDeviceTest(ctx, in)
	if err != nil {
		t.Fatalf("terminated 后重放应幂等成功(不重启): %v", err)
	}
	if started2 {
		t.Error("terminated workflow 被普通重放自动重启(违反差距 #11)")
	}
	// 显式 retry:Attempt=1 → 新 ID -r1,允许起新 run
	retry := in
	retry.Attempt = 1
	id3, started3, err := s.StartDeviceTest(ctx, retry)
	if err != nil || !started3 {
		t.Fatalf("explicit retry: id=%q started=%v err=%v", id3, started3, err)
	}
	if id3 != id1+"-r1" {
		t.Errorf("retry id = %q, want %q", id3, id1+"-r1")
	}
}
