package main

import (
	"context"

	"github.com/rs/zerolog"
	"go.temporal.io/sdk/client"

	"hermes-devops/runtime/internal/activity"
	"hermes-devops/runtime/internal/callbacks"
	"hermes-devops/runtime/internal/cardaction"
	"hermes-devops/runtime/internal/feishu"
	"hermes-devops/runtime/internal/feishucmd"
	"hermes-devops/runtime/internal/rerun"
	"hermes-devops/runtime/internal/store"
	"hermes-devops/runtime/internal/trigger"
)

type workerStore interface {
	activity.Store
	callbacks.Store
	feishucmd.Store
	cardaction.Store
	cardaction.ConsumerStore
	cardaction.SweepStore
	rerun.Store
}

var (
	_ workerStore = (*store.MemStore)(nil)
	_ workerStore = (*store.PGStore)(nil)
)

type cardActionSweeperRunner interface {
	Run(context.Context)
}

type cardActionAssembly struct {
	Whitelist map[string]bool
	Starter   *trigger.TemporalStarter
	Executor  *feishucmd.Executor
	Resolver  *rerun.Resolver
	Consumer  *cardaction.Consumer
	Readiness *cardaction.Readiness
	Handler   *cardaction.Handler
	Sweeper   *cardaction.Sweeper
	Listener  *feishucmd.Listener

	sweeperRunner cardActionSweeperRunner
}

func assembleCardActions(
	lifecycleCtx context.Context,
	cfg Config,
	st workerStore,
	tc client.Client,
	sender feishu.Sender,
	feishuMode string,
	acts *activity.Acts,
	log *zerolog.Logger,
) *cardActionAssembly {
	whitelist := feishucmd.ParseWhitelist(cfg.Activity.FeishuCmdWhitelist)
	temporalStarter := &trigger.TemporalStarter{
		Client: tc, TaskQueue: cfg.TemporalTaskQueue,
	}
	resolver := &rerun.Resolver{Store: st, Starter: temporalStarter}
	consumer := &cardaction.Consumer{
		Store: st, Resolver: resolver, Starter: temporalStarter, Log: log,
	}
	listenerWired := len(whitelist) > 0 &&
		cfg.Activity.FeishuAppID != "" &&
		cfg.Activity.FeishuAppSecret != ""
	readiness := cardaction.NewReadiness(cardaction.ReadinessConfig{
		Enabled:           cfg.FeishuCardActionsEnabled,
		WhitelistNonEmpty: len(whitelist) > 0,
		SenderIsApp:       feishuMode == "app",
		HandlerWired:      listenerWired,
	})
	handler := &cardaction.Handler{
		Store: st, Readiness: readiness, Whitelist: whitelist,
		AppID: cfg.Activity.FeishuAppID, Log: log,
	}
	handler.Consume = func(eventID string) {
		if err := consumer.ConsumeOne(lifecycleCtx, eventID); err != nil && log != nil {
			log.Error().
				Err(err).
				Str("event_id", eventID).
				Msg("consume card action")
		}
	}

	var updater feishu.CardUpdater
	if feishuMode == "app" {
		updater, _ = sender.(feishu.CardUpdater)
	}
	sweeper := &cardaction.Sweeper{
		Store: st, Consumer: consumer, Starter: temporalStarter,
		Updater: updater, Log: log,
	}
	executor := &feishucmd.Executor{
		Store: st, Sender: sender, Log: log,
		Whitelist: whitelist, Starter: temporalStarter,
	}

	var listener *feishucmd.Listener
	if listenerWired {
		listener = &feishucmd.Listener{
			AppID: cfg.Activity.FeishuAppID, AppSecret: cfg.Activity.FeishuAppSecret,
			Exec: executor, Card: handler, Readiness: readiness,
		}
	}
	if acts != nil {
		acts.CardActions = readiness
	}
	return &cardActionAssembly{
		Whitelist: whitelist,
		Starter:   temporalStarter,
		Executor:  executor,
		Resolver:  resolver,
		Consumer:  consumer,
		Readiness: readiness,
		Handler:   handler,
		Sweeper:   sweeper,
		Listener:  listener,

		sweeperRunner: sweeper,
	}
}

func (a *cardActionAssembly) startSweeper(ctx context.Context) {
	if a == nil || a.sweeperRunner == nil {
		return
	}
	go a.sweeperRunner.Run(ctx)
}
