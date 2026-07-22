package server

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"hermes-devops/agent/internal/adb"
	"hermes-devops/agent/internal/reporter"
	"hermes-devops/agent/internal/store"
	"hermes-devops/agent/internal/uploader"
)

const (
	testSerial  = "SERIAL123"
	testSerial2 = "SERIAL999"
	runSh       = "#!/system/bin/sh\nexit 0\n"
)

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// buildPackage 构造含 manifest.yaml 的合法测试包(与 executor 测试同款)。
func buildPackage(t *testing.T) []byte {
	t.Helper()
	sum := sha256.Sum256([]byte(runSh))
	manifest := fmt.Sprintf(`manifest_version: 1
artifact: {project: p, commit: deadbee1, pipeline_id: 1, platform: aarch64_Android_SNPE_2.21, build_type: Release}
requirements: {os: android, abi: arm64-v8a, soc: [QCM6125], min_free_storage_mb: 100}
deploy:
  workdir: /data/local/tmp/tst
  files:
    - {src: run.sh, dst: run.sh, mode: "0755", sha256: %s}
  env: {LD_LIBRARY_PATH: "{workdir}/lib"}
test:
  entry: ./run.sh
  args: ["--suite", "s"]
  timeout_sec: 900
  success: {exit_code: 0, require_files: [results/result.json]}
collect: [results/result.json, results/*.json]
cleanup: {remove_workdir: true, keep_on_failure: true}
`, hex.EncodeToString(sum[:]))

	// tar.gz 为二进制,写临时文件后读回
	path := filepath.Join(t.TempDir(), "pkg.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, e := range map[string]struct {
		data string
		mode int64
	}{
		"run.sh":        {runSh, 0o755},
		"manifest.yaml": {manifest, 0o644},
	} {
		hdr := &tar.Header{Name: name, Size: int64(len(e.data)), Mode: e.mode}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.data)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// fakeADB 以 argv 模式匹配模拟设备(覆盖 executor 流水线 + 设备发现 + 诊断)。
type fakeADB struct {
	mu        sync.Mutex
	calls     [][]string
	props     map[string]string
	dfAvailKB int64
	devices   []string // 发现的 serial 列表
	logcatOut string
	block     chan struct{} // 非 nil 时 entry 执行阻塞至关闭或 ctx 取消
}

func newFakeADB() *fakeADB {
	return &fakeADB{
		props: map[string]string{
			"ro.product.cpu.abi":       "arm64-v8a",
			"ro.board.platform":        "qcm6125",
			"ro.build.version.release": "12",
		},
		dfAvailKB: 1 << 20,
		devices:   []string{testSerial, testSerial2},
		logcatOut: "fake logcat content\n",
	}
}

func (f *fakeADB) Run(ctx context.Context, args []string) (adb.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{}, args...))
	block := f.block
	f.mu.Unlock()

	if args[0] == "devices" {
		out := "List of devices attached\n"
		for _, s := range f.devices {
			out += s + " device product:p model:m device:d transport_id:1\n"
		}
		return adb.Result{Stdout: out}, nil
	}

	cmd := args[2] // args[0]=="-s", args[1]==serial
	switch cmd {
	case "push":
		return adb.Result{}, nil
	case "pull":
		dest := args[4]
		os.MkdirAll(filepath.Dir(dest), 0o755)
		os.WriteFile(dest, []byte(`{"result_version":1}`), 0o644)
		return adb.Result{}, nil
	case "logcat":
		if args[3] == "-d" {
			f.mu.Lock()
			out := f.logcatOut
			f.mu.Unlock()
			return adb.Result{Stdout: out}, nil
		}
		return adb.Result{}, nil // -c
	case "shell":
		s := args[3]
		switch {
		case s == "getprop" && len(args) == 5:
			return adb.Result{Stdout: f.props[args[4]] + "\n"}, nil
		case s == "df" && len(args) == 6:
			return adb.Result{Stdout: fmt.Sprintf(
				"Filesystem 1K-blocks Used Available Use%% Mounted on\n/dev/block/dm-0 10000000 100 %d 1%% /data\n",
				f.dfAvailKB)}, nil
		case strings.Contains(s, "ls -1d"):
			return adb.Result{Stdout: "results/result.json\n"}, nil
		case strings.Contains(s, "'./run.sh'"):
			if block != nil {
				select {
				case <-block:
					return adb.Result{Stdout: "suite ok\n"}, nil
				case <-ctx.Done():
					return adb.Result{ExitCode: -1}, ctx.Err()
				}
			}
			return adb.Result{Stdout: "suite ok\n"}, nil
		default: // mkdir/rm/chmod/pkill
			return adb.Result{}, nil
		}
	}
	return adb.Result{}, fmt.Errorf("fakeADB: unexpected argv %v", args)
}

