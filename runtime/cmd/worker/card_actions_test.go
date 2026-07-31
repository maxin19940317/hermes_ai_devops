package main

import (
	"context"
	"testing"
	"time"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	"github.com/rs/zerolog"

	"hermes-devops/runtime/internal/activity"
	"hermes-devops/runtime/internal/store"
)

type testContextKey string

type recordingConsumerStore struct {
	ctx chan context.Context
}

func (s *recordingConsumerStore) ClaimInbox(
	ctx context.Context, _, _ string, _ time.Duration,
) (*store.InboxRow, error) {
	s.ctx <- ctx
	return nil, store.ErrInboxNotClaimable
}

func (*recordingConsumerStore) GetCardAction(context.Context, string) (*store.CardAction, error) {
	panic("unexpected GetCardAction")
}

func (*recordingConsumerStore) CompleteAccept(
	context.Context, store.AcceptRequest,
) (*store.AcceptOutcome, error) {
	panic("unexpected CompleteAccept")
}

func (*recordingConsumerStore) CompleteReject(
	context.Context, string, string, store.RejectRender,
) error {
	panic("unexpected CompleteReject")
}

func (*recordingConsumerStore) FinalizeAction(
	context.Context, string, string, string, string,
) (bool, error) {
	panic("unexpected FinalizeAction")
}

type fakeSweeperRunner struct {
	started chan context.Context
	stopped chan struct{}
}

func (s *fakeSweeperRunner) Run(ctx context.Context) {
	s.started <- ctx
	<-ctx.Done()
	close(s.stopped)
}

type fakeAppSender struct{}

func (*fakeAppSender) SendText(context.Context, string) error { return nil }
func (*fakeAppSender) PatchCard(context.Context, string, any) error {
	return nil
}

type fakeWebhookSender struct{}

func (*fakeWebhookSender) SendText(context.Context, string) error { return nil }

func workerCardConfig() Config {
	return Config{
		TemporalTaskQueue:        "device-test",
		FeishuCardActionsEnabled: true,
		Activity: activity.Config{
			FeishuAppID:        "cli_a",
			FeishuAppSecret:    "secret",
			FeishuCmdWhitelist: "ou_a",
		},
	}
}

func validWorkerCardEvent() *callback.CardActionTriggerEvent {
	tenant := "tenant_a"
	return &callback.CardActionTriggerEvent{
		EventV2Base: &larkevent.EventV2Base{
			Schema: "2.0",
			Header: &larkevent.EventHeader{
				EventID:   "evt_worker",
				EventType: "card.action.trigger",
				AppID:     "cli_a",
				TenantKey: tenant,
			},
		},
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{TenantKey: &tenant, OpenID: "ou_a"},
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
	}
}

func TestAssembleCardActionsReusesDependencies(t *testing.T) {
	cfg := workerCardConfig()
	st := store.NewMemStore()
	acts := &activity.Acts{}
	log := zerolog.Nop()
	app := &fakeAppSender{}

	got := assembleCardActions(
		context.Background(), cfg, st, nil, app, "app", acts, &log,
	)

	if got.Listener == nil || got.Listener.Card != got.Handler ||
		got.Listener.Readiness != got.Readiness {
		t.Fatalf("listener wiring = %#v", got.Listener)
	}
	if got.Handler.Readiness != got.Readiness || acts.CardActions != got.Readiness {
		t.Fatal("Acts, Handler, and Listener must share the exact Readiness pointer")
	}
	got.Handler.Whitelist["ou_b"] = true
	if !got.Executor.Whitelist["ou_b"] {
		t.Fatal("Executor and Handler must share the exact whitelist map")
	}
	if got.Executor.Starter != got.Starter ||
		got.Resolver.Starter != got.Starter ||
		got.Consumer.Starter != got.Starter ||
		got.Sweeper.Starter != got.Starter {
		t.Fatal("Executor, Resolver, Consumer, and Sweeper must share one TemporalStarter")
	}
	if got.Consumer.Resolver != got.Resolver {
		t.Fatal("Consumer must use the assembled rerun Resolver")
	}
}

