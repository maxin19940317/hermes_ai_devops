package larkws

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	sdkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

type discardLogger struct{}

func (discardLogger) Debug(context.Context, ...interface{}) {}
func (discardLogger) Info(context.Context, ...interface{})  {}
func (discardLogger) Warn(context.Context, ...interface{})  {}
func (discardLogger) Error(context.Context, ...interface{}) {}

type testSocket struct {
	conn *websocket.Conn
	done chan struct{}
	once sync.Once
}

func (s *testSocket) stop() {
	s.once.Do(func() {
		_ = s.conn.Close()
		close(s.done)
	})
}

type transportHarness struct {
	server *httptest.Server

	accepted        chan *testSocket
	handlerErrors   chan error
	bootstrapCount  atomic.Int64
	mu              sync.Mutex
	sockets         []*testSocket
	endpointForCall func(int64) string
	configForCall   func(int64) *sdkws.ClientConfig
	expectedSource  string
	expectedHeader  string
	expectedAppID   string
	expectedSecret  string
}

func newTransportHarness(t *testing.T) *transportHarness {
	t.Helper()
	h := &transportHarness{
		accepted:       make(chan *testSocket, 8),
		handlerErrors:  make(chan error, 8),
		expectedAppID:  "cli_test",
		expectedSecret: "secret_test",
		configForCall: func(int64) *sdkws.ClientConfig {
			return &sdkws.ClientConfig{
				ReconnectCount:    -1,
				ReconnectInterval: 0,
				ReconnectNonce:    0,
				PingInterval:      60,
			}
		},
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc(sdkws.GenEndpointUri, func(w http.ResponseWriter, r *http.Request) {
		call := h.bootstrapCount.Add(1)
		if r.Method != http.MethodPost {
			h.reportHandlerError(errors.New("bootstrap method is not POST"))
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		var req sdkws.BootstrapRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			h.reportHandlerError(err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.AppID != h.expectedAppID || req.AppSecret != h.expectedSecret ||
			req.ClientAssertion != "" {
			h.reportHandlerError(errors.New("bootstrap credentials do not use app-secret auth"))
			http.Error(w, "credentials", http.StatusUnauthorized)
			return
		}
		if h.expectedSource != "" &&
			!strings.Contains(r.Header.Get("User-Agent"), " source/"+h.expectedSource) {
			h.reportHandlerError(errors.New("bootstrap user agent is missing source"))
		}
		if h.expectedHeader != "" && r.Header.Get("X-Listener-Test") != h.expectedHeader {
			h.reportHandlerError(errors.New("bootstrap custom header is missing"))
		}

		socketURL := "ws" + strings.TrimPrefix(h.server.URL, "http") +
			"/ws?device_id=device_test&service_id=1"
		h.mu.Lock()
		if h.endpointForCall != nil {
			socketURL = h.endpointForCall(call)
		}
		config := h.configForCall(call)
		h.mu.Unlock()
		_ = json.NewEncoder(w).Encode(&sdkws.EndpointResp{
			Code: sdkws.OK,
			Data: &sdkws.Endpoint{Url: socketURL, ClientConfig: config},
		})
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			h.reportHandlerError(err)
			return
		}
		socket := &testSocket{conn: conn, done: make(chan struct{})}
		h.mu.Lock()
		h.sockets = append(h.sockets, socket)
		h.mu.Unlock()
		h.accepted <- socket
		<-socket.done
	})
	h.server = httptest.NewServer(mux)
	t.Cleanup(func() {
		h.mu.Lock()
		sockets := append([]*testSocket(nil), h.sockets...)
		h.mu.Unlock()
		for _, socket := range sockets {
			socket.stop()
		}
		h.server.Close()
	})
	return h
}

func (h *transportHarness) reportHandlerError(err error) {
	select {
	case h.handlerErrors <- err:
	default:
	}
}

func (h *transportHarness) waitSocket(t *testing.T) *testSocket {
	t.Helper()
	select {
	case socket := <-h.accepted:
		return socket
	case err := <-h.handlerErrors:
		t.Fatalf("test server error: %v", err)
	case <-time.After(time.Second):
		t.Fatal("WebSocket did not connect")
	}
	return nil
}

func (h *transportHarness) assertNoHandlerError(t *testing.T) {
	t.Helper()
	select {
	case err := <-h.handlerErrors:
		t.Fatalf("test server error: %v", err)
	default:
	}
}

func startClient(t *testing.T, ctx context.Context, client *Client) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- client.Start(ctx)
	}()
	return done
}

