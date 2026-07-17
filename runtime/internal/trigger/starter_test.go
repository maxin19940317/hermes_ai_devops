package trigger

import (
	"context"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"hermes-devops/runtime/internal/testtemporal"
	wf "hermes-devops/runtime/internal/workflow"
)

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
