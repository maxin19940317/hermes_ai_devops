// Package larkws provides the context-aware Feishu WebSocket transport used by
// the runtime listener.
//
// Portions of this file are derived from the MIT-licensed WebSocket client in
// github.com/larksuite/oapi-sdk-go/v3/ws. See LICENSE for attribution.
package larkws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	sdkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

const (
	defaultReconnectNonce    = 30
	defaultReconnectInterval = 2 * time.Minute
	defaultPingInterval      = 2 * time.Minute
	fragmentTTL              = 5 * time.Second
)

type connectionConfig struct {
	reconnectCount    int
	reconnectInterval time.Duration
	reconnectNonce    int
	pingInterval      time.Duration
}

type connection struct {
	socket    *websocket.Conn
	url       *url.URL
	connID    string
	serviceID string
	writeMu   sync.Mutex
}

type fragment struct {
	parts     [][]byte
	expiresAt time.Time
}

// Client owns a Feishu WebSocket connection and all transport goroutines.
type Client struct {
	appID         string
	appSecret     string
	eventHandler  *dispatcher.EventDispatcher
	logLevel      larkcore.LogLevel
	logger        larkcore.Logger
	domain        string
	headers       http.Header
	source        string
	autoReconnect bool
	httpClient    *http.Client
	dialer        *websocket.Dialer

	onReady        func()
	onError        func(error)
	onReconnecting func()
	onReconnected  func()
	onDisconnected func()

	configMu sync.RWMutex
	config   connectionConfig
	configCh chan struct{}

	runMu   sync.Mutex
	running bool
}

// ClientOption configures a Client before it starts.
type ClientOption func(*Client)

// WithEventHandler sets the SDK event dispatcher used for incoming events.
func WithEventHandler(handler *dispatcher.EventDispatcher) ClientOption {
	return func(client *Client) {
		client.eventHandler = handler
	}
}

// WithLogLevel sets the SDK-compatible logger level.
func WithLogLevel(level larkcore.LogLevel) ClientOption {
	return func(client *Client) {
		client.logLevel = level
	}
}

// WithLogger sets the logger used by the transport.
func WithLogger(logger larkcore.Logger) ClientOption {
	return func(client *Client) {
		client.logger = logger
	}
}

// WithAutoReconnect enables or disables automatic reconnects.
func WithAutoReconnect(enabled bool) ClientOption {
	return func(client *Client) {
		client.autoReconnect = enabled
	}
}

// WithDomain overrides the Feishu API base URL.
func WithDomain(domain string) ClientOption {
	return func(client *Client) {
		client.domain = domain
	}
}

// WithHeaders adds headers to the bootstrap request.
func WithHeaders(headers http.Header) ClientOption {
	return func(client *Client) {
		client.headers = headers.Clone()
	}
}

// WithSource appends a source identifier to the SDK User-Agent.
func WithSource(source string) ClientOption {
	return func(client *Client) {
		client.source = source
	}
}

// WithOnReady sets the callback for the initial successful connection.
func WithOnReady(callback func()) ClientOption {
	return func(client *Client) {
		client.onReady = callback
	}
}

// WithOnError sets the callback for bootstrap or dial errors.
func WithOnError(callback func(error)) ClientOption {
	return func(client *Client) {
		client.onError = callback
	}
}

// WithOnReconnecting sets the callback invoked before reconnect attempts.
func WithOnReconnecting(callback func()) ClientOption {
	return func(client *Client) {
		client.onReconnecting = callback
	}
}

// WithOnReconnected sets the callback for a successful reconnect.
func WithOnReconnected(callback func()) ClientOption {
	return func(client *Client) {
		client.onReconnected = callback
	}
}

// WithOnDisconnected sets the callback invoked after a socket is fully closed.
func WithOnDisconnected(callback func()) ClientOption {
	return func(client *Client) {
		client.onDisconnected = callback
	}
}