func waitStartCanceled(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not return promptly after cancellation")
	}
}

func waitSocketClosed(t *testing.T, socket *testSocket) {
	t.Helper()
	if err := socket.conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set WebSocket read deadline: %v", err)
	}
	for {
		if _, _, err := socket.conn.ReadMessage(); err != nil {
			socket.stop()
			return
		}
	}
}

func TestClientStartCancellationClosesConnectionAndStopsTransport(t *testing.T) {
	h := newTransportHarness(t)
	h.expectedSource = "listener-test"
	h.expectedHeader = "present"

	var returned atomic.Bool
	var disconnected atomic.Int64
	lateCallback := make(chan struct{}, 1)
	callback := func() {
		if returned.Load() {
			select {
			case lateCallback <- struct{}{}:
			default:
			}
		}
	}
	headers := make(http.Header)
	headers.Set("X-Listener-Test", "present")
	ctx, cancel := context.WithCancel(context.Background())
	client := NewClient("cli_test", "secret_test",
		WithDomain(h.server.URL),
		WithHeaders(headers),
		WithSource("listener-test"),
		WithLogger(discardLogger{}),
		WithOnReady(callback),
		WithOnReconnecting(callback),
		WithOnReconnected(callback),
		WithOnError(func(error) { callback() }),
		WithOnDisconnected(func() {
			disconnected.Add(1)
			callback()
		}),
	)
	done := startClient(t, ctx, client)
	socket := h.waitSocket(t)

	cancel()
	waitStartCanceled(t, done)
	returned.Store(true)
	waitSocketClosed(t, socket)

	if got := disconnected.Load(); got != 1 {
		t.Fatalf("OnDisconnected calls = %d, want 1", got)
	}
	select {
	case <-lateCallback:
		t.Fatal("transport invoked a lifecycle callback after Start returned")
	case <-time.After(100 * time.Millisecond):
	}
	h.assertNoHandlerError(t)
}