func TestAssembleCardActionReadinessFactors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		mode   string
		ws     bool
		want   bool
	}{
		{name: "all ready", mode: "app", ws: true, want: true},
		{name: "default disabled", mode: "app", ws: true, mutate: func(c *Config) {
			c.FeishuCardActionsEnabled = false
		}},
		{name: "empty whitelist", mode: "app", ws: true, mutate: func(c *Config) {
			c.Activity.FeishuCmdWhitelist = ""
		}},
		{name: "webhook sender", mode: "webhook", ws: true},
		{name: "listener unwired", mode: "app", ws: true, mutate: func(c *Config) {
			c.Activity.FeishuAppSecret = ""
		}},
		{name: "websocket down", mode: "app"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := workerCardConfig()
			if tt.mutate != nil {
				tt.mutate(&cfg)
			}
			got := assembleCardActions(
				context.Background(), cfg, store.NewMemStore(), nil,
				&fakeAppSender{}, tt.mode, &activity.Acts{}, nil,
			)
			got.Readiness.SetWS(tt.ws)
			if ready := got.Readiness.Ready(); ready != tt.want {
				t.Fatalf("Ready() = %v, want %v", ready, tt.want)
			}
			if tt.name == "default disabled" &&
				(got.Listener == nil || got.Listener.Card != got.Handler) {
				t.Fatal("disabled feature must still register Handler on an eligible listener")
			}
		})
	}
}

func TestHandlerConsumeUsesLifecycleContext(t *testing.T) {
	lifecycleCtx := context.WithValue(
		context.Background(), testContextKey("scope"), "worker-lifecycle",
	)
	got := assembleCardActions(
		lifecycleCtx, workerCardConfig(), store.NewMemStore(), nil,
		&fakeAppSender{}, "app", &activity.Acts{}, nil,
	)
	recording := &recordingConsumerStore{ctx: make(chan context.Context, 1)}
	got.Consumer.Store = recording
	got.Handler.Store = store.NewMemStore()
	got.Readiness.SetWS(true)

	callbackCtx, cancelCallback := context.WithCancel(context.Background())
	cancelCallback()
	resp, err := got.Handler.OnCardAction(callbackCtx, validWorkerCardEvent())
	if err != nil {
		t.Fatalf("OnCardAction: %v", err)
	}
	if resp.Toast == nil || resp.Toast.Content != "已收到，正在处理" {
		t.Fatalf("response = %#v", resp)
	}
	select {
	case consumeCtx := <-recording.ctx:
		if consumeCtx.Err() != nil {
			t.Fatalf("consumer context already canceled: %v", consumeCtx.Err())
		}
		if consumeCtx.Value(testContextKey("scope")) != "worker-lifecycle" {
			t.Fatal("consumer did not receive process/listener lifecycle context")
		}
	case <-time.After(time.Second):
		t.Fatal("Handler.Consume did not invoke Consumer.ConsumeOne")
	}
}

func TestSweeperStartsWithoutListenerOrFeatureFlagAndStopsOnCancel(t *testing.T) {
	cfg := workerCardConfig()
	cfg.FeishuCardActionsEnabled = false
	cfg.Activity.FeishuCmdWhitelist = ""
	got := assembleCardActions(
		context.Background(), cfg, store.NewMemStore(), nil,
		&fakeWebhookSender{}, "webhook", &activity.Acts{}, nil,
	)
	if got.Listener != nil {
		t.Fatal("empty whitelist must leave listener disabled")
	}
	fake := &fakeSweeperRunner{
		started: make(chan context.Context, 1),
		stopped: make(chan struct{}),
	}
	got.sweeperRunner = fake
	ctx, cancel := context.WithCancel(context.Background())
	got.startSweeper(ctx)
	select {
	case startedCtx := <-fake.started:
		if startedCtx != ctx {
			t.Fatal("sweeper must receive the process context")
		}
	case <-time.After(time.Second):
		t.Fatal("sweeper did not start while listener/feature were disabled")
	}
	cancel()
	select {
	case <-fake.stopped:
	case <-time.After(time.Second):
		t.Fatal("sweeper did not stop on process cancellation")
	}
}

func TestAssembleCardActionsSelectsUpdaterBySenderMode(t *testing.T) {
	t.Run("app", func(t *testing.T) {
		app := &fakeAppSender{}
		got := assembleCardActions(
			context.Background(), workerCardConfig(), store.NewMemStore(), nil,
			app, "app", &activity.Acts{}, nil,
		)
		if got.Sweeper.Updater != app {
			t.Fatal("app sender must supply CardUpdater")
		}
	})
	t.Run("webhook", func(t *testing.T) {
		got := assembleCardActions(
			context.Background(), workerCardConfig(), store.NewMemStore(), nil,
			&fakeWebhookSender{}, "webhook", &activity.Acts{}, nil,
		)
		if got.Sweeper.Updater != nil {
			t.Fatal("webhook sender must not be forced to implement CardUpdater")
		}
	})
}