func (f *fakeADB) count(substr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), substr) {
			n++
		}
	}
	return n
}

// fakeRuntime 记录 task-events / results 回调(简化版假 Runtime)。
type fakeRuntime struct {
	mu      sync.Mutex
	events  []map[string]any
	results []map[string]any
}

func newFakeRuntime(t *testing.T) (*fakeRuntime, *httptest.Server) {
	t.Helper()
	f := &fakeRuntime{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/callbacks/v1/task-events":
			f.events = append(f.events, body)
		case "/callbacks/v1/results":
			f.results = append(f.results, body)
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeRuntime) snapshot() (events, results []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any{}, f.events...), append([]map[string]any{}, f.results...)
}

// fakeMinio 记录预签名 PUT 的对象键(URL path 末段)。
type fakeMinio struct {
	mu   sync.Mutex
	keys []string
}

func newFakeMinio(t *testing.T) (*fakeMinio, *httptest.Server) {
	t.Helper()
	f := &fakeMinio{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		f.mu.Lock()
		f.keys = append(f.keys, strings.TrimPrefix(r.URL.Path, "/bucket/"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeMinio) putKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.keys...)
}

// testEnv 是一套 server + 依赖(store/假 Runtime/假 adb)。
type testEnv struct {
	srv     *Server
	handler http.Handler
	st      *store.Store
	runner  *fakeADB
	runtime *fakeRuntime
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	rt, rtSrv := newFakeRuntime(t)
	client := &reporter.Client{BaseURL: rtSrv.URL}
	runner := newFakeADB()
	s := New(Config{
		Store:        st,
		Runner:       runner,
		Events:       &reporter.EventReporter{Store: st, Client: client},
		Results:      &reporter.ResultReporter{Store: st, Client: client, InitialBackoff: time.Millisecond},
		Uploader:     &uploader.Uploader{},
		RunsRoot:     t.TempDir(),
		AgentVersion: "test",
	})
	return &testEnv{srv: s, handler: s.Mux(), st: st, runner: runner, runtime: rt}
}

// dispatchBody 生成合法 dispatch 载荷;pkgURL 为产物包地址。
func dispatchBody(taskID, key, pkgURL, pkgSHA string, extra map[string]any) []byte {
	d := map[string]any{
		"task_id":         taskID,
		"idempotency_key": key,
		"attempt":         1,
		"artifact": map[string]any{
			"url":    pkgURL,
			"sha256": pkgSHA,
			"auth":   map[string]any{"type": "bearer", "token": "tok"},
		},
		"manifest_digest":   strings.Repeat("b", 64),
		"device_serial":     testSerial,
		"callback_base_url": "http://runtime:18091",
	}
	for k, v := range extra {
		d[k] = v
	}
	body, _ := json.Marshal(d)
	return body
}

func doReq(t *testing.T, h http.Handler, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body != nil {
		rdr = strings.NewReader(string(body))
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rdr)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// waitFor 轮询直至条件满足(5s 超时)。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// unblock 解除 fakeADB 的执行阻塞(幂等,测试主体与 Cleanup 都可调用)。
func unblock(f *fakeADB) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.block != nil {
		close(f.block)
		f.block = nil
	}
}

// waitState 轮询 store 直至任务到达目标状态(5s 超时)。
func waitState(t *testing.T, st *store.Store, taskID string, want store.State) store.Task {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, err := st.GetTask(context.Background(), taskID)
		if err == nil && task.State == want {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _ := st.GetTask(context.Background(), taskID)
	t.Fatalf("task %s 未到达 %s(当前 %s)", taskID, want, task.State)
	return store.Task{}
}

// pkgServer 伺服测试包并统计下载次数(执行次数判据)。
type pkgServer struct {
	srv       *httptest.Server
	sha       string
	mu        sync.Mutex
	downloads int
}

func newPkgServer(t *testing.T) *pkgServer {
	t.Helper()
	data := buildPackage(t)
	p := &pkgServer{sha: sha256hex(data)}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		p.downloads++
		p.mu.Unlock()
		w.Write(data)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *pkgServer) downloadCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.downloads
}

func decodeResponse(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
}

// ---- 端到端:派单 → 202 → 异步执行 → 事件/结果回调 → 附件直传 ----

func TestDispatchEndToEnd(t *testing.T) {
	env := newTestEnv(t)
	pkg := newPkgServer(t)
	minio, minioSrv := newFakeMinio(t)

	uploads := []any{}
	for _, name := range []string{"result.json", "junit.xml", "logcat.txt", "stdout.log", "stderr.log"} {
		uploads = append(uploads, map[string]any{
			"object_key": "runs/t-e2e/" + name,
			"url":        minioSrv.URL + "/bucket/runs/t-e2e/" + name + "?sig=x",
		})
	}
	body := dispatchBody("t-e2e", "wf:t-e2e:a1", pkg.srv.URL, pkg.sha, map[string]any{"presigned_uploads": uploads})

	rec := doReq(t, env.handler, http.MethodPost, "/api/v1/tasks", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch = %d, want 202: %s", rec.Code, rec.Body)
	}
	var st TaskStatus
	decodeResponse(t, rec, &st)
	if st.TaskID != "t-e2e" || st.State != string(store.StateQueued) || st.Attempt != 1 {
		t.Errorf("dispatch response = %+v", st)
	}

	waitState(t, env.st, "t-e2e", store.StateCompleted)
	// 终态落盘先于即发回调返回,结果上报又在终态事件之后——等结果到齐再断言
	waitFor(t, "result reported", func() bool {
		_, results := env.runtime.snapshot()
		return len(results) == 1
	})

	// 事件链:首事件 QUEUED→ACCEPTED,终事件 →COMPLETED,seq 从 1 单调递增
	events, results := env.runtime.snapshot()
	if len(events) < 6 {
		t.Fatalf("events = %d, 至少 6 条(ACCEPTED→…→COMPLETED): %v", len(events), events)
	}
	if events[0]["from"] != "QUEUED" || events[0]["to"] != "ACCEPTED" {
		t.Errorf("首事件 = %v→%v, want QUEUED→ACCEPTED", events[0]["from"], events[0]["to"])
	}
	last := events[len(events)-1]
	if last["to"] != "COMPLETED" {
		t.Errorf("终事件 to = %v, want COMPLETED", last["to"])
	}
	for i, ev := range events {
		if ev["seq"].(float64) != float64(i+1) {
			t.Errorf("events[%d].seq = %v, want %d", i, ev["seq"], i+1)
		}
	}

	// 结果回流:status COMPLETED,附件含直传成功的对象键
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	res := results[0]["result"].(map[string]any)
	if res["status"] != "COMPLETED" {
		t.Errorf("result.status = %v", res["status"])
	}
	atts, _ := res["attachments"].([]any)
	gotKeys := map[string]bool{}
	for _, a := range atts {
		gotKeys[a.(map[string]any)["object_key"].(string)] = true
	}
	// junit.xml 任务不产出 → 跳过;其余固定键集应已直传
	for _, name := range []string{"result.json", "logcat.txt", "stdout.log", "stderr.log"} {
		key := "runs/t-e2e/" + name
		if !gotKeys[key] {
			t.Errorf("attachments 缺少 %s(got %v)", key, gotKeys)
		}
	}
	if gotKeys["runs/t-e2e/junit.xml"] {
		t.Error("junit.xml 未产出,不应出现在 attachments")
	}
	puts := minio.putKeys()
	if len(puts) != 4 {
		t.Errorf("minio PUT = %v, want 4 个对象", puts)
	}

	// GET 现状:终态 + updated_at
	rec = doReq(t, env.handler, http.MethodGet, "/api/v1/tasks/t-e2e", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", rec.Code, rec.Body)
	}
	decodeResponse(t, rec, &st)
	if st.State != "COMPLETED" || st.UpdatedAt == "" {
		t.Errorf("GET status = %+v", st)
	}
}

// ---- 幂等:同幂等键重复派单返回现状,且只执行一次 ----

func TestDuplicateIdempotencyKeyExecutesOnce(t *testing.T) {
	env := newTestEnv(t)
	env.runner.block = make(chan struct{})
	t.Cleanup(func() { unblock(env.runner) })
	pkg := newPkgServer(t)
	body := dispatchBody("t-dup", "wf:t-dup:a1", pkg.srv.URL, pkg.sha, nil)

	if rec := doReq(t, env.handler, http.MethodPost, "/api/v1/tasks", body); rec.Code != http.StatusAccepted {
		t.Fatalf("first dispatch = %d: %s", rec.Code, rec.Body)
	}
	waitState(t, env.st, "t-dup", store.StateRunning)

	// 重复派单:同幂等键 → 202 + 当前状态(RUNNING),不重复执行
	rec := doReq(t, env.handler, http.MethodPost, "/api/v1/tasks", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("dup dispatch = %d: %s", rec.Code, rec.Body)
	}
	var st TaskStatus
	decodeResponse(t, rec, &st)
	if st.State != string(store.StateRunning) {
		t.Errorf("dup dispatch state = %s, want RUNNING(现状)", st.State)
	}

	unblock(env.runner)
	waitState(t, env.st, "t-dup", store.StateCompleted)
	if n := pkg.downloadCount(); n != 1 {
		t.Errorf("包下载次数 = %d, want 1(恰好一次执行)", n)
	}
}

// ---- 冲突:同 task_id 异幂等键 → 409 ----

func TestTaskIDConflict409(t *testing.T) {
	env := newTestEnv(t)
	pkg := newPkgServer(t)

	body1 := dispatchBody("t-conflict", "wf:t-conflict:a1", pkg.srv.URL, pkg.sha, nil)
	if rec := doReq(t, env.handler, http.MethodPost, "/api/v1/tasks", body1); rec.Code != http.StatusAccepted {
		t.Fatalf("first dispatch = %d: %s", rec.Code, rec.Body)
	}
	body2 := dispatchBody("t-conflict", "wf:t-conflict:a2", pkg.srv.URL, pkg.sha, nil)
	rec := doReq(t, env.handler, http.MethodPost, "/api/v1/tasks", body2)
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflict dispatch = %d, want 409: %s", rec.Code, rec.Body)
	}
	var e Error
	decodeResponse(t, rec, &e)
	if e.Code == "" || e.Message == "" {
		t.Errorf("409 响应必须是 {code,message}: %+v", e)
	}
	waitState(t, env.st, "t-conflict", store.StateCompleted)
}