func TestClientCancellationStopsReconnectLoop(t *testing.T) {
	h := newTransportHarness(t)
	h.endpointForCall = func(call int64) string {
		if call == 1 {
			return "ws" + strings.TrimPrefix(h.server.URL, "http") +
				"/ws?device_id=device_test&service_id=1"
		}
		return "ws://127.0.0.1:1/ws?device_id=device_test&service_id=1"
	}

	reconnecting := make(chan struct{}, 1)
	connectError := make(chan struct{}, 1)
	lateCallback := make(chan struct{}, 1)
	var returned atomic.Bool
	markCallback := func() {
		if returned.Load() {
			select {
			case lateCallback <- struct{}{}:
			default:
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	client := NewClient("cli_test", "secret_test",
		WithDomain(h.server.URL),
		WithLogger(discardLogger{}),
		WithOnReady(markCallback),
		WithOnReconnecting(func() {
			markCallback()
			select {
			case reconnecting <- struct{}{}:
			default:
			}
		}),
		WithOnReconnected(markCallback),
		WithOnDisconnected(markCallback),
		WithOnError(func(error) {
			markCallback()
			select {
			case connectError <- struct{}{}:
			default:
			}
		}),
	)
	done := startClient(t, ctx, client)
	first := h.waitSocket(t)
	first.stop()
	select {
	case <-reconnecting:
	case <-time.After(time.Second):
		t.Fatal("client did not enter reconnecting state")
	}
	select {
	case <-connectError:
	case <-time.After(time.Second):
		t.Fatal("client did not report reconnect failure")
	}

	cancel()
	waitStartCanceled(t, done)
	returned.Store(true)
	select {
	case <-lateCallback:
		t.Fatal("reconnect loop invoked a lifecycle callback after Start returned")
	case <-time.After(100 * time.Millisecond):
	}
	h.assertNoHandlerError(t)
}

func TestClientAutomaticallyReconnectsAndReportsLifecycle(t *testing.T) {
	h := newTransportHarness(t)
	ready := make(chan struct{}, 1)
	reconnecting := make(chan struct{}, 1)
	reconnected := make(chan struct{}, 1)
	var disconnected atomic.Int64

	ctx, cancel := context.WithCancel(context.Background())
	client := NewClient("cli_test", "secret_test",
		WithDomain(h.server.URL),
		WithLogger(discardLogger{}),
		WithOnReady(func() { ready <- struct{}{} }),
		WithOnReconnecting(func() { reconnecting <- struct{}{} }),
		WithOnReconnected(func() { reconnected <- struct{}{} }),
		WithOnDisconnected(func() { disconnected.Add(1) }),
	)
	done := startClient(t, ctx, client)
	first := h.waitSocket(t)
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("OnReady was not called")
	}

	first.stop()
	select {
	case <-reconnecting:
	case <-time.After(time.Second):
		t.Fatal("OnReconnecting was not called")
	}
	second := h.waitSocket(t)
	select {
	case <-reconnected:
	case <-time.After(time.Second):
		t.Fatal("OnReconnected was not called")
	}

	cancel()
	waitStartCanceled(t, done)
	waitSocketClosed(t, second)
	if got := disconnected.Load(); got != 2 {
		t.Fatalf("OnDisconnected calls = %d, want 2", got)
	}
	h.assertNoHandlerError(t)
}

func TestClientCombinesFragmentedCardEventAndAcknowledgesResponse(t *testing.T) {
	h := newTransportHarness(t)
	handler := dispatcher.NewEventDispatcher("", "").
		OnP2CardActionTrigger(func(
			_ context.Context,
			_ *callback.CardActionTriggerEvent,
		) (*callback.CardActionTriggerResponse, error) {
			return &callback.CardActionTriggerResponse{
				Toast: &callback.Toast{Type: "info", Content: "accepted"},
			}, nil
		})
	payload := cardActionPayload(t)

	ctx, cancel := context.WithCancel(context.Background())
	client := NewClient("cli_test", "secret_test",
		WithDomain(h.server.URL),
		WithLogger(discardLogger{}),
		WithEventHandler(handler),
	)
	done := startClient(t, ctx, client)
	socket := h.waitSocket(t)

	midpoint := len(payload) / 2
	writeDataFrame(t, socket.conn, "msg_fragmented", 2, 1, payload[midpoint:])
	writeDataFrame(t, socket.conn, "msg_fragmented", 2, 0, payload[:midpoint])
	ack := readDataFrame(t, socket.conn)
	if got := sdkws.Headers(ack.Headers).GetString(sdkws.HeaderBizRt); got == "" {
		t.Fatal("response frame is missing biz_rt")
	}
	var response sdkws.Response
	if err := json.Unmarshal(ack.Payload, &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("response status = %d, want 200", response.StatusCode)
	}
	var cardResponse callback.CardActionTriggerResponse
	if err := json.Unmarshal(response.Data, &cardResponse); err != nil {
		t.Fatalf("unmarshal card response: %v", err)
	}
	if cardResponse.Toast == nil || cardResponse.Toast.Content != "accepted" {
		t.Fatalf("card response = %#v", cardResponse)
	}

	cancel()
	waitStartCanceled(t, done)
	waitSocketClosed(t, socket)
	h.assertNoHandlerError(t)
}

func TestClientHandlerErrorAcknowledgesInternalServerError(t *testing.T) {
	h := newTransportHarness(t)
	handler := dispatcher.NewEventDispatcher("", "").
		OnP2CardActionTrigger(func(
			context.Context,
			*callback.CardActionTriggerEvent,
		) (*callback.CardActionTriggerResponse, error) {
			return nil, errors.New("persist failed")
		})
	ctx, cancel := context.WithCancel(context.Background())
	client := NewClient("cli_test", "secret_test",
		WithDomain(h.server.URL),
		WithLogger(discardLogger{}),
		WithEventHandler(handler),
	)
	done := startClient(t, ctx, client)
	socket := h.waitSocket(t)

	payload := cardActionPayload(t)
	writeDataFrame(t, socket.conn, "msg_failed", 1, 0, payload)
	ack := readDataFrame(t, socket.conn)
	var response sdkws.Response
	if err := json.Unmarshal(ack.Payload, &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("response status = %d, want 500", response.StatusCode)
	}

	cancel()
	waitStartCanceled(t, done)
	waitSocketClosed(t, socket)
	h.assertNoHandlerError(t)
}

func TestClientDispatchesCompleteEventsConcurrently(t *testing.T) {
	h := newTransportHarness(t)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	defer release()

	handler := dispatcher.NewEventDispatcher("", "").
		OnP2CardActionTrigger(func(
			_ context.Context,
			ev *callback.CardActionTriggerEvent,
		) (*callback.CardActionTriggerResponse, error) {
			workflowID, _ := ev.Event.Action.Value["source_workflow_id"].(string)
			if workflowID == "wf_first" {
				close(firstStarted)
				<-releaseFirst
			}
			return &callback.CardActionTriggerResponse{
				Toast: &callback.Toast{Type: "info", Content: workflowID},
			}, nil
		})
	ctx, cancel := context.WithCancel(context.Background())
	client := NewClient("cli_test", "secret_test",
		WithDomain(h.server.URL),
		WithLogger(discardLogger{}),
		WithEventHandler(handler),
	)
	done := startClient(t, ctx, client)
	socket := h.waitSocket(t)

	writeDataFrame(t, socket.conn, "msg_first", 1, 0, cardActionPayloadForWorkflow(t, "wf_first"))
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first callback did not start")
	}
	writeDataFrame(t, socket.conn, "msg_second", 1, 0, cardActionPayloadForWorkflow(t, "wf_second"))

	ack := readDataFrameWithTimeout(t, socket.conn, 500*time.Millisecond)
	if got := sdkws.Headers(ack.Headers).GetString(sdkws.HeaderMessageID); got != "msg_second" {
		t.Fatalf("first ACK message_id = %q, want msg_second while first callback is blocked", got)
	}

	release()
	firstAck := readDataFrame(t, socket.conn)
	if got := sdkws.Headers(firstAck.Headers).GetString(sdkws.HeaderMessageID); got != "msg_first" {
		t.Fatalf("second ACK message_id = %q, want msg_first", got)
	}

	cancel()
	waitStartCanceled(t, done)
	waitSocketClosed(t, socket)
	h.assertNoHandlerError(t)
}

func TestClientWaitsForEventHandlersDuringShutdown(t *testing.T) {
	h := newTransportHarness(t)
	handlerStarted := make(chan struct{})
	handlerCanceled := make(chan struct{})
	allowHandlerExit := make(chan struct{})
	handlerExited := make(chan struct{})
	handler := dispatcher.NewEventDispatcher("", "").
		OnP2CardActionTrigger(func(
			ctx context.Context,
			_ *callback.CardActionTriggerEvent,
		) (*callback.CardActionTriggerResponse, error) {
			close(handlerStarted)
			<-ctx.Done()
			close(handlerCanceled)
			<-allowHandlerExit
			close(handlerExited)
			return nil, ctx.Err()
		})
	ctx, cancel := context.WithCancel(context.Background())
	client := NewClient("cli_test", "secret_test",
		WithDomain(h.server.URL),
		WithLogger(discardLogger{}),
		WithEventHandler(handler),
	)
	done := startClient(t, ctx, client)
	socket := h.waitSocket(t)

	writeDataFrame(t, socket.conn, "msg_shutdown", 1, 0, cardActionPayload(t))
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("callback did not start")
	}
	cancel()
	select {
	case <-handlerCanceled:
	case <-time.After(time.Second):
		t.Fatal("callback did not observe connection cancellation")
	}
	select {
	case err := <-done:
		t.Fatalf("Start returned before callback exited: %v", err)
	default:
	}
	close(allowHandlerExit)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start error = %v, want context canceled", err)
		}
		select {
		case <-handlerExited:
		default:
			t.Fatal("Start returned before callback exit was recorded")
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not return after callback exited")
	}
	waitSocketClosed(t, socket)
	h.assertNoHandlerError(t)
}

