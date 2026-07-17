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
// workflow ID 由 bundle 确定(project+commit+pipeline iid),配合
// AllowDuplicateFailedOnly 复用策略实现去重语义:
// 重复投递不重跑;仅当上一次以失败终结时允许重新触发。
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
		WorkflowIDReusePolicy:                    enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
	}, wf.DeviceTestWorkflowName, in)
	if err != nil {
		var already *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &already) {
			return id, false, nil // 已存在 → 幂等成功
		}
		return "", false, err
	}
	return id, true, nil
}