// ---- 400:请求体不过 Schema ----

func TestDispatchBadBody400(t *testing.T) {
	env := newTestEnv(t)
	pkg := newPkgServer(t)

	cases := map[string][]byte{
		"非 JSON": []byte("{nope"),
	}
	// 缺 artifact:直接删掉字段
	d := map[string]any{}
	_ = json.Unmarshal(dispatchBody("t-bad", "wf:t-bad:a1", pkg.srv.URL, pkg.sha, nil), &d)
	delete(d, "artifact")
	cases["缺 artifact"], _ = json.Marshal(d)

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := doReq(t, env.handler, http.MethodPost, "/api/v1/tasks", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("= %d, want 400: %s", rec.Code, rec.Body)
			}
			var e Error
			decodeResponse(t, rec, &e)
			if e.Code == "" || e.Message == "" {
				t.Errorf("400 响应必须是 {code,message}: %+v", e)
			}
		})
	}
}

// ---- GET / DELETE 404 ----

func TestGetDeleteUnknownTask404(t *testing.T) {
	env := newTestEnv(t)
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		rec := doReq(t, env.handler, method, "/api/v1/tasks/nope", nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404: %s", method, rec.Code, rec.Body)
		}
		var e Error
		decodeResponse(t, rec, &e)
		if e.Code != "task_not_found" {
			t.Errorf("%s 404 code = %q", method, e.Code)
		}
	}
}

