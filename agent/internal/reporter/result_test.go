package reporter

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"hermes-devops/agent/internal/executor"
	"hermes-devops/agent/internal/store"
)

const contractsDir = "../../../contracts"

// TestEmbeddedSchemaMatchesContract 防契约漂移:嵌入副本必须与
// contracts/result.schema.json 逐字节一致(改契约后 cp 同步)。
func TestEmbeddedSchemaMatchesContract(t *testing.T) {
	want, err := os.ReadFile(filepath.Join(contractsDir, "result.schema.json"))
	if err != nil {
		t.Fatalf("read contracts schema: %v", err)
	}
	if !bytes.Equal(EmbeddedResultSchema, want) {
		t.Fatal("embedded result.schema.json 与 contracts/ 不一致,请重新拷贝(防契约漂移)")
	}
}

// compileRealResultSchema 从 contracts/ 编译"真"契约(不经嵌入副本),
// 用于校验实际上送的 result 载荷。
func compileRealResultSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(contractsDir, "result.schema.json"))
	if err != nil {
		t.Fatalf("read contracts schema: %v", err)
	}
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2020
	if err := c.AddResource("result.schema.json", bytes.NewReader(raw)); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	sch, err := c.Compile("result.schema.json")
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}
	return sch
}

func TestValidateResult(t *testing.T) {
	cases := []struct {
		name    string
		doc     string
		wantErr bool
	}{
		{"最小合法", `{"result_version":1,"task_id":"t","attempt":1,"status":"COMPLETED","exit_code":0,"duration_sec":1.2,"cases":{"total":1,"passed":1,"failed":0,"skipped":0}}`, false},
		{"未知字段宽容", `{"result_version":1,"task_id":"t","attempt":1,"status":"FAILED","exit_code":2,"duration_sec":0,"cases":{"total":0,"passed":0,"failed":0,"skipped":0},"extra_field":"x"}`, false},
		{"缺 cases", `{"result_version":1,"task_id":"t","attempt":1,"status":"COMPLETED","exit_code":0,"duration_sec":1}`, true},
		{"result_version 非 1", `{"result_version":2,"task_id":"t","attempt":1,"status":"COMPLETED","exit_code":0,"duration_sec":1,"cases":{"total":0,"passed":0,"failed":0,"skipped":0}}`, true},
		{"非终态 status", `{"result_version":1,"task_id":"t","attempt":1,"status":"RUNNING","exit_code":0,"duration_sec":1,"cases":{"total":0,"passed":0,"failed":0,"skipped":0}}`, true},
		{"附件 sha256 非法", `{"result_version":1,"task_id":"t","attempt":1,"status":"COMPLETED","exit_code":0,"duration_sec":1,"cases":{"total":0,"passed":0,"failed":0,"skipped":0},"attachments":[{"name":"a","object_key":"k","sha256":"xyz","size":1}]}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateResult([]byte(tc.doc))
			if tc.wantErr && err == nil {
				t.Error("want validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected validation error: %v", err)
			}
		})
	}
}

// seedTerminalTask 建好一个 COMPLETED 终态任务并写 run-summary.json。
func seedTerminalTask(t *testing.T, s *store.Store, taskID, key string, criteriaMet bool) store.Task {
	t.Helper()
	task := seedTask(t, s, taskID, key, "SERIAL1")
	driveTerminal(t, s, taskID, store.StateCompleted)
	writeSummary(t, executor.Summary{
		Status:             executor.StatusCompleted,
		ExitCode:           0,
		DurationSec:        1.5,
		SuccessCriteriaMet: criteriaMet,
		Collected:          []string{"results/result.json"},
		Environment:        map[string]string{"serial": "SERIAL1", "abi": "arm64-v8a", "android": "13", "soc": "trinket"},
		OutDir:             task.OutDir,
	})
	return task
}

func newResultReporter(s *store.Store, baseURL string) *ResultReporter {
	return &ResultReporter{
		Store:          s,
		Client:         &Client{BaseURL: baseURL},
		InitialBackoff: time.Millisecond,
		MaxBackoff:     5 * time.Millisecond,
	}
}

func testAttachments() []Attachment {
	return []Attachment{
		{Name: "result.json", ObjectKey: "runs/t1/result.json", SHA256: testSHA256, Size: 123},
		{Name: "logcat.txt", ObjectKey: "runs/t1/logcat.txt", SHA256: testSHA256, Size: 4567},
	}
}

func TestReportAssemblesValidResult(t *testing.T) {
	f, srv := newFakeRuntime(t)
	s := openTempStore(t)
	// dispatch 带 artifact 溯源信息(前向兼容字段)
	dispatch := `{"task_id":"t1","device_serial":"SERIAL1","artifact":{"url":"http://gl/pkg.tar.gz","sha256":"` + testSHA256 + `","auth":{"type":"bearer","token":"x"},"commit":"abc1234","pipeline_id":42}}`
	task := store.Task{TaskID: "t1", IdempotencyKey: "wf1:t1:a1", Attempt: 2, DispatchJSON: dispatch, OutDir: t.TempDir()}
	if err := s.CreateTask(context.Background(), task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	driveTerminal(t, s, "t1", store.StateCompleted)
	writeSummary(t, executor.Summary{
		Status:             executor.StatusCompleted,
		ExitCode:           0,
		DurationSec:        1.5,
		SuccessCriteriaMet: true,
		Environment:        map[string]string{"serial": "SERIAL1", "abi": "arm64-v8a", "android": "13", "soc": "trinket"},
		OutDir:             task.OutDir,
	})
	// 设备端 result.json:提供 cases/signatures/metrics;task_id 是设备侧
	// 自填的,必须以 store 为准覆盖
	writeDeviceResult(t, task.OutDir, `{
	  "result_version": 1, "task_id": "dev-side", "attempt": 1,
	  "status": "COMPLETED", "exit_code": 0, "duration_sec": 1,
	  "cases": {"total": 3, "passed": 2, "failed": 1, "skipped": 0,
	            "failures": [{"name": "case3", "message": "boom"}]},
	  "signatures_hit": ["smoke_fail_marker"],
	  "metrics": {"fps": 59.8}
	}`)

	rr := newResultReporter(s, srv.URL)
	if err := rr.Report(context.Background(), "t1", testAttachments()); err != nil {
		t.Fatalf("Report: %v", err)
	}

	_, _, results := f.snapshot()
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	rep := results[0]
	if rep["task_id"] != "t1" || rep["idempotency_key"] != "wf1:t1:a1" {
		t.Errorf("report identity = %v", rep)
	}
	result, _ := rep["result"].(map[string]any)

	// 关键:上送的 result 必须过 contracts/result.schema.json(真契约)
	if err := compileRealResultSchema(t).Validate(result); err != nil {
		t.Fatalf("上送 result 未过契约校验: %v", err)
	}

	if result["task_id"] != "t1" {
		t.Errorf("task_id = %v, want t1 (覆盖设备侧 dev-side)", result["task_id"])
	}
	if result["attempt"] != float64(2) {
		t.Errorf("attempt = %v, want 2", result["attempt"])
	}
	if result["status"] != "COMPLETED" || result["exit_code"] != float64(0) || result["duration_sec"] != 1.5 {
		t.Errorf("status/exit_code/duration = %v/%v/%v", result["status"], result["exit_code"], result["duration_sec"])
	}
	cases, _ := result["cases"].(map[string]any)
	if cases["total"] != float64(3) || cases["passed"] != float64(2) || cases["failed"] != float64(1) {
		t.Errorf("cases = %v, want 设备端计数 3/2/1", cases)
	}
	failures, _ := cases["failures"].([]any)
	if len(failures) != 1 || failures[0].(map[string]any)["name"] != "case3" {
		t.Errorf("failures = %v", failures)
	}
	if sigs, _ := result["signatures_hit"].([]any); len(sigs) != 1 || sigs[0] != "smoke_fail_marker" {
		t.Errorf("signatures_hit = %v", result["signatures_hit"])
	}
	if metrics, _ := result["metrics"].(map[string]any); metrics["fps"] != 59.8 {
		t.Errorf("metrics = %v", result["metrics"])
	}
	env, _ := result["environment"].(map[string]any)
	if env["soc"] != "trinket" || env["serial"] != "SERIAL1" {
		t.Errorf("environment = %v", env)
	}
	artifact, _ := result["artifact"].(map[string]any)
	if artifact["commit"] != "abc1234" || artifact["pipeline_id"] != float64(42) {
		t.Errorf("artifact = %v, want dispatch 溯源信息", artifact)
	}
	atts, _ := result["attachments"].([]any)
	if len(atts) != 2 || atts[0].(map[string]any)["object_key"] != "runs/t1/result.json" {
		t.Errorf("attachments = %v", atts)
	}

	recorded, err := s.ResultRecorded(context.Background(), "t1")
	if err != nil || !recorded {
		t.Errorf("ResultRecorded = %v, %v; want true", recorded, err)
	}

	// 重复上报:已记录则跳过(§4 去重)
	if err := rr.Report(context.Background(), "t1", testAttachments()); err != nil {
		t.Fatalf("re-Report: %v", err)
	}
	if n := f.resultCallCount(); n != 1 {
		t.Errorf("result calls = %d, want 1 (重复上报被本地去重)", n)
	}
}

func TestReportSynthesizesCases(t *testing.T) {
	cases := []struct {
		name        string
		criteriaMet bool
		wantPassed  float64
		wantFailed  float64
	}{
		{"判据满足 passed=1", true, 1, 0},
		{"判据不满足 failed=1", false, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newFakeRuntime(t)
			s := openTempStore(t)
			seedTerminalTask(t, s, "t1", "wf1:t1:a1", tc.criteriaMet) // 不写设备 result.json

			rr := newResultReporter(s, srv.URL)
			if err := rr.Report(context.Background(), "t1", nil); err != nil {
				t.Fatalf("Report: %v", err)
			}
			_, _, results := f.snapshot()
			result, _ := results[0]["result"].(map[string]any)
			if err := compileRealResultSchema(t).Validate(result); err != nil {
				t.Fatalf("合成 cases 的 result 未过契约校验: %v", err)
			}
			cs, _ := result["cases"].(map[string]any)
			if cs["total"] != float64(1) || cs["passed"] != tc.wantPassed || cs["failed"] != tc.wantFailed || cs["skipped"] != float64(0) {
				t.Errorf("synthesized cases = %v", cs)
			}
			// 无设备数据:signatures_hit=[]、metrics={}、attachments=[] 必须
			// 是数组/对象而非 null(契约类型严格)
			if sigs, ok := result["signatures_hit"].([]any); !ok || len(sigs) != 0 {
				t.Errorf("signatures_hit = %v (%T), want []", result["signatures_hit"], result["signatures_hit"])
			}
			if m, ok := result["metrics"].(map[string]any); !ok || len(m) != 0 {
				t.Errorf("metrics = %v (%T), want {}", result["metrics"], result["metrics"])
			}
			if atts, ok := result["attachments"].([]any); !ok || len(atts) != 0 {
				t.Errorf("attachments = %v (%T), want []", result["attachments"], result["attachments"])
			}
		})
	}
}

func TestReport400NotRetried(t *testing.T) {
	f, srv := newFakeRuntime(t)
	f.resultStatus = func(int) int { return 400 }
	s := openTempStore(t)
	seedTerminalTask(t, s, "t1", "wf1:t1:a1", true)

	rr := newResultReporter(s, srv.URL)
	err := rr.Report(context.Background(), "t1", testAttachments())
	if err == nil {
		t.Fatal("want error on 400, got nil")
	}
	if Retryable(err) {
		t.Errorf("400 不应可重试: %v", err)
	}
	if n := f.resultCallCount(); n != 1 {
		t.Errorf("result calls = %d, want 1 (400 不重发)", n)
	}
	if recorded, _ := s.ResultRecorded(context.Background(), "t1"); recorded {
		t.Error("400 后不应标记 ResultRecorded")
	}
}

func TestReport500RetriedUntilSuccess(t *testing.T) {
	f, srv := newFakeRuntime(t)
	f.resultStatus = func(call int) int {
		if call <= 2 {
			return 500 // signal_error:重发安全(dedup 保证)
		}
		return 200
	}
	s := openTempStore(t)
	seedTerminalTask(t, s, "t1", "wf1:t1:a1", true)

	rr := newResultReporter(s, srv.URL)
	if err := rr.Report(context.Background(), "t1", testAttachments()); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if n := f.resultCallCount(); n != 3 {
		t.Errorf("result calls = %d, want 3 (500×2 后成功)", n)
	}
	if recorded, _ := s.ResultRecorded(context.Background(), "t1"); !recorded {
		t.Error("成功后应标记 ResultRecorded")
	}
}

func TestReport500ExhaustsAttempts(t *testing.T) {
	f, srv := newFakeRuntime(t)
	f.resultStatus = func(int) int { return 500 }
	s := openTempStore(t)
	seedTerminalTask(t, s, "t1", "wf1:t1:a1", true)

	rr := newResultReporter(s, srv.URL)
	rr.MaxAttempts = 3
	if err := rr.Report(context.Background(), "t1", nil); err == nil {
		t.Fatal("want error after exhausting attempts")
	}
	if n := f.resultCallCount(); n != 3 {
		t.Errorf("result calls = %d, want 3", n)
	}
	if recorded, _ := s.ResultRecorded(context.Background(), "t1"); recorded {
		t.Error("未成功不应标记 ResultRecorded(留待恢复补报)")
	}
}

func TestReportBlocksInvalidResultBeforeSend(t *testing.T) {
	f, srv := newFakeRuntime(t)
	s := openTempStore(t)
	// task_id 超长(契约 maxLength 128):组装结果必不过 Schema
	longID := strings.Repeat("x", 200)
	seedTerminalTask(t, s, longID, "wf1:"+longID+":a1", true)

	rr := newResultReporter(s, srv.URL)
	err := rr.Report(context.Background(), longID, testAttachments())
	if err == nil {
		t.Fatal("want schema validation error, got nil")
	}
	if n := f.resultCallCount(); n != 0 {
		t.Errorf("result calls = %d, want 0 (本地校验不过绝不发送)", n)
	}
}

func TestReportRejectsNonTerminalTask(t *testing.T) {
	_, srv := newFakeRuntime(t)
	s := openTempStore(t)
	seedTask(t, s, "t1", "wf1:t1:a1", "SERIAL1") // 仍 QUEUED

	rr := newResultReporter(s, srv.URL)
	if err := rr.Report(context.Background(), "t1", nil); err == nil {
		t.Fatal("want error for non-terminal task")
	}
}

// TestRecoverAfterCrash 崩溃恢复:进程中 SQLite 落盘了终态任务与未上报
// 事件,"重启"(关闭重开同一库文件)后 RecoverPending 补报结果、drain
// 按序补报事件。
func TestRecoverAfterCrash(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "agent.db")
	ctx := context.Background()

	// ---- 崩溃前:任务跑到终态,事件/结果均未上报 ----
	s1, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedTerminalTask(t, s1, "t1", "wf1:t1:a1", false)
	if err := s1.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// ---- 重启后 ----
	f, srv := newFakeRuntime(t)
	s2 := openStoreAt(t, dbPath)

	inf, err := s2.LoadInflight(ctx)
	if err != nil {
		t.Fatalf("LoadInflight: %v", err)
	}
	if len(inf.PendingResults) != 1 || inf.PendingResults[0].TaskID != "t1" {
		t.Fatalf("PendingResults = %+v, want [t1]", inf.PendingResults)
	}
	if len(inf.UnreportedEvents) != 4 {
		t.Fatalf("UnreportedEvents = %d, want 4", len(inf.UnreportedEvents))
	}

	rr := newResultReporter(s2, srv.URL)
	if err := rr.RecoverPending(ctx); err != nil {
		t.Fatalf("RecoverPending: %v", err)
	}
	er := newEventReporter(s2, srv.URL)
	er.drain(ctx)

	_, events, results := f.snapshot()
	if len(results) != 1 {
		t.Fatalf("补报结果 = %d, want 1", len(results))
	}
	result, _ := results[0]["result"].(map[string]any)
	if err := compileRealResultSchema(t).Validate(result); err != nil {
		t.Fatalf("补报 result 未过契约校验: %v", err)
	}
	if result["status"] != "COMPLETED" {
		t.Errorf("recovered status = %v", result["status"])
	}
	if len(events) != 4 {
		t.Fatalf("补报事件 = %d, want 4", len(events))
	}
	for i, ev := range events {
		if ev["seq"] != float64(i+1) || ev["task_id"] != "t1" {
			t.Errorf("events[%d] = %v seq=%v, want t1 seq=%d", i, ev["task_id"], ev["seq"], i+1)
		}
	}

	// 恢复完成后:无未上报事件,结果已记录,再次恢复为空操作
	recorded, _ := s2.ResultRecorded(ctx, "t1")
	if !recorded {
		t.Error("恢复后应标记 ResultRecorded")
	}
	unreported, _ := s2.UnreportedEvents(ctx)
	if len(unreported) != 0 {
		t.Errorf("恢复后仍有未上报事件: %v", unreported)
	}
	if err := rr.RecoverPending(ctx); err != nil {
		t.Fatalf("二次 RecoverPending: %v", err)
	}
	if n := f.resultCallCount(); n != 1 {
		t.Errorf("result calls = %d, want 1 (二次恢复无重投)", n)
	}
}
