package feishucmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"

	"hermes-devops/runtime/internal/cardaction"
	"hermes-devops/runtime/internal/store"
)

type fakeListenerClient struct {
	start func(context.Context) error
}

func (c fakeListenerClient) Start(ctx context.Context) error {
	return c.start(ctx)
}

func listenerReadiness() *cardaction.Readiness {
	return cardaction.NewReadiness(cardaction.ReadinessConfig{
		Enabled:           true,
		WhitelistNonEmpty: true,
		SenderIsApp:       true,
		HandlerWired:      true,
	})
}

func listenerCardPayload(t *testing.T) []byte {
	t.Helper()
	tenant := "tenant_a"
	raw, err := json.Marshal(&callback.CardActionTriggerEvent{
		EventV2Base: &larkevent.EventV2Base{
			Schema: "2.0",
			Header: &larkevent.EventHeader{
				EventID:   "evt_listener",
				EventType: "card.action.trigger",
				AppID:     "cli_a",
				TenantKey: tenant,
			},
		},
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{TenantKey: &tenant, OpenID: "ou_allowed"},
			Action: &callback.CallBackAction{
				Tag: "button",
				Value: map[string]any{
					"action":             "retry",
					"source_workflow_id": "wf_source",
				},
			},
			Host:    "im_message",
			Context: &callback.Context{OpenMessageID: "om_1"},
		},
	})
	if err != nil {
		t.Fatalf("marshal card event: %v", err)
	}
	return raw
}

func listenerEventPayload(t *testing.T, eventType string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"schema": "2.0",
		"header": map[string]any{"event_type": eventType},
		"event":  map[string]any{},
	})
	if err != nil {
		t.Fatalf("marshal %s event: %v", eventType, err)
	}
	return raw
}

func TestListenerRegistersCardAndMessageHandlers(t *testing.T) {
	st := store.NewMemStore()
	readiness := listenerReadiness()
	card := &cardaction.Handler{
		Store:     st,
		Readiness: readiness,
		Whitelist: map[string]bool{"ou_allowed": true},
		AppID:     "cli_a",
	}
	startErr := errors.New("listener stopped")
	l := &Listener{
		AppID:     "cli_a",
		AppSecret: "secret",
		Exec:      &Executor{Whitelist: map[string]bool{}},
		Card:      card,
		Readiness: readiness,
	}
	l.newClient = func(appID, appSecret string, cfg listenerClientConfig) listenerClient {
		if appID != "cli_a" || appSecret != "secret" {
			t.Fatalf("credentials = %q/%q", appID, appSecret)
		}
		return fakeListenerClient{start: func(ctx context.Context) error {
			cfg.OnReady()
			got, err := cfg.Handler.Do(ctx, listenerCardPayload(t))
			if err != nil {
				t.Fatalf("dispatch card action: %v", err)
			}
			resp, ok := got.(*callback.CardActionTriggerResponse)
			if !ok || resp.Toast == nil || resp.Toast.Content != "已收到，正在处理" {
				t.Fatalf("card response = %#v", got)
			}
			for _, eventType := range []string{
				"im.message.receive_v1",
				"im.message.message_read_v1",
			} {
				if _, err := cfg.Handler.Do(ctx, listenerEventPayload(t, eventType)); err != nil {
					t.Fatalf("dispatch %s: %v", eventType, err)
				}
			}
			return startErr
		}}
	}

	if err := l.Run(context.Background()); !errors.Is(err, startErr) {
		t.Fatalf("Run error = %v, want %v", err, startErr)
	}
	row, err := st.GetInbox(context.Background(), "evt_listener")
	if err != nil {
		t.Fatalf("GetInbox: %v", err)
	}
	if row.Disposition != "accepted" || row.State != "received" {
		t.Fatalf("persisted inbox = %#v", row)
	}
}

