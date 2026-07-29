package reporter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

var tsPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)

func TestClientPostPathsAndClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		wantErr   bool
		retryable bool
	}{
		{"200 ok", http.StatusOK, false, false},
		{"400 schema_violation 不重试", http.StatusBadRequest, true, false},
		{"422 不重试", http.StatusUnprocessableEntity, true, false},
		{"500 signal_error 重试", http.StatusInternalServerError, true, true},
		{"503 重试", http.StatusServiceUnavailable, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != pathTaskEvents {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			c := &Client{BaseURL: srv.URL}
			err := c.ReportEvent(context.Background(), TaskEvent{
				TaskID: "t1", IdempotencyKey: "wf:t1:a1", Seq: 1,
				From: "QUEUED", To: "PREPARING", Ts: utcNowMs(),
			})
			if tc.wantErr && err == nil {
				t.Fatal("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if err != nil {
				var se *StatusError
				if !errors.As(err, &se) || se.Code != tc.status {
					t.Fatalf("want StatusError{%d}, got %v", tc.status, err)
				}
				if got := Retryable(err); got != tc.retryable {
					t.Errorf("Retryable = %v, want %v", got, tc.retryable)
				}
			}
		})
	}
}

func TestClientNetworkErrorIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // 立即关闭,连接必失败

	c := &Client{BaseURL: srv.URL}
	err := c.ReportEvent(context.Background(), TaskEvent{TaskID: "t1", Seq: 1, Ts: utcNowMs()})
	if err == nil {
		t.Fatal("want network error, got nil")
	}
	if !Retryable(err) {
		t.Errorf("network error should be retryable, got %v", err)
	}
}

func TestHeartbeatAckToleratesOkOnly(t *testing.T) {
	// 契约只保证 ok 字段(Phase 3 字段本轮缺失),且应答可能极简
	for _, body := range []string{`{"ok":true}`, `{}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != pathHeartbeat {
				t.Errorf("unexpected path %s", r.URL.Path)
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(body))
		}))
		c := &Client{BaseURL: srv.URL}
		if _, err := c.Heartbeat(context.Background(), HeartbeatRequest{
			ClientID: "c1", AgentVersion: "0.1.0", Ts: utcNowMs(),
			Devices: []DeviceInfo{}, ActiveTaskIDs: []any{},
		}); err != nil {
			t.Errorf("heartbeat with ack %s: %v", body, err)
		}
		srv.Close()
	}
}

func TestUtcNowMsFormat(t *testing.T) {
	if !tsPattern.MatchString(utcNowMs()) {
		t.Errorf("utcNowMs() = %q, want UTC millisecond ISO-8601", utcNowMs())
	}
}

// 401 表示租约已非己有,调用方必须能区分它与"端点挂了"——前者不回退,后者回退。
func TestRequestUploadsDistinguishesUnauthorized(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantUnauth bool
		wantErr    bool
	}{
		{"200 正常", http.StatusOK, false, false},
		{"401 租约失配", http.StatusUnauthorized, true, true},
		{"503 未配置", http.StatusServiceUnavailable, false, true},
		{"500 服务端错", http.StatusInternalServerError, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				if tc.status == http.StatusOK {
					_, _ = w.Write([]byte(`{"uploads":[],"rejected":[]}`))
				}
			}))
			defer srv.Close()
			c := &Client{BaseURL: srv.URL}
			_, err := c.RequestUploads(context.Background(), srv.URL, UploadRequest{
				TaskID: "t1", ClientID: "c1", DeviceID: "d1", Attempt: 1,
				LeaseID: "l1", LeaseGeneration: 1, Files: []string{"a.log"},
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got := errors.Is(err, ErrLeaseNotOwned); got != tc.wantUnauth {
				t.Errorf("ErrLeaseNotOwned = %v, want %v (err=%v)", got, tc.wantUnauth, err)
			}
		})
	}
}