func TestClientBootstrapAttemptHasOwnTimeout(t *testing.T) {
	requestStarted := make(chan struct{})
	requestCanceled := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(requestStarted)
		<-r.Context().Done()
		requestCanceled <- r.Context().Err()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewClient("cli_test", "secret_test",
		WithDomain(server.URL),
		WithAutoReconnect(false),
		WithLogger(discardLogger{}),
	)
	client.bootstrapTimeout = 25 * time.Millisecond
	done := startClient(t, ctx, client)
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("bootstrap request did not start")
	}
	var startErr error
	safetyCanceled := false
	select {
	case startErr = <-done:
	case <-time.After(500 * time.Millisecond):
		safetyCanceled = true
		cancel()
		server.CloseClientConnections()
		startErr = <-done
	}
	server.CloseClientConnections()
	var requestErr error
	select {
	case requestErr = <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("bootstrap handler did not observe request cancellation")
	}
	if safetyCanceled {
		t.Fatalf("Start did not enforce a bootstrap timeout; returned only after safety cancellation: %v", startErr)
	}
	if !errors.Is(requestErr, context.Canceled) {
		t.Fatalf("bootstrap request context error = %v, want canceled", requestErr)
	}
	if !isDeadlineError(startErr) {
		t.Fatalf("Start error = %v, want deadline exceeded", startErr)
	}
}