func TestListenerLifecycleHooksUpdateReadiness(t *testing.T) {
	tests := []struct {
		name string
		want bool
		call func(listenerClientConfig)
	}{
		{name: "ready", want: true, call: func(c listenerClientConfig) { c.OnReady() }},
		{name: "reconnected", want: true, call: func(c listenerClientConfig) { c.OnReconnected() }},
		{name: "reconnecting", call: func(c listenerClientConfig) { c.OnReconnecting() }},
		{name: "disconnected", call: func(c listenerClientConfig) { c.OnDisconnected() }},
		{name: "error", call: func(c listenerClientConfig) { c.OnError(errors.New("socket")) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			readiness := listenerReadiness()
			if !tt.want {
				readiness.SetWS(true)
			}
			stopErr := errors.New("stop")
			observed := false
			l := &Listener{
				Exec:      &Executor{Whitelist: map[string]bool{}},
				Readiness: readiness,
			}
			l.newClient = func(_, _ string, cfg listenerClientConfig) listenerClient {
				return fakeListenerClient{start: func(context.Context) error {
					tt.call(cfg)
					observed = readiness.Ready()
					return stopErr
				}}
			}
			if err := l.Run(context.Background()); !errors.Is(err, stopErr) {
				t.Fatalf("Run error = %v", err)
			}
			if observed != tt.want {
				t.Fatalf("readiness in %s hook = %v, want %v", tt.name, observed, tt.want)
			}
		})
	}
}

func TestListenerRunExitClearsReadiness(t *testing.T) {
	tests := []struct {
		name  string
		start func(context.Context, listenerClientConfig) error
	}{
		{
			name: "start error",
			start: func(_ context.Context, cfg listenerClientConfig) error {
				cfg.OnReady()
				return errors.New("start failed")
			},
		},
		{
			name: "cancellation",
			start: func(ctx context.Context, cfg listenerClientConfig) error {
				cfg.OnReady()
				<-ctx.Done()
				return ctx.Err()
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			readiness := listenerReadiness()
			ctx, cancel := context.WithCancel(context.Background())
			if tt.name == "cancellation" {
				cancel()
			} else {
				defer cancel()
			}
			l := &Listener{
				Exec:      &Executor{Whitelist: map[string]bool{}},
				Readiness: readiness,
			}
			l.newClient = func(_, _ string, cfg listenerClientConfig) listenerClient {
				return fakeListenerClient{start: func(ctx context.Context) error {
					err := tt.start(ctx, cfg)
					if !readiness.Ready() {
						t.Fatal("readiness should be true before Start exits")
					}
					return err
				}}
			}
			_ = l.Run(ctx)
			if readiness.Ready() {
				t.Fatal("Run exit must clear WebSocket readiness")
			}
		})
	}
}

func TestListenerLateLifecycleHooksCannotRestoreReadinessAfterExit(t *testing.T) {
	readiness := listenerReadiness()
	stopErr := errors.New("stop")
	var captured listenerClientConfig
	l := &Listener{
		Exec:      &Executor{Whitelist: map[string]bool{}},
		Readiness: readiness,
	}
	l.newClient = func(_, _ string, cfg listenerClientConfig) listenerClient {
		captured = cfg
		return fakeListenerClient{start: func(context.Context) error {
			cfg.OnReady()
			return stopErr
		}}
	}

	if err := l.Run(context.Background()); !errors.Is(err, stopErr) {
		t.Fatalf("Run error = %v, want %v", err, stopErr)
	}
	if readiness.Ready() {
		t.Fatal("Run exit must clear WebSocket readiness")
	}

	for name, hook := range map[string]func(){
		"ready":       captured.OnReady,
		"reconnected": captured.OnReconnected,
	} {
		t.Run(name, func(t *testing.T) {
			hook()
			if readiness.Ready() {
				t.Fatal("late lifecycle hook restored readiness after Run exited")
			}
		})
	}
}

func TestListenerLifecycleHooksConcurrentWithExitStayInactive(t *testing.T) {
	readiness := listenerReadiness()
	stopErr := errors.New("stop")
	started := make(chan struct{})
	exit := make(chan struct{})
	var captured listenerClientConfig
	l := &Listener{
		Exec:      &Executor{Whitelist: map[string]bool{}},
		Readiness: readiness,
	}
	l.newClient = func(_, _ string, cfg listenerClientConfig) listenerClient {
		captured = cfg
		return fakeListenerClient{start: func(context.Context) error {
			cfg.OnReady()
			close(started)
			<-exit
			return stopErr
		}}
	}

	runDone := make(chan error, 1)
	go func() {
		runDone <- l.Run(context.Background())
	}()
	<-started

	hooks := []func(){
		captured.OnReady,
		captured.OnReconnected,
		captured.OnReconnecting,
		captured.OnDisconnected,
		func() { captured.OnError(errors.New("socket")) },
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				hooks[(offset+j)%len(hooks)]()
			}
		}(i)
	}
	close(exit)
	if err := <-runDone; !errors.Is(err, stopErr) {
		t.Fatalf("Run error = %v, want %v", err, stopErr)
	}
	wg.Wait()

	captured.OnReady()
	captured.OnReconnected()
	if readiness.Ready() {
		t.Fatal("lifecycle callbacks restored readiness after concurrent Run exit")
	}
}