// NewClient constructs a Feishu WebSocket client.
func NewClient(appID, appSecret string, options ...ClientOption) *Client {
	client := &Client{
		appID:         appID,
		appSecret:     appSecret,
		domain:        lark.FeishuBaseUrl,
		autoReconnect: true,
		httpClient:    http.DefaultClient,
		dialer:        websocket.DefaultDialer,
		configCh:      make(chan struct{}, 1),
		config: connectionConfig{
			reconnectCount:    -1,
			reconnectInterval: defaultReconnectInterval,
			reconnectNonce:    defaultReconnectNonce,
			pingInterval:      defaultPingInterval,
		},
	}
	for _, option := range options {
		option(client)
	}
	if client.logger == nil {
		client.logger = larkcore.NewDefaultLogger(client.logLevel)
	}
	return client
}

// Start blocks until ctx is canceled or the client reaches a non-retryable
// transport error. It joins every transport goroutine before returning.
func (c *Client) Start(ctx context.Context) error {
	if ctx == nil {
		return errors.New("start Feishu WebSocket: context is nil")
	}
	if err := c.beginRun(); err != nil {
		return err
	}
	defer c.endRun()

	reconnecting := false
	reconnectAttempt := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if reconnecting {
			if !c.retryAllowed(reconnectAttempt) {
				count := c.configSnapshot().reconnectCount
				return fmt.Errorf("unable to connect to server after %d retries", count)
			}
			var wait time.Duration
			if reconnectAttempt == 0 {
				wait = c.reconnectJitter()
			} else {
				wait = c.configSnapshot().reconnectInterval
			}
			if err := waitContext(ctx, wait); err != nil {
				return err
			}
			reconnectAttempt++
		}

		conn, err := c.connect(ctx)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			c.logger.Error(ctx, "connect failed, err: ", err)
			c.callError(err)
			var clientErr *sdkws.ClientError
			if errors.As(err, &clientErr) || !c.autoReconnect {
				return err
			}
			if !reconnecting {
				reconnecting = true
				reconnectAttempt = 0
				c.call(c.onReconnecting)
			}
			continue
		}

		if reconnecting {
			c.call(c.onReconnected)
		} else {
			c.call(c.onReady)
		}
		reconnecting = false
		reconnectAttempt = 0

		err = c.runConnection(ctx, conn)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err == nil {
			err = errors.New("Feishu WebSocket connection stopped")
		}
		c.logger.Error(ctx, c.logArgs(conn, "connection stopped, err: %v", err)...)
		if !c.autoReconnect {
			return err
		}
		reconnecting = true
		c.call(c.onReconnecting)
	}
}

func (c *Client) beginRun() error {
	c.runMu.Lock()
	defer c.runMu.Unlock()
	if c.running {
		return errors.New("Feishu WebSocket client is already running")
	}
	c.running = true
	return nil
}

func (c *Client) endRun() {
	c.runMu.Lock()
	c.running = false
	c.runMu.Unlock()
}

func (c *Client) retryAllowed(attempt int) bool {
	count := c.configSnapshot().reconnectCount
	return count < 0 || attempt < count
}

func (c *Client) reconnectJitter() time.Duration {
	nonce := c.configSnapshot().reconnectNonce
	if nonce <= 0 {
		return 0
	}
	return time.Duration(rand.Intn(nonce*1000)) * time.Millisecond
}

func waitContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) connect(ctx context.Context) (*connection, error) {
	rawURL, err := c.getConnURL(ctx)
	if err != nil {
		return nil, fmt.Errorf("get connection URL: %w", err)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse connection URL: %w", err)
	}
	socket, response, err := c.dialer.DialContext(ctx, rawURL, nil)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		if response != nil && response.StatusCode != http.StatusSwitchingProtocols {
			return nil, parseHandshakeError(response)
		}
		return nil, fmt.Errorf("dial WebSocket: %w", err)
	}
	conn := &connection{
		socket:    socket,
		url:       parsed,
		connID:    parsed.Query().Get(sdkws.DeviceID),
		serviceID: parsed.Query().Get(sdkws.ServiceID),
	}
	c.logger.Info(ctx, c.logArgs(conn, "connected to %s", parsed)...)
	return conn, nil
}