func TestClientDialAttemptHasOwnTimeout(t *testing.T) {
	dialStarted := make(chan struct{})
	dialCanceled := make(chan error, 1)
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	mux.HandleFunc(sdkws.GenEndpointUri, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(&sdkws.EndpointResp{
			Code: sdkws.OK,
			Data: &sdkws.Endpoint{
				Url: "ws" + strings.TrimPrefix(server.URL, "http") + "/ws",
				ClientConfig: &sdkws.ClientConfig{
					ReconnectCount: -1,
					PingInterval:   60,
				},
			},
		})
	})
	mux.HandleFunc("/ws", func(_ http.ResponseWriter, r *http.Request) {
		close(dialStarted)
		<-r.Context().Done()
		dialCanceled <- r.Context().Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewClient("cli_test", "secret_test",
		WithDomain(server.URL),
		WithAutoReconnect(false),
		WithLogger(discardLogger{}),
	)
	client.bootstrapTimeout = time.Second
	client.dialTimeout = 25 * time.Millisecond
	done := startClient(t, ctx, client)
	select {
	case <-dialStarted:
	case <-time.After(time.Second):
		t.Fatal("WebSocket dial did not start")
	}
	var startErr error
	safetyCanceled := false
	select {
	case startErr = <-done:
	case <-time.After(500 * time.Millisecond):
		safetyCanceled = true
		cancel()
		server.CloseClientConnections()
		startErr = <-done
	}
	server.CloseClientConnections()
	var dialErr error
	select {
	case dialErr = <-dialCanceled:
	case <-time.After(time.Second):
		t.Fatal("dial handler did not observe request cancellation")
	}
	if safetyCanceled {
		t.Fatalf("Start did not enforce a dial timeout; returned only after safety cancellation: %v", startErr)
	}
	if !errors.Is(dialErr, context.Canceled) {
		t.Fatalf("dial request context error = %v, want canceled", dialErr)
	}
	if !isDeadlineError(startErr) {
		t.Fatalf("Start error = %v, want deadline exceeded", startErr)
	}
}

func TestBoundedDeadlineUsesConfiguredTimeout(t *testing.T) {
	now := time.Date(2026, 8, 1, 6, 0, 0, 0, time.UTC)
	got, err := boundedDeadline(context.Background(), now, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(3 * time.Second); !got.Equal(want) {
		t.Fatalf("deadline = %s, want %s", got, want)
	}
}

func TestBoundedDeadlineRespectsEarlierContextDeadline(t *testing.T) {
	now := time.Now()
	parentDeadline := now.Add(time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), parentDeadline)
	defer cancel()

	got, err := boundedDeadline(ctx, now, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(parentDeadline) {
		t.Fatalf("deadline = %s, want caller deadline %s", got, parentDeadline)
	}
}

func TestBoundedDeadlineRejectsNonpositiveTimeout(t *testing.T) {
	if _, err := boundedDeadline(context.Background(), time.Now(), 0); err == nil {
		t.Fatal("boundedDeadline accepted a nonpositive timeout")
	}
}

func TestClientAppliesPongPingInterval(t *testing.T) {
	h := newTransportHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	client := NewClient("cli_test", "secret_test",
		WithDomain(h.server.URL),
		WithLogger(discardLogger{}),
	)
	done := startClient(t, ctx, client)
	socket := h.waitSocket(t)

	firstPing := readControlFrame(t, socket.conn, sdkws.MessageTypePing, time.Second)
	if firstPing.Service != 1 {
		t.Fatalf("ping service = %d, want 1", firstPing.Service)
	}
	pongPayload, err := json.Marshal(&sdkws.ClientConfig{
		ReconnectCount:    -1,
		ReconnectInterval: 0,
		ReconnectNonce:    0,
		PingInterval:      1,
	})
	if err != nil {
		t.Fatalf("marshal pong config: %v", err)
	}
	headers := sdkws.Headers{}
	headers.Add(sdkws.HeaderType, string(sdkws.MessageTypePong))
	pong := sdkws.Frame{
		Method:  int32(sdkws.FrameTypeControl),
		Service: 1,
		Headers: headers,
		Payload: pongPayload,
	}
	raw, err := pong.Marshal()
	if err != nil {
		t.Fatalf("marshal pong frame: %v", err)
	}
	if err := socket.conn.WriteMessage(websocket.BinaryMessage, raw); err != nil {
		t.Fatalf("write pong frame: %v", err)
	}
	_ = readControlFrame(t, socket.conn, sdkws.MessageTypePing, 2*time.Second)

	cancel()
	waitStartCanceled(t, done)
	waitSocketClosed(t, socket)
	h.assertNoHandlerError(t)
}

func cardActionPayload(t *testing.T) []byte {
	return cardActionPayloadForWorkflow(t, "wf_source")
}

func cardActionPayloadForWorkflow(t *testing.T, workflowID string) []byte {
	t.Helper()
	tenant := "tenant_a"
	raw, err := json.Marshal(&callback.CardActionTriggerEvent{
		EventV2Base: &larkevent.EventV2Base{
			Schema: "2.0",
			Header: &larkevent.EventHeader{
				EventID:   "evt_transport",
				EventType: "card.action.trigger",
				AppID:     "cli_test",
				TenantKey: tenant,
			},
		},
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{TenantKey: &tenant, OpenID: "ou_test"},
			Action: &callback.CallBackAction{
				Tag: "button",
				Value: map[string]any{
					"action":             "retry",
					"source_workflow_id": workflowID,
				},
			},
			Host:    "im_message",
			Context: &callback.Context{OpenMessageID: "om_1"},
		},
	})
	if err != nil {
		t.Fatalf("marshal card action: %v", err)
	}
	return raw
}

func isDeadlineError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func writeDataFrame(
	t *testing.T,
	conn *websocket.Conn,
	messageID string,
	sum, seq int,
	payload []byte,
) {
	t.Helper()
	headers := sdkws.Headers{}
	headers.Add(sdkws.HeaderType, string(sdkws.MessageTypeEvent))
	headers.Add(sdkws.HeaderMessageID, messageID)
	headers.Add(sdkws.HeaderTraceID, "trace_test")
	headers.Add(sdkws.HeaderSum, strconv.Itoa(sum))
	headers.Add(sdkws.HeaderSeq, strconv.Itoa(seq))
	frame := sdkws.Frame{
		Method:  int32(sdkws.FrameTypeData),
		Service: 1,
		Headers: headers,
		Payload: payload,
	}
	raw, err := frame.Marshal()
	if err != nil {
		t.Fatalf("marshal data frame: %v", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, raw); err != nil {
		t.Fatalf("write data frame: %v", err)
	}
}

func readDataFrame(t *testing.T, conn *websocket.Conn) sdkws.Frame {
	t.Helper()
	return readDataFrameWithTimeout(t, conn, 2*time.Second)
}

func readDataFrameWithTimeout(
	t *testing.T,
	conn *websocket.Conn,
	timeout time.Duration,
) sdkws.Frame {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read response frame: %v", err)
		}
		var frame sdkws.Frame
		if err := frame.Unmarshal(raw); err != nil {
			t.Fatalf("unmarshal response frame: %v", err)
		}
		if sdkws.FrameType(frame.Method) == sdkws.FrameTypeData {
			return frame
		}
	}
}

func readControlFrame(
	t *testing.T,
	conn *websocket.Conn,
	messageType sdkws.MessageType,
	timeout time.Duration,
) sdkws.Frame {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read control frame: %v", err)
		}
		var frame sdkws.Frame
		if err := frame.Unmarshal(raw); err != nil {
			t.Fatalf("unmarshal control frame: %v", err)
		}
		headers := sdkws.Headers(frame.Headers)
		if sdkws.FrameType(frame.Method) == sdkws.FrameTypeControl &&
			sdkws.MessageType(headers.GetString(sdkws.HeaderType)) == messageType {
			return frame
		}
	}
}