func TestListenerNilCardAndReadinessPreserveMessageListener(t *testing.T) {
	stopErr := errors.New("stop")
	l := &Listener{Exec: &Executor{Whitelist: map[string]bool{}}}
	l.newClient = func(_, _ string, cfg listenerClientConfig) listenerClient {
		return fakeListenerClient{start: func(ctx context.Context) error {
			cfg.OnReady()
			cfg.OnReconnected()
			cfg.OnReconnecting()
			cfg.OnDisconnected()
			cfg.OnError(errors.New("socket"))
			for _, eventType := range []string{
				"im.message.receive_v1",
				"im.message.message_read_v1",
			} {
				if _, err := cfg.Handler.Do(ctx, listenerEventPayload(t, eventType)); err != nil {
					t.Fatalf("dispatch %s: %v", eventType, err)
				}
			}
			if _, err := cfg.Handler.Do(ctx, listenerCardPayload(t)); err == nil {
				t.Fatal("nil Card must not register card.action.trigger")
			}
			if cfg.LogLevel != listenerLogLevelError || !cfg.AutoReconnect {
				t.Fatalf("client config = %+v", cfg)
			}
			return stopErr
		}}
	}
	if err := l.Run(context.Background()); !errors.Is(err, stopErr) {
		t.Fatalf("Run error = %v, want %v", err, stopErr)
	}
}

func TestDedupCache(t *testing.T) {
	now := time.Date(2026, 7, 29, 1, 40, 0, 0, time.UTC)

	t.Run("首次通过,TTL 内重复拒绝", func(t *testing.T) {
		c := newDedupCache(10*time.Minute, 100)
		if !c.addIfNew("m1", now) {
			t.Fatal("首次应通过")
		}
		if c.addIfNew("m1", now.Add(5*time.Minute)) {
			t.Error("TTL 内重复应拒绝")
		}
		if !c.addIfNew("m2", now.Add(5*time.Minute)) {
			t.Error("不同 id 应通过")
		}
	})

	t.Run("TTL 过期后同 id 重新放行", func(t *testing.T) {
		c := newDedupCache(10*time.Minute, 100)
		c.addIfNew("m1", now)
		if !c.addIfNew("m1", now.Add(11*time.Minute)) {
			t.Error("过期后应重新放行")
		}
	})

	t.Run("容量上限淘汰最旧", func(t *testing.T) {
		c := newDedupCache(time.Hour, 3)
		c.addIfNew("m1", now)
		c.addIfNew("m2", now.Add(time.Second))
		c.addIfNew("m3", now.Add(2*time.Second))
		// 已满:加入 m4 时淘汰最旧的 m1
		if !c.addIfNew("m4", now.Add(3*time.Second)) {
			t.Fatal("新 id 应通过")
		}
		if !c.addIfNew("m1", now.Add(4*time.Second)) {
			t.Error("被淘汰的最旧 id 应视为新 id 放行")
		}
		// m1 重新入队时按同样规则淘汰了当前的队首 m2
		if !c.addIfNew("m2", now.Add(5*time.Second)) {
			t.Error("m2 已被 m1 的重新入队淘汰,应放行")
		}
		if c.addIfNew("m4", now.Add(6*time.Second)) {
			t.Error("未被淘汰的 id 仍应拒绝")
		}
	})

	t.Run("容量紧张时先清过期", func(t *testing.T) {
		c := newDedupCache(time.Minute, 2)
		c.addIfNew("old", now)
		c.addIfNew("m1", now.Add(50*time.Second))
		// old 已过期,应清它而不是 m1
		if !c.addIfNew("m2", now.Add(70*time.Second)) {
			t.Fatal("新 id 应通过")
		}
		if c.addIfNew("m1", now.Add(71*time.Second)) {
			t.Error("未过期项不应被误淘汰")
		}
	})

	t.Run("并发安全", func(t *testing.T) {
		c := newDedupCache(time.Hour, 10000)
		done := make(chan struct{})
		for g := 0; g < 8; g++ {
			go func(g int) {
				defer func() { done <- struct{}{} }()
				for i := 0; i < 200; i++ {
					c.addIfNew(fmt.Sprintf("g%d-m%d", g, i), now)
				}
			}(g)
		}
		for g := 0; g < 8; g++ {
			<-done
		}
		if len(c.seen) != 1600 {
			t.Errorf("seen = %d, want 1600", len(c.seen))
		}
	})
}