func (c *Client) getConnURL(ctx context.Context) (string, error) {
	if c.appSecret == "" {
		return "", sdkws.NewClientError(
			larkcore.ErrCodeAppSecretAndClientAssertionEmpty,
			"appSecret cannot be empty",
		)
	}
	body, err := json.Marshal(&sdkws.BootstrapRequest{
		AppID:     c.appID,
		AppSecret: c.appSecret,
	})
	if err != nil {
		return "", fmt.Errorf("marshal bootstrap request: %w", err)
	}
	requestURL := strings.TrimRight(c.domain, "/") + sdkws.GenEndpointUri
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		requestURL,
		bytes.NewReader(body),
	)
	if err != nil {
		return "", fmt.Errorf("create bootstrap request: %w", err)
	}
	request.Header.Set("locale", "zh")
	request.Header.Set("Content-Type", "application/json")
	for key, values := range c.headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	request.Header.Set("User-Agent", larkcore.UserAgent(c.source))

	response, err := c.httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("bootstrap request: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("read bootstrap response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		serverMessage := "system busy"
		var errorResponse struct {
			Msg string `json:"msg"`
		}
		if json.Unmarshal(responseBody, &errorResponse) == nil && errorResponse.Msg != "" {
			serverMessage = errorResponse.Msg
		}
		return "", sdkws.NewServerError(response.StatusCode, serverMessage)
	}

	var endpointResponse sdkws.EndpointResp
	if err := json.Unmarshal(responseBody, &endpointResponse); err != nil {
		return "", fmt.Errorf("decode bootstrap response: %w", err)
	}
	switch endpointResponse.Code {
	case sdkws.OK:
	case sdkws.SystemBusy:
		return "", sdkws.NewServerError(endpointResponse.Code, "system busy")
	case sdkws.InternalError:
		return "", sdkws.NewServerError(endpointResponse.Code, endpointResponse.Msg)
	default:
		return "", sdkws.NewClientError(endpointResponse.Code, endpointResponse.Msg)
	}
	if endpointResponse.Data == nil || endpointResponse.Data.Url == "" {
		return "", sdkws.NewServerError(
			http.StatusInternalServerError,
			"endpoint is null",
		)
	}
	if endpointResponse.Data.ClientConfig != nil {
		c.configure(endpointResponse.Data.ClientConfig)
	}
	return endpointResponse.Data.Url, nil
}

func (c *Client) configure(config *sdkws.ClientConfig) {
	c.configMu.Lock()
	c.config.reconnectCount = config.ReconnectCount
	c.config.reconnectInterval = time.Duration(config.ReconnectInterval) * time.Second
	c.config.reconnectNonce = config.ReconnectNonce
	if config.PingInterval > 0 {
		c.config.pingInterval = time.Duration(config.PingInterval) * time.Second
	}
	c.configMu.Unlock()
	select {
	case c.configCh <- struct{}{}:
	default:
	}
}

func (c *Client) configSnapshot() connectionConfig {
	c.configMu.RLock()
	defer c.configMu.RUnlock()
	return c.config
}

func (c *Client) runConnection(ctx context.Context, conn *connection) error {
	connectionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	go c.runWorker(connectionCtx, &workers, errCh, func() error {
		return c.receiveLoop(connectionCtx, conn)
	})
	go c.runWorker(connectionCtx, &workers, errCh, func() error {
		return c.pingLoop(connectionCtx, conn)
	})

	var runErr error
	select {
	case <-ctx.Done():
		runErr = ctx.Err()
	case runErr = <-errCh:
	}
	cancel()
	_ = conn.socket.Close()
	workers.Wait()
	c.logger.Info(ctx, c.logArgs(conn, "disconnected from %s", conn.url)...)
	c.call(c.onDisconnected)
	return runErr
}

func (c *Client) runWorker(
	ctx context.Context,
	workers *sync.WaitGroup,
	errCh chan<- error,
	work func() error,
) {
	defer workers.Done()
	defer func() {
		if recovered := recover(); recovered != nil {
			err := fmt.Errorf(
				"WebSocket worker panic: %v, stack: %s",
				recovered,
				string(debug.Stack()),
			)
			select {
			case errCh <- err:
			case <-ctx.Done():
			}
		}
	}()
	if err := work(); err != nil {
		select {
		case errCh <- err:
		case <-ctx.Done():
		}
	}
}

