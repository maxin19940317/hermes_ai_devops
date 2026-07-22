package reporter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"hermes-devops/agent/internal/adb"
)

// fakeRuntime 是 httptest 假 Runtime:记录三类回调载荷,按可配状态码
// 应答;task-events 模拟服务端 (idempotency_key, seq) 去重(§4)。
type fakeRuntime struct {
	mu sync.Mutex

	heartbeatStatus int    // 0 → 200
	heartbeatBody   string // 空 → {"ok":true}
	heartbeats      []map[string]any
	heartbeatTimes  []time.Time

	eventStatus int // 0 → 200
	eventCalls  int
	events      []map[string]any
	eventDups   int
	seenEvents  map[string]bool

	resultStatus func(call int) int // nil → 200
	results      []map[string]any
	resultCalls  int
}

func newFakeRuntime(t *testing.T) (*fakeRuntime, *httptest.Server) {
	t.Helper()
	f := &fakeRuntime{seenEvents: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeRuntime) handle(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case pathHeartbeat:
		f.heartbeats = append(f.heartbeats, body)
		f.heartbeatTimes = append(f.heartbeatTimes, time.Now())
		status := f.heartbeatStatus
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		resp := f.heartbeatBody
		if resp == "" {
			resp = `{"ok":true}`
		}
		fmt.Fprint(w, resp)
	case pathTaskEvents:
		f.eventCalls++
		status := f.eventStatus
		if status == 0 {
			status = http.StatusOK
		}
		if status == http.StatusOK {
			// 模拟 Runtime 去重:重复 (idempotency_key, seq) 仍 200 但不重复生效
			key := fmt.Sprintf("%v/%v", body["idempotency_key"], body["seq"])
			if f.seenEvents[key] {
				f.eventDups++
			} else {
				f.seenEvents[key] = true
				f.events = append(f.events, body)
			}
		}
		w.WriteHeader(status)
		if status != http.StatusOK {
			fmt.Fprint(w, `{"code":"invalid","message":"bad event"}`)
		}
	case pathResults:
		f.resultCalls++
		status := http.StatusOK
		if f.resultStatus != nil {
			status = f.resultStatus(f.resultCalls)
		}
		if status == http.StatusOK {
			f.results = append(f.results, body)
		}
		w.WriteHeader(status)
		if status != http.StatusOK {
			fmt.Fprint(w, `{"code":"error","message":"result rejected"}`)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeRuntime) snapshot() (heartbeats, events, results []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any{}, f.heartbeats...),
		append([]map[string]any{}, f.events...),
		append([]map[string]any{}, f.results...)
}

func (f *fakeRuntime) heartbeatAttempts() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time{}, f.heartbeatTimes...)
}

func (f *fakeRuntime) resultCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resultCalls
}

func (f *fakeRuntime) dupEventCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.eventDups
}

func (f *fakeRuntime) eventCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.eventCalls
}

// fakeRunner 按 argv 精确匹配的 fake adb.Runner;未登记的调用报错。
type fakeRunner struct {
	mu        sync.Mutex
	responses map[string]adb.Result
	calls     [][]string
}

func (f *fakeRunner) Run(_ context.Context, args []string) (adb.Result, error) {
	key := strings.Join(args, " ")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{}, args...))
	res, ok := f.responses[key]
	if !ok {
		return adb.Result{}, fmt.Errorf("unexpected adb call: %s", key)
	}
	return res, nil
}