// ---- DELETE 取消运行中任务:RUNNING → CANCELED 终态 ----

func TestDeleteCancelsRunningTask(t *testing.T) {
	env := newTestEnv(t)
	env.runner.block = make(chan struct{}) // 永不主动放行,只靠 Cancel 解除
	t.Cleanup(func() { unblock(env.runner) })
	pkg := newPkgServer(t)
	body := dispatchBody("t-cancel", "wf:t-cancel:a1", pkg.srv.URL, pkg.sha, nil)

	if rec := doReq(t, env.handler, http.MethodPost, "/api/v1/tasks", body); rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch = %d: %s", rec.Code, rec.Body)
	}
	waitState(t, env.st, "t-cancel", store.StateRunning)

	rec := doReq(t, env.handler, http.MethodDelete, "/api/v1/tasks/t-cancel", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("DELETE = %d, want 202: %s", rec.Code, rec.Body)
	}

	waitState(t, env.st, "t-cancel", store.StateCanceled)
	waitFor(t, "result reported", func() bool {
		_, results := env.runtime.snapshot()
		return len(results) == 1
	})
	// pkill 必须到达设备(取消复用超时 kill 路径)
	if n := env.runner.count("pkill"); n == 0 {
		t.Error("取消后应 kill 设备进程(pkill)")
	}
	// 终态以回调为准:事件含 →CANCELED,结果 status CANCELED
	events, results := env.runtime.snapshot()
	foundCancel := false
	for _, ev := range events {
		if ev["to"] == "CANCELED" {
			foundCancel = true
		}
	}
	if !foundCancel {
		t.Errorf("事件流缺少 →CANCELED: %v", events)
	}
	if len(results) != 1 || results[0]["result"].(map[string]any)["status"] != "CANCELED" {
		t.Errorf("结果应为 CANCELED: %v", results)
	}
}