func (c *Client) pingLoop(ctx context.Context, conn *connection) error {
	serviceID, _ := strconv.ParseInt(conn.serviceID, 10, 32)
	for {
		frame := sdkws.NewPingFrame(int32(serviceID))
		raw, err := frame.Marshal()
		if err != nil {
			return fmt.Errorf("marshal ping: %w", err)
		}
		if err := conn.write(websocket.BinaryMessage, raw); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("write ping: %w", err)
		}
		c.logger.Debug(ctx, c.logArgs(conn, "ping success")...)
		if err := c.waitPing(ctx); err != nil {
			return nil
		}
	}
}

func (c *Client) waitPing(ctx context.Context) error {
	for {
		timer := time.NewTimer(c.configSnapshot().pingInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-c.configCh:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
			return nil
		}
	}
}

func (c *Client) receiveLoop(ctx context.Context, conn *connection) error {
	fragments := make(map[string]fragment)
	for {
		messageType, raw, err := conn.socket.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read message: %w", err)
		}
		if messageType != websocket.BinaryMessage {
			c.logger.Warn(
				ctx,
				c.logArgs(
					conn,
					"receive unknown message, message_type: %d",
					messageType,
				)...,
			)
			continue
		}
		if err := c.handleMessage(ctx, conn, fragments, raw); err != nil {
			return err
		}
	}
}

func (c *Client) handleMessage(
	ctx context.Context,
	conn *connection,
	fragments map[string]fragment,
	raw []byte,
) error {
	var frame sdkws.Frame
	if err := frame.Unmarshal(raw); err != nil {
		c.logger.Error(ctx, c.logArgs(conn, "unmarshal message failed: %v", err)...)
		return nil
	}
	switch sdkws.FrameType(frame.Method) {
	case sdkws.FrameTypeControl:
		c.handleControlFrame(ctx, conn, frame)
		return nil
	case sdkws.FrameTypeData:
		return c.handleDataFrame(ctx, conn, fragments, frame)
	default:
		return nil
	}
}

func (c *Client) handleControlFrame(
	ctx context.Context,
	conn *connection,
	frame sdkws.Frame,
) {
	headers := sdkws.Headers(frame.Headers)
	if sdkws.MessageType(headers.GetString(sdkws.HeaderType)) != sdkws.MessageTypePong {
		return
	}
	c.logger.Debug(ctx, c.logArgs(conn, "receive pong")...)
	if len(frame.Payload) == 0 {
		return
	}
	var config sdkws.ClientConfig
	if err := json.Unmarshal(frame.Payload, &config); err != nil {
		c.logger.Warn(ctx, c.logArgs(conn, "unmarshal client config: %v", err)...)
		return
	}
	c.configure(&config)
}

