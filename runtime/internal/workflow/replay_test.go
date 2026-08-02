// 本文件无 build tag,永远进普通 CI(与 record_history_test.go 严格分离,
// 后者带 //go:build record_history,只在显式录制时编译)。
package workflow

import (
	"os"
	"testing"

	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/encoding/protojson"

	historypb "go.temporal.io/api/history/v1"
)

// fixtureWorkflowID 是录制 testdata/history-pre-notify-card.json 时使用的
// workflow ID(与 devicetest_test.go 的 const wfID 保持一致)。重放必须显式把它
// 传给 OriginalExecution——原因见下方 replayFixture 的注释。
const fixtureWorkflowID = wfID

// replayFixture 从 JSON 文件反序列化 history 并重放。
//
// 关键坑(调试记录):不能直接用 worker.WorkflowReplayer.ReplayWorkflowHistoryFromJSONFile,
// 也不能用零值 OriginalExecution 调 ReplayWorkflowHistory——两者内部都以空
// WorkflowExecution{} 起播,重放期间 workflow.GetInfo(ctx).WorkflowExecution.ID
// 会变成 SDK 占位符 "ReplayId",而不是录制时的真实 ID。runAttempt 把
// workflow.GetInfo(ctx).WorkflowExecution.ID 拼进 taskID,taskID 又是
// AcquireDevice/CreateTask/Dispatch/LoadResult 每个活动入参的一部分——
// 重放时这些入参会因为 wfID 变成 "ReplayId" 而与 history 里记录的不一致,
// SDK 的确定性校验随即在毫不相关的位置(LoadResult 的 ActivityTaskScheduled)
// 报 "TMPRL1100 lookup failed for scheduledEventID to activityID"。
// 修复是显式传 OriginalExecution.ID=录制时的 workflow ID(RunID 留空即可,
// 生产代码不消费它)。这也是选择 in-memory 路径
// (protojson 反序列化 + ReplayWorkflowHistoryWithOptions)而非
// ReplayWorkflowHistoryFromJSONFile 的唯一原因——后者没有带 Options 的变体,
// 无法设置 OriginalExecution。
func replayFixture(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	hist := &historypb.History{}
	if err := protojson.Unmarshal(data, hist); err != nil {
		return err
	}
	return replayHistory(hist)
}

// replayHistory 重放内存态 history(录制阶段落盘前自检、以及 replayFixture
// 都复用这条路径,确保两处用的是同一套"什么样的重放才算数"的判断)。
func replayHistory(hist *historypb.History) error {
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(DeviceTestWorkflow)
	return replayer.ReplayWorkflowHistoryWithOptions(nil, hist, worker.ReplayWorkflowHistoryOptions{
		OriginalExecution: workflow.Execution{ID: fixtureWorkflowID},
	})
}

// TestReplayPreNotifyCardHistory 重放 notify-card 改动**之前**录制的 history
// (设计文档 §6 的 DoD)。本测试在生产代码尚未改动时就必须通过——它是回归判据:
// 后续任务改完 workflow 后它仍须通过,才说明在途 workflow 不会因版本分支
// (workflow.GetVersion)而重放失败。
func TestReplayPreNotifyCardHistory(t *testing.T) {
	if err := replayFixture("testdata/history-pre-notify-card.json"); err != nil {
		t.Fatalf("重放改动前的 history 失败: %v", err)
	}
}
