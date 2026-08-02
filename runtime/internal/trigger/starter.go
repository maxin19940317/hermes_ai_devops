package trigger

import (
	"context"
	"errors"
	"fmt"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	wf "hermes-devops/runtime/internal/workflow"
)

// TemporalStarter 是 WorkflowStarter 的 Temporal 实现。
// workflow ID 由 bundle 确定(project+commit+pipeline iid,显式 retry 时加 -r{N});
// 复用策略 RejectDuplicate(差距 #11):webhook/kick 重放绝不自动重启——
// 运行中/已完成/已失败的同 ID workflow 都拒绝重复;只有显式 retry(新 ID)
// 才会起新 run。重复投递由 AlreadyStarted 归一化为幂等成功。
// workflow 按类型名启动(DeviceTestWorkflow 本体属 Phase 1.6)。
type TemporalStarter struct {
	Client    client.Client
	TaskQueue string
}

func (s *TemporalStarter) StartDeviceTest(ctx context.Context, in wf.DeviceTestInput) (string, bool, error) {
	id := in.WorkflowID()
	_, err := s.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                                       id,
		TaskQueue:                                s.TaskQueue,
		WorkflowIDReusePolicy:                    enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
	}, wf.DeviceTestWorkflowName, in)
	if err != nil {
		var already *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &already) {
			return id, false, nil // 已存在 → 幂等成功(不重启)
		}
		return "", false, err
	}
	return id, true, nil
}

func (s *TemporalStarter) WorkflowClosed(ctx context.Context, workflowID string) (bool, error) {
	desc, err := s.Client.DescribeWorkflowExecution(ctx, workflowID, "")
	if err != nil {
		return false, fmt.Errorf("describe workflow %s: %w", workflowID, err)
	}
	status := desc.GetWorkflowExecutionInfo().GetStatus()
	switch status {
	case enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
		enumspb.WORKFLOW_EXECUTION_STATUS_PAUSED:
		return false, nil
	case enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED,
		enumspb.WORKFLOW_EXECUTION_STATUS_FAILED,
		enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED,
		enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED,
		enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT,
		enumspb.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW:
		return true, nil
	default:
		return false, fmt.Errorf("describe workflow %s: unknown execution status %v", workflowID, status)
	}
}

func (s *TemporalStarter) WorkflowResult(
	ctx context.Context,
	workflowID string,
) (*wf.DeviceTestOutput, error) {
	var out wf.DeviceTestOutput
	if err := s.Client.GetWorkflow(ctx, workflowID, "").Get(ctx, &out); err != nil {
		return nil, fmt.Errorf("get workflow result %s: %w", workflowID, err)
	}
	return &out, nil
}
