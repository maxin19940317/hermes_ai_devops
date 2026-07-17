// trigger — Phase 1.5 Trigger 服务(CLAUDE.md §12.5)。
// GitLab pipeline webhook → 拉 bundle → 登记 artifacts → 启动 DeviceTestWorkflow。
//
// 配置(环境变量):
//
//	TRIGGER_ADDR          监听地址,缺省 :8090
//	TRIGGER_WEBHOOK_SECRET  GitLab webhook Secret Token(必填)
//	TRIGGER_REFS          逗号分隔分支白名单,缺省 master(tag 事件总是放行)
//	GITLAB_BASE_URL       如 https://gitlab.example(必填)
//	GITLAB_TOKEN          read_api 访问令牌(必填)
//	GITLAB_TOKEN_HEADER   缺省 PRIVATE-TOKEN(Deploy Token 用 Deploy-Token)
//	PACKAGE_NAME          Generic 包名,缺省 algo-super-sdk
//	TEMPORAL_ADDRESS      缺省 127.0.0.1:7233
//	TEMPORAL_TASK_QUEUE   缺省 device-test
//	DATABASE_URL          Postgres DSN;缺省用内存 store(仅开发)
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"go.temporal.io/sdk/client"

	"hermes-devops/runtime/internal/store"
	"hermes-devops/runtime/internal/trigger"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	zerolog.TimeFieldFormat = "2006-01-02T15:04:05.000Z07:00" // UTC + 毫秒(§4)
	zerolog.TimestampFunc = func() time.Time { return time.Now().UTC() }
	log := zerolog.New(os.Stderr).With().Timestamp().Str("service", "trigger").Logger()

	secret := os.Getenv("TRIGGER_WEBHOOK_SECRET")
	gitlabBase := os.Getenv("GITLAB_BASE_URL")
	gitlabToken := os.Getenv("GITLAB_TOKEN")
	if secret == "" || gitlabBase == "" || gitlabToken == "" {
		log.Fatal().Msg("TRIGGER_WEBHOOK_SECRET / GITLAB_BASE_URL / GITLAB_TOKEN 必填")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---- store:有 DATABASE_URL 用 Postgres,否则内存(仅开发) ----
	var artifacts store.ArtifactStore
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		pg, err := store.OpenPG(ctx, dsn)
		if err != nil {
			log.Fatal().Err(err).Msg("open postgres")
		}
		defer pg.DB.Close()
		artifacts = pg
		log.Info().Msg("using postgres artifact store")
	} else {
		artifacts = store.NewMemStore()
		log.Warn().Msg("DATABASE_URL 未设置,artifacts 登记仅在内存(重启即失)")
	}

	// ---- Temporal ----
	tc, err := client.Dial(client.Options{HostPort: env("TEMPORAL_ADDRESS", "127.0.0.1:7233")})
	if err != nil {
		log.Fatal().Err(err).Msg("dial temporal")
	}
	defer tc.Close()

	h := trigger.New(trigger.Config{
		WebhookSecret: secret,
		Refs:          strings.Split(env("TRIGGER_REFS", "master"), ","),
		Logger:        &log,
	}, &trigger.GitLabClient{
		BaseURL:     strings.TrimRight(gitlabBase, "/"),
		Token:       gitlabToken,
		TokenHeader: env("GITLAB_TOKEN_HEADER", "PRIVATE-TOKEN"),
		PackageName: env("PACKAGE_NAME", "algo-super-sdk"),
		HTTP:        &http.Client{Timeout: 60 * time.Second},
	}, artifacts, &trigger.TemporalStarter{
		Client:    tc,
		TaskQueue: env("TEMPORAL_TASK_QUEUE", "device-test"),
	})

	mux := http.NewServeMux()
	mux.Handle("/webhooks/gitlab", h)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              env("TRIGGER_ADDR", ":8090"),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second, // 含 bundle 拉取
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Info().Str("addr", srv.Addr).Msg("trigger service listening")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal().Err(err).Msg("serve")
	}
	log.Info().Msg("trigger service stopped")
}
