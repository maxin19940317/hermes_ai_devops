//go:build record_history

// 本文件只负责"录制"改动前的 workflow history,不进普通 CI(见文件顶部 build
// tag)。它必须与 replay_test.go 分开:tag 作用于整个文件,混在一起会连累
// 那条永久回归测试也被挡在 CI 外。
//
// 用法(-count=1 是硬要求,测试缓存命中就跳过写盘副作用):
//
//	go test -tags record_history ./internal/workflow -run '^TestRecordPreNotifyCardHistory$' -count=1 -v
package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"google.golang.org/protobuf/encoding/protojson"

	"hermes-devops/runtime/internal/testtemporal"
)

const recordHistoryTaskQueue = "record-history-tq"

// TestRecordPreNotifyCardHistory 用真实 dev server(非 testsuite 的模拟时钟)跑
// 一遍"选变体 → 一个变体 PASSED → Notify"的最小路径,把 workflow history 落盘
// 为 notify-card 改动**之前**的回归判据(设计 §5/§6)。
//
// 这份 history 只能录一次:Task 5 改完 workflow 代码后再跑本录制器,会把它
// 替换成"改动后"的历史,replay_test.go 就再也发现不了版本分支(GetVersion)的
// 兼容性问题了。fixture 已存在时下面的 O_EXCL 会拒绝覆盖并报错。
func TestRecordPreNotifyCardHistory(t *testing.T) {
	addr := testtemporal.StartDevServer(t)
	c, err := client.Dial(client.Options{HostPort: addr})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	f := &fakeActs{specs: []TestSpec{spec1()}}
	w := worker.New(c, recordHistoryTaskQueue, worker.Options{})
	w.RegisterWorkflow(DeviceTestWorkflow)
	w.RegisterActivity(f)
	if err := w.Start(); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	defer w.Stop()

	// 结果先备好:workflow 收到 signal 后会经 LoadResult 权威读 results 表
	// (原则 3/差距 #2),不是直接消费 signal 载荷。
	sig := passResult(taskID("a1"))
	seedResult(f, sig)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: wfID, TaskQueue: recordHistoryTaskQueue,
	}, DeviceTestWorkflow, input())
	if err != nil {
		t.Fatalf("start workflow: %v", err)
	}
	// 故意等 Dispatch 活动真正执行完(fakeActs.dispatched 出现一条记录)再发
	// signal,而不是像 spike_test.go 那样抢在最前面发:这样产生的 history 里
	// Timer 会先真正挂起等待、signal 到达后另起一个 workflow task 取消它,
	// 更贴近生产时序(agent 总要花点时间才会回结果)。
	// 注:早期怀疑过"同一 workflow task 内 Timer 起+撤"会让 WorkflowReplayer
	// 的命令 ID 预测失配(TMPRL1100),用最小复现单独验证后排除——那不是根因,
	// 见下面 replayHistory/replay_test.go 顶部注释里记录的真实原因
	// (OriginalExecution 未设置,taskID 里的 workflow ID 在重放时被替换成
	// SDK 占位符)。这里保留延迟发送,单纯是因为它让 history 更接近真实生产
	// 时序,不是绕过某个 bug 的手段。
	deadline := time.Now().Add(10 * time.Second)
	for {
		f.mu.Lock()
		dispatched := len(f.dispatched)
		f.mu.Unlock()
		if dispatched >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("10s 内 Dispatch 活动未执行,无法安全发送 signal")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// 再留一点余量,让 workflow task 处理完 Dispatch 的完成事件、真正进入
	// awaitResult 的 Timer 等待(而不仅仅是活动本身跑完)。
	time.Sleep(300 * time.Millisecond)
	if err := c.SignalWorkflow(ctx, run.GetID(), run.GetRunID(), SignalTaskResult, sig); err != nil {
		t.Fatalf("signal: %v", err)
	}

	var out DeviceTestOutput
	if err := run.Get(ctx, &out); err != nil {
		t.Fatalf("workflow 未完成: %v", err)
	}
	if len(out.Tasks) != 1 || out.Tasks[0].Verdict != "PASSED" {
		t.Fatalf("workflow 输出 = %+v, want 单变体 PASSED", out.Tasks)
	}

	const fixture = "testdata/history-pre-notify-card.json"

	hist := fetchHistory(t, c, run.GetID(), run.GetRunID()) // 步骤 1a

	// 落盘前自检:复用 replay_test.go 的 replayHistory(与 CI 里
	// TestReplayPreNotifyCardHistory 走同一条重放路径),写入一份重放不了的
	// fixture 比不写更糟(会把假绿的回归判据焊死在仓库里)。
	if err := replayHistory(hist); err != nil {
		t.Fatalf("刚录制的 history 自重放失败,拒绝落盘: %v", err)
	}

	// 步骤 2:按 EventType 结构化校验,不做字节级字符串搜索——protojson 把
	// enum 写成 proto 名(EVENT_TYPE_WORKFLOW_EXECUTION_STARTED 等,见
	// go.temporal.io/api enums/v1/event_type.pb.go),字节搜索会拒掉每一份
	// 合法的 history。
	var started, completed bool
	for _, ev := range hist.GetEvents() {
		switch ev.GetEventType() {
		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED:
			started = true
		case enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_COMPLETED:
			completed = true
		}
	}
	if !started || !completed {
		t.Fatalf("history 不完整(started=%v completed=%v, %d 事件),拒绝落盘",
			started, completed, len(hist.GetEvents()))
	}

	data := serializeHistory(t, hist) // 步骤 1b
	if err := os.MkdirAll(filepath.Dir(fixture), 0o755); err != nil {
		t.Fatalf("创建 testdata 目录失败: %v", err) // 步骤 3
	}
	fh, err := os.OpenFile(fixture, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644) // 步骤 4:原子取得首次写入权
	if errors.Is(err, os.ErrExist) {
		t.Fatalf("%s 已存在,拒绝覆盖。这份 history 必须是**改动前**录的;"+
			"Task 5 之后再跑本录制器会把它替换成改动后的历史,"+
			"重放测试就再也发现不了版本分支的问题。要重录请先手工删除。", fixture)
	}
	if err != nil {
		t.Fatalf("创建 %s 失败: %v", fixture, err)
	}
	// 步骤 6:失败即删半成品,否则 O_EXCL 会被一个残缺文件永久占住,
	// 从此录不了新 fixture(挡它的正是那个残缺文件本身)。
	ok := false
	defer func() {
		cerr := fh.Close()
		if ok && cerr == nil {
			return
		}
		if rerr := os.Remove(fixture); rerr != nil && !os.IsNotExist(rerr) {
			t.Errorf("删除半成品 %s 失败,后续录制会被 O_EXCL 永久挡住,请手工删除: %v",
				fixture, rerr)
		}
		if cerr != nil {
			t.Errorf("关闭 %s 失败: %v", fixture, cerr)
		}
	}()
	if _, err := fh.Write(data); err != nil { // 步骤 5
		t.Fatalf("写入 %s 失败(将删除): %v", fixture, err)
	}
	ok = true
}

