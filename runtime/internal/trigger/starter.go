package trigger

import (
	"context"
	"errors"

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