// ---- DELETE 已终态任务:幂等 202 返回现状 ----

func TestDeleteTerminalTaskIsIdempotent202(t *testing.T) {
	env := newTestEnv(t)
	pkg := newPkgServer(t)
	body := dispatchBody("t-term", "wf:t-term:a1", pkg.srv.URL, pkg.sha, nil)
	doReq(t, env.handler, http.MethodPost, "/api/v1/tasks", body)
	waitState(t, env.st, "t-term", store.StateCompleted)

	rec := doReq(t, env.handler, http.MethodDelete, "/api/v1/tasks/t-term", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("DELETE terminal = %d, want 202: %s", rec.Code, rec.Body)
	}
	var st TaskStatus
	decodeResponse(t, rec, &st)
	if st.State != "COMPLETED" {
		t.Errorf("DELETE terminal state = %s, want COMPLETED", st.State)
	}
}

// ---- 设备清单:BUSY 标记 ----

func TestListDevices(t *testing.T) {
	env := newTestEnv(t)
	pkg := newPkgServer(t)

	// 空闲时两台都是 IDLE,属性/空间齐全
	rec := doReq(t, env.handler, http.MethodGet, "/api/v1/devices", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /devices = %d: %s", rec.Code, rec.Body)
	}
	var devs []reporter.DeviceInfo
	decodeResponse(t, rec, &devs)
	if len(devs) != 2 {
		t.Fatalf("devices = %d, want 2: %v", len(devs), devs)
	}
	for _, d := range devs {
		if d.State != reporter.DeviceIdle {
			t.Errorf("%s state = %s, want IDLE", d.Serial, d.State)
		}
		if d.Props == nil || d.Props.ABI != "arm64-v8a" || d.Props.SOC != "qcm6125" || d.Props.Android != "12" {
			t.Errorf("%s props = %+v", d.Serial, d.Props)
		}
		if d.WorkdirFreeMB == nil || *d.WorkdirFreeMB <= 0 {
			t.Errorf("%s workdir_free_mb = %v", d.Serial, d.WorkdirFreeMB)
		}
	}

	// 派单占用 testSerial 后:该台 BUSY,另一台仍 IDLE
	env.runner.block = make(chan struct{})
	t.Cleanup(func() { unblock(env.runner) })
	body := dispatchBody("t-busy", "wf:t-busy:a1", pkg.srv.URL, pkg.sha, nil)
	doReq(t, env.handler, http.MethodPost, "/api/v1/tasks", body)
	waitState(t, env.st, "t-busy", store.StateRunning)

	rec = doReq(t, env.handler, http.MethodGet, "/api/v1/devices", nil)
	decodeResponse(t, rec, &devs)
	states := map[string]reporter.DeviceState{}
	for _, d := range devs {
		states[d.Serial] = d.State
	}
	if states[testSerial] != reporter.DeviceBusy {
		t.Errorf("%s state = %s, want BUSY", testSerial, states[testSerial])
	}
	if states[testSerial2] != reporter.DeviceIdle {
		t.Errorf("%s state = %s, want IDLE", testSerial2, states[testSerial2])
	}
}

// ---- 诊断:四探测 + 拒绝 + 截断 ----