// fetchHistory 拉取完整 workflow history(全量事件,含活动入参/结果 payload)。
func fetchHistory(t *testing.T, c client.Client, workflowID, runID string) *historypb.History {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	iter := c.GetWorkflowHistory(ctx, workflowID, runID, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	hist := &historypb.History{}
	for iter.HasNext() {
		ev, err := iter.Next()
		if err != nil {
			t.Fatalf("读取 history 事件失败: %v", err)
		}
		hist.Events = append(hist.Events, ev)
	}
	return hist
}

// serializeHistory 用标准 protojson(google.golang.org/protobuf/encoding/protojson)
// 序列化 history,与 replay_test.go 用 protojson.Unmarshal 读回配套(in-memory
// 路径,而不是 worker.WorkflowReplayer.ReplayWorkflowHistoryFromJSONFile)。
//
// 选 in-memory 路径不是"两条路都行随便选"——是唯一可行路径。原因:
// ReplayWorkflowHistoryFromJSONFile 内部固定传空 WorkflowExecution{} 起播
// (见 vendored go.temporal.io/sdk@v1.46.0 internal/internal_worker.go
// ReplayPartialWorkflowHistoryFromJSONFile 的实现),没有能设置 OriginalExecution
// 的重载;而 runAttempt 把 workflow.GetInfo(ctx).WorkflowExecution.ID 拼进
// taskID,taskID 又是每个活动入参的一部分,重放时缺了正确的 OriginalExecution
// 就会和 history 记录的入参对不上,报出一个和真正病因毫不相关的
// "TMPRL1100 lookup failed for scheduledEventID to activityID"。
// 只有 in-memory 路径(自己 Unmarshal 再调
// ReplayWorkflowHistoryWithOptions)能传 OriginalExecution,见 replay_test.go。
// 序列化格式本身(protojson vs `temporal workflow show` 导出格式)在这次调查
// 中不是问题——两者对这份 history 都能被 SDK 的 temporalproto 解析器读回;
// 挑标准 protojson 只是因为不需要额外拼 CLI 的导出格式。
func serializeHistory(t *testing.T, hist *historypb.History) []byte {
	t.Helper()
	data, err := protojson.Marshal(hist)
	if err != nil {
		t.Fatalf("序列化 history 失败: %v", err)
	}
	return data
}