func (c *Client) handleDataFrame(
	ctx context.Context,
	conn *connection,
	fragments map[string]fragment,
	frame sdkws.Frame,
) error {
	headers := sdkws.Headers(frame.Headers)
	sum := headers.GetInt(sdkws.HeaderSum)
	seq := headers.GetInt(sdkws.HeaderSeq)
	messageID := headers.GetString(sdkws.HeaderMessageID)
	traceID := headers.GetString(sdkws.HeaderTraceID)
	messageType := sdkws.MessageType(headers.GetString(sdkws.HeaderType))

	payload := frame.Payload
	if sum > 1 {
		var complete bool
		var err error
		payload, complete, err = combineFragments(
			fragments,
			messageID,
			sum,
			seq,
			payload,
			time.Now(),
		)
		if err != nil {
			c.logger.Error(ctx, c.logArgs(conn, "combine fragments: %v", err)...)
			return nil
		}
		if !complete {
			return nil
		}
	}
	c.logger.Debug(
		ctx,
		c.logArgs(
			conn,
			"receive message, message_type: %s, message_id: %s, trace_id: %s, payload: %s",
			messageType,
			messageID,
			traceID,
			payload,
		)...,
	)

	if messageType != sdkws.MessageTypeEvent {
		return nil
	}
	started := time.Now()
	var responseData interface{}
	var handlerErr error
	if c.eventHandler == nil {
		handlerErr = errors.New("event handler is nil")
	} else {
		responseData, handlerErr = c.eventHandler.Do(ctx, payload)
	}
	headers.Add(sdkws.HeaderBizRt, strconv.FormatInt(time.Since(started).Milliseconds(), 10))

	response := sdkws.NewResponseByCode(http.StatusOK)
	if handlerErr != nil {
		c.logger.Error(
			ctx,
			c.logArgs(
				conn,
				"handle message failed, message_type: %s, message_id: %s, trace_id: %s, err: %v",
				messageType,
				messageID,
				traceID,
				handlerErr,
			)...,
		)
		response = sdkws.NewResponseByCode(http.StatusInternalServerError)
	} else if responseData != nil {
		data, err := json.Marshal(responseData)
		if err != nil {
			response = sdkws.NewResponseByCode(http.StatusInternalServerError)
		} else {
			response.Data = data
		}
	}
	responsePayload, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	frame.Payload = responsePayload
	frame.Headers = headers
	raw, err := frame.Marshal()
	if err != nil {
		return fmt.Errorf("marshal response frame: %w", err)
	}
	if err := conn.write(websocket.BinaryMessage, raw); err != nil {
		return fmt.Errorf("write response: %w", err)
	}
	return nil
}

func combineFragments(
	fragments map[string]fragment,
	messageID string,
	sum, seq int,
	payload []byte,
	now time.Time,
) ([]byte, bool, error) {
	for key, pending := range fragments {
		if !pending.expiresAt.After(now) {
			delete(fragments, key)
		}
	}
	if messageID == "" {
		return nil, false, errors.New("fragmented frame has no message_id")
	}
	if sum <= 1 || seq < 0 || seq >= sum {
		return nil, false, fmt.Errorf("invalid fragment sum=%d seq=%d", sum, seq)
	}
	pending, exists := fragments[messageID]
	if !exists || len(pending.parts) != sum {
		pending = fragment{
			parts:     make([][]byte, sum),
			expiresAt: now.Add(fragmentTTL),
		}
	}
	pending.parts[seq] = append([]byte(nil), payload...)
	pending.expiresAt = now.Add(fragmentTTL)
	fragments[messageID] = pending

	size := 0
	for _, part := range pending.parts {
		if len(part) == 0 {
			return nil, false, nil
		}
		size += len(part)
	}
	combined := make([]byte, 0, size)
	for _, part := range pending.parts {
		combined = append(combined, part...)
	}
	delete(fragments, messageID)
	return combined, true, nil
}

func (conn *connection) write(messageType int, payload []byte) error {
	conn.writeMu.Lock()
	defer conn.writeMu.Unlock()
	return conn.socket.WriteMessage(messageType, payload)
}

func (c *Client) call(callback func()) {
	if callback != nil {
		callback()
	}
}

func (c *Client) callError(err error) {
	if c.onError != nil {
		c.onError(err)
	}
}

func (c *Client) logArgs(
	conn *connection,
	format string,
	args ...interface{},
) []interface{} {
	result := []interface{}{fmt.Sprintf(format, args...)}
	if conn != nil && conn.connID != "" {
		result = append(result, fmt.Sprintf("[conn_id=%s]", conn.connID))
	}
	return result
}

func parseHandshakeError(response *http.Response) error {
	code, _ := strconv.Atoi(response.Header.Get(sdkws.HeaderHandshakeStatus))
	message := response.Header.Get(sdkws.HeaderHandshakeMsg)
	switch code {
	case sdkws.AuthFailed:
		authCode, _ := strconv.Atoi(
			response.Header.Get(sdkws.HeaderHandshakeAuthErrCode),
		)
		if authCode == sdkws.ExceedConnLimit {
			return sdkws.NewClientError(code, message)
		}
		return sdkws.NewServerError(code, message)
	case sdkws.Forbidden:
		return sdkws.NewClientError(code, message)
	default:
		return sdkws.NewServerError(code, message)
	}
}