func TestDiagnostics(t *testing.T) {
	env := newTestEnv(t)

	t.Run("adb_devices", func(t *testing.T) {
		rec := doReq(t, env.handler, http.MethodPost, "/api/v1/diagnostics",
			[]byte(`{"probe":"adb_devices"}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("= %d: %s", rec.Code, rec.Body)
		}
		var resp DiagnosticsResponse
		decodeResponse(t, rec, &resp)
		if !strings.Contains(resp.Output, testSerial) || resp.Truncated {
			t.Errorf("resp = %+v", resp)
		}
	})

	t.Run("adb_devices 带参数拒绝", func(t *testing.T) {
		rec := doReq(t, env.handler, http.MethodPost, "/api/v1/diagnostics",
			[]byte(`{"probe":"adb_devices","args":{"serial":"`+testSerial+`"}}`))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("= %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("logcat_tail", func(t *testing.T) {
		rec := doReq(t, env.handler, http.MethodPost, "/api/v1/diagnostics",
			[]byte(`{"probe":"logcat_tail","args":{"serial":"`+testSerial+`","lines":50}}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("= %d: %s", rec.Code, rec.Body)
		}
		var resp DiagnosticsResponse
		decodeResponse(t, rec, &resp)
		if !strings.Contains(resp.Output, "fake logcat") {
			t.Errorf("output = %q", resp.Output)
		}
		if env.runner.count("-t 50") == 0 {
			t.Error("应调用 adb logcat -t 50")
		}
	})

	t.Run("logcat_tail lines 越界", func(t *testing.T) {
		rec := doReq(t, env.handler, http.MethodPost, "/api/v1/diagnostics",
			[]byte(`{"probe":"logcat_tail","args":{"serial":"`+testSerial+`","lines":1001}}`))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("= %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("df", func(t *testing.T) {
		rec := doReq(t, env.handler, http.MethodPost, "/api/v1/diagnostics",
			[]byte(`{"probe":"df","args":{"serial":"`+testSerial+`"}}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("= %d: %s", rec.Code, rec.Body)
		}
		var resp DiagnosticsResponse
		decodeResponse(t, rec, &resp)
		if !strings.Contains(resp.Output, "Available") {
			t.Errorf("output = %q", resp.Output)
		}
	})

	t.Run("getprop", func(t *testing.T) {
		rec := doReq(t, env.handler, http.MethodPost, "/api/v1/diagnostics",
			[]byte(`{"probe":"getprop","args":{"serial":"`+testSerial+`","prop_name":"ro.product.cpu.abi"}}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("= %d: %s", rec.Code, rec.Body)
		}
		var resp DiagnosticsResponse
		decodeResponse(t, rec, &resp)
		if !strings.Contains(resp.Output, "arm64-v8a") {
			t.Errorf("output = %q", resp.Output)
		}
	})

	t.Run("getprop 属性名白名单", func(t *testing.T) {
		rec := doReq(t, env.handler, http.MethodPost, "/api/v1/diagnostics",
			[]byte(`{"probe":"getprop","args":{"serial":"`+testSerial+`","prop_name":"x;rm -rf /"}}`))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("= %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("未知探测拒绝", func(t *testing.T) {
		rec := doReq(t, env.handler, http.MethodPost, "/api/v1/diagnostics",
			[]byte(`{"probe":"shell","args":{"serial":"`+testSerial+`"}}`))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("= %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("未知参数键拒绝", func(t *testing.T) {
		rec := doReq(t, env.handler, http.MethodPost, "/api/v1/diagnostics",
			[]byte(`{"probe":"df","args":{"serial":"`+testSerial+`","cmd":"rm -rf /"}}`))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("= %d, want 400: %s", rec.Code, rec.Body)
		}
	})

	t.Run("输出截断", func(t *testing.T) {
		env.runner.mu.Lock()
		env.runner.logcatOut = strings.Repeat("x", 100000) + "\n"
		env.runner.mu.Unlock()
		env.srv.cfg.DiagnosticsMaxBytes = 1024
		rec := doReq(t, env.handler, http.MethodPost, "/api/v1/diagnostics",
			[]byte(`{"probe":"logcat_tail","args":{"serial":"`+testSerial+`"}}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("= %d: %s", rec.Code, rec.Body)
		}
		var resp DiagnosticsResponse
		decodeResponse(t, rec, &resp)
		if !resp.Truncated || len(resp.Output) > 1024 {
			t.Errorf("truncated=%v len=%d, want truncated 且 ≤1024", resp.Truncated, len(resp.Output))
		}
	})
}

// ---- healthz ----

func TestHealthz(t *testing.T) {
	env := newTestEnv(t)
	rec := doReq(t, env.handler, http.MethodGet, "/healthz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("= %d: %s", rec.Code, rec.Body)
	}
	var body map[string]any
	decodeResponse(t, rec, &body)
	if body["status"] != "ok" || body["agent_version"] != "test" || body["adb_server_port"].(float64) != 5137 {
		t.Errorf("healthz = %v", body)
	}
}
