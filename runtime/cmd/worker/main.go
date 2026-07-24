// worker — Phase 1.6 Runtime worker 进程(CLAUDE.md §12.6)。
// 装配 Temporal worker(DeviceTestWorkflow + 全部活动)与 Client 回调 HTTP 服务
// (contracts/callbacks-api.openapi.yaml),共享同一个 store。
//
// 配置(环境变量,缺省值见 §10,详见 config.go):
//
//	TEMPORAL_ADDRESS        缺省 127.0.0.1:7233
//	TEMPORAL_TASK_QUEUE     缺省 device-test(须与 trigger 一致)
//	DATABASE_URL            Postgres DSN;缺省用内存 store(仅开发)
//	WORKER_CALLBACKS_ADDR   Client 回调服务监听地址,缺省 :8091
//	VARIANTS_CONFIG         ci/variants.yaml 路径(必填)
//	CALLBACK_BASE_URL       派单载荷 callback_base_url(必填,§8.1)
//	LEASE_SECONDS           任务租约,缺省 120
//	QUARANTINE_AFTER        设备隔离阈值,缺省 3
//	MAX_INFRA_RETRIES       INFRA 机械重试上限,缺省 2
//	HARD_TIMEOUT_MARGIN_SEC 硬超时叠加在 manifest timeout 上的余量,缺省 1200
//	DEVICE_WAIT_ROUNDS      设备忙时等待轮数,缺省 20
//	DEVICE_WAIT_SECONDS     每轮等待秒数,缺省 30
//	ARTIFACT_AUTH_TYPE      缺省 job_token
//	ARTIFACT_AUTH_TOKEN     可选
//	FEISHU_WEBHOOK_URL      可选;缺省 Notify 静默成功(开发模式)
//	MINIO_ENDPOINT          集群内 endpoint(如 minio:9000);空 → 禁用预签名(§3.7 降级)
//	MINIO_PUBLIC_ENDPOINT   预签名 URL 的 host,须 Client 可达;空 → 用 MINIO_ENDPOINT
//	MINIO_ACCESS_KEY        空 → 禁用预签名
//	MINIO_SECRET_KEY        空 → 禁用预签名
//	MINIO_BUCKET            缺省 hermes-evidence
//	MINIO_PRESIGN_TTL       缺省 1h(Go duration)
//	HERMES_ENDPOINT         hermes-agent 平台调用 URL(§12 Phase 2);空 → Analyzer 禁用,规则引擎保底
//	HERMES_AUTH_TOKEN       可选,Bearer
//	HERMES_TIMEOUT_SEC      Analyzer 调用超时,缺省 60
//	HERMES_MODEL            可选透传;模型主体由平台配置(§4)
package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"hermes-devops/runtime/internal/activity"
	"hermes-devops/runtime/internal/callbacks"
	"hermes-devops/runtime/internal/hermesclient"
	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

func main() {
	zerolog.TimeFieldFormat = "2006-01-02T15:04:05.000Z07:00" // UTC + 毫秒(§4)
	zerolog.TimestampFunc = func() time.Time { return time.Now().UTC() }
	log := zerolog.New(os.Stderr).With().Timestamp().Str("service", "worker").Logger()

	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		log.Fatal().Err(err).Msg("load config")
	}

	specCfg, err := activity.LoadSpecConfig(cfg.VariantsConfigPath, cfg.SpecDefaults)
	if err != nil {
		log.Fatal().Err(err).Msg("load variants config")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---- store:有 DATABASE_URL 用 Postgres,否则内存(仅开发) ----
	var st interface {
		activity.Store
		callbacks.Store
	}
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
		log.Warn().Msg("DATABASE_URL 未设置,store 仅在内存(重启即失)")
	}

	// ---- Temporal ----
	tc, err := client.Dial(client.Options{HostPort: cfg.TemporalAddress})
	if err != nil {
		log.Fatal().Err(err).Msg("dial temporal")
	}
	defer tc.Close()

	// ---- Phase 2 Analyzer(§12):HERMES_ENDPOINT 空 → NewHTTPClient 返回 nil,
	// Analyzer 禁用,verdict 由规则引擎保底 ----
	var hermes hermesclient.Client
	if h := hermesclient.NewHTTPClient(hermesclient.Config{
		Endpoint:  cfg.Activity.HermesEndpoint,
		AuthToken: cfg.Activity.HermesAuthToken,
		Timeout:   cfg.Activity.HermesTimeout,
	}); h != nil {
		hermes = h
		log.Info().Msg("hermes analyzer enabled")
	} else {
		log.Info().Msg("HERMES_ENDPOINT 未设置,Analyzer 禁用,规则引擎保底")
	}

	acts := &activity.Acts{
		Store:   st,
		Cfg:     cfg.Activity,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		SpecCfg: specCfg,
		Log:     &log,
		Hermes:  hermes,
	}

	w := worker.New(tc, cfg.TemporalTaskQueue, worker.Options{})
	w.RegisterWorkflowWithOptions(wf.DeviceTestWorkflow, workflow.RegisterOptions{
		Name: wf.DeviceTestWorkflowName,
	})
	w.RegisterActivity(acts)

	// ---- Client 回调 HTTP 服务(§8.2) ----
	cb := callbacks.New(st, tc, &log, cfg.Activity.LeaseSeconds)
	callbackSrv := &http.Server{
		Addr:              cfg.CallbacksAddr,
		Handler:           cb.Mux(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	// 先同步 bind 端口再进入运行循环:绑定失败(如端口占用)在这里直接 fail fast,
	// 不必等到 goroutine 内部才发现——避免了在后台 goroutine 里调用 log.Fatal()
	// 直接 os.Exit,把 Temporal worker 的正常关闭路径整个跳过。
	ln, err := net.Listen("tcp", cfg.CallbacksAddr)
	if err != nil {
		log.Fatal().Err(err).Str("addr", cfg.CallbacksAddr).Msg("listen callbacks addr")
	}

	// callbackServed 在 Serve 返回后关闭(即 HTTP 服务已完全排空),
	// main() 必须等它关闭才能退出进程——否则 SIGTERM 到达时,w.Run() 可能先于
	// callbackSrv.Shutdown() 完成而让 main() 提前返回,中断正在处理的 /callbacks/v1/*
	// 请求(§8.2 回调虽然可安全重发,但不应该被进程退出无谓打断)。
	callbackServed := make(chan struct{})
	go func() {
		defer close(callbackServed)
		log.Info().Str("addr", cfg.CallbacksAddr).Msg("callbacks service listening")
		if err := callbackSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error().Err(err).Msg("serve callbacks")
		}
	}()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = callbackSrv.Shutdown(shutdownCtx)
	}()

	log.Info().Str("task_queue", cfg.TemporalTaskQueue).Msg("temporal worker starting")
	runErr := w.Run(worker.InterruptCh())
	<-callbackServed
	if runErr != nil {
		log.Fatal().Err(runErr).Msg("worker run")
	}
	log.Info().Msg("worker stopped")
}
