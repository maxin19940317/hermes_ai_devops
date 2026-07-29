// relay — 独立 Outbox Relay 进程(docs/device-test-sequence.md 原则 3,
// 差距清单 #1):claim 未投递 outbox 行 → 发 Temporal Signal(至少一次)
// → 标记已投;失败重试并记录 last_error。
//
// 配置(环境变量,缺省值见 config.go):
//
//	TEMPORAL_ADDRESS     缺省 127.0.0.1:7233
//	DATABASE_URL         Postgres DSN;缺省用内存 store(仅开发,进程内无行可投)
//	RELAY_BATCH_SIZE     每轮 claim 上限,缺省 100
//	RELAY_POLL_INTERVAL  空转间隔,缺省 1s(Go duration)
//	RELAY_MAX_BACKOFF    claim 连续失败的退避上限,缺省 30s
//
// 积压监控(第四批):
//
//	RELAY_BACKLOG_INTERVAL   积压报告间隔,缺省 1m;设 0 关闭
//	RELAY_STUCK_ATTEMPTS     attempts >= 此值算"卡住",缺省 3
//	RELAY_BACKLOG_WARN_AGE   最老未投递行超过此年龄升级为 warn,缺省 5m
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"go.temporal.io/sdk/client"

	"hermes-devops/runtime/internal/relay"
	"hermes-devops/runtime/internal/store"
)

func main() {
	zerolog.TimeFieldFormat = "2006-01-02T15:04:05.000Z07:00" // UTC + 毫秒(§4)
	zerolog.TimestampFunc = func() time.Time { return time.Now().UTC() }
	log := zerolog.New(os.Stderr).With().Timestamp().Str("service", "relay").Logger()

	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		log.Fatal().Err(err).Msg("load config")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---- store:有 DATABASE_URL 用 Postgres,否则内存(仅开发) ----
	var st relay.Store
	if cfg.DatabaseURL != "" {
		pg, err := store.OpenPG(ctx, cfg.DatabaseURL)
		if err != nil {
			log.Fatal().Err(err).Msg("open postgres")
		}
		defer pg.DB.Close()
		st = pg
		log.Info().Msg("using postgres store")
	} else {
		st = store.NewMemStore()
		log.Warn().Msg("DATABASE_URL 未设置,内存 store 无行可投(仅开发)")
	}

	tc, err := client.Dial(client.Options{HostPort: cfg.TemporalAddress})
	if err != nil {
		log.Fatal().Err(err).Msg("dial temporal")
	}
	defer tc.Close()

	r := &relay.Relay{
		Store: st, Signaler: tc, Log: &log,
		BatchSize: cfg.BatchSize, PollInterval: cfg.PollInterval, MaxBackoff: cfg.MaxBackoff,
		BacklogInterval: cfg.BacklogInterval,
		StuckAttempts:   cfg.StuckAttempts,
		BacklogWarnAge:  cfg.BacklogWarnAge,
	}
	log.Info().Dur("poll_interval", cfg.PollInterval).Msg("outbox relay starting")
	if err := r.Run(ctx); err != nil {
		log.Fatal().Err(err).Msg("relay run")
	}
	log.Info().Msg("relay stopped")
}
