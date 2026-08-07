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
//	ARTIFACT_AUTH_TYPE      缺省 job_token(bearer | job_token | basic;basic 用于只读 Deploy Token,原则 5)
//	ARTIFACT_AUTH_TOKEN     可选
//	ARTIFACT_AUTH_USERNAME  可选;仅 basic(Deploy Token 用户名)
//	FEISHU_WEBHOOK_URL      可选;群自定义机器人 webhook(双模兜底)
//	FEISHU_APP_ID           可选;企业自建应用(三件套齐全时优先于 webhook)
//	FEISHU_APP_SECRET       可选;同上
//	FEISHU_RECEIVE_ID       可选;接收方 open_id(个人单聊)或 chat_id(群)
//	FEISHU_RECEIVE_ID_TYPE  可选;chat_id|open_id,缺省 chat_id
//	FEISHU_CMD_WHITELIST    可选;指令 listener 白名单(逗号分隔 open_id),空 = 不启动
//	ESCALATION_ENDPOINT     可选;kanban_bridge URL,空 = 升级禁用(现状)
//	ESCALATION_TOKEN        可选;bridge Bearer 共享密钥
//	ESCALATION_MIN_CONFIDENCE 可选;hermes 置信度门槛,缺省 0.7
//	FEISHU_CMD_NL           可选;飞书指令自然语言翻译旁路总开关,缺省 false(灰度)。
//	                        启用需三者合取:=true && HERMES_ENDPOINT 非空 && FEISHU_CMD_WHITELIST 非空
//	FEISHU_CMD_NL_TIMEOUT_SEC 可选;/translate 调用超时,缺省 60(不复用 HERMES_TIMEOUT_SEC)
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
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"hermes-devops/runtime/internal/activity"
	"hermes-devops/runtime/internal/callbacks"
	"hermes-devops/runtime/internal/cmdapi"
	"hermes-devops/runtime/internal/feishu"
	"hermes-devops/runtime/internal/feishucmd"
	"hermes-devops/runtime/internal/hermesclient"
	"hermes-devops/runtime/internal/mtls"
	"hermes-devops/runtime/internal/presign"
	"hermes-devops/runtime/internal/store"
	"hermes-devops/runtime/internal/trigger"
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
		feishucmd.Store
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

	httpClient := &http.Client{Timeout: 30 * time.Second}
	// Phase 3 mTLS: Client 侧证书用于 Dispatch 活动访问 Agent HTTPS 端点。
	if tr, err := mtls.ClientTransport(
		cfg.Activity.MTLSCAFile, cfg.Activity.MTLSCertFile,
	); err != nil {
		log.Fatal().Err(err).Msg("mtls client transport")
	} else if tr != nil {
		httpClient.Transport = tr
		log.Info().Msg("dispatch: mTLS client cert enabled")
	}

	acts := &activity.Acts{
		Store:   st,
		Cfg:     cfg.Activity,
		HTTP:    httpClient,
		SpecCfg: specCfg,
		Log:     &log,
		Hermes:  hermes,
	}

	// ---- 飞书双模:app 凭据齐全 → 企业自建应用;否则 webhook 兜底;
	// 全空 → disabled(Notify 静默成功,开发模式) ----
	feishuSender, feishuMode := feishu.NewSender(feishu.Config{
		AppID:         cfg.Activity.FeishuAppID,
		AppSecret:     cfg.Activity.FeishuAppSecret,
		ReceiveID:     cfg.Activity.FeishuReceiveID,
		ReceiveIDType: cfg.Activity.FeishuReceiveIDType,
		WebhookURL:    cfg.Activity.FeishuWebhookURL,
	})
	acts.Feishu = feishuSender
	log.Info().Str("mode", feishuMode).Msg("feishu notify mode")

	// ---- 飞书指令执行器(飞书 listener 与受控命令接口 cmdapi 共用) ----
	// Executor 是全部指令逻辑的唯一实现:只读查询(status/devices/runs/result/
	// metrics/artifacts)与副作用指令(test/rerun/quarantine/unquarantine/cancel)
	// 都在这里。飞书 listener 走 open_id 白名单;cmdapi 走 Bearer Token,二者
	// 复用同一 execute 路径,保证行为一致(2026-08-07)。
	exec := &feishucmd.Executor{
		Store: st, Log: &log,
		Starter:  &trigger.TemporalStarter{Client: tc, TaskQueue: cfg.TemporalTaskQueue},
		Variants: specCfg.VariantNames(),
		SpecCfg:  specCfg,
	}

	// ---- 飞书指令 listener:白名单(FEISHU_CMD_WHITELIST)非空才启动;
	// 需要 app 凭据(与通知共用)——长连接事件订阅只收白名单 open_id 的单聊指令 ----
	if wl := feishucmd.ParseWhitelist(cfg.Activity.FeishuCmdWhitelist); len(wl) > 0 {
		if cfg.Activity.FeishuAppID == "" || cfg.Activity.FeishuAppSecret == "" {
			log.Warn().Msg("FEISHU_CMD_WHITELIST 已配置但缺 FEISHU_APP_ID/SECRET,listener=disabled")
		} else {
			exec.Sender = feishuSender
			exec.Whitelist = wl
			// 卡片回复能力(app/webhook 发送方都实现 CardSender;类型断言失败 → nil,
			// devices 等查询回落纯文本)。
			if cs, ok := feishuSender.(feishu.CardSender); ok {
				exec.CardSender = cs
				log.Info().Msg("feishu cmd card reply=enabled")
			}
			// 表述层(Express,Smart Reply):HERMES_ENDPOINT 已配时启用
			// (独立 hermes client,与翻译共用端点/认证);模型独立配置,空回落翻译层模型。
			if cfg.Activity.HermesEndpoint != "" {
				expressModel := cfg.Activity.HermesExpressModel
				if expressModel == "" {
					expressModel = cfg.Activity.HermesModel
				}
				exec.Express = hermesclient.NewHTTPClient(hermesclient.Config{
					Endpoint:  cfg.Activity.HermesEndpoint,
					AuthToken: cfg.Activity.HermesAuthToken,
					Timeout:   cfg.Activity.FeishuCmdNLTimeout,
				})
				exec.ExpressModel = expressModel
				log.Info().Str("model", expressModel).Msg("feishu cmd express=enabled")
			}
			// 自然语言翻译旁路(设计文档 §3.1):三个条件合取才启用——
			// 开关打开、bridge 端点已配、指令 listener 本身已启用。
			nlReason := ""
			switch {
			case !cfg.Activity.FeishuCmdNL:
				nlReason = "FEISHU_CMD_NL != true"
			case cfg.Activity.HermesEndpoint == "":
				nlReason = "HERMES_ENDPOINT empty"
			}
			if nlReason == "" {
				nlClient := hermesclient.NewHTTPClient(hermesclient.Config{
					Endpoint:  cfg.Activity.HermesEndpoint,
					AuthToken: cfg.Activity.HermesAuthToken,
					Timeout:   cfg.Activity.FeishuCmdNLTimeout,
				})
				exec.Translator = &feishucmd.Translator{
					Client:   nlClient,
					Store:    st,
					Variants: specCfg.VariantNames(),
					Model:    cfg.Activity.HermesModel,
					Log:      &log,
				}
				// Planner 与 Translator 共用同一 hermes client:
				// 同一端点、同一认证、同一超时。
				exec.Planner = nlClient
				log.Info().Dur("timeout", cfg.Activity.FeishuCmdNLTimeout).Msg("feishu cmd nl=enabled")
			} else {
				// NL 关闭时 Planner 仍可独立启用(规划不需要 NL 翻译能力)。
				log.Info().Str("reason", nlReason).Msg("feishu cmd nl=disabled")
			}
			// Planner 独立于 NL 翻译:即使 NL 关着,只要 hermes 端点可配就启用规划。
			if exec.Planner == nil && cfg.Activity.HermesEndpoint != "" {
				planClient := hermesclient.NewHTTPClient(hermesclient.Config{
					Endpoint:  cfg.Activity.HermesEndpoint,
					AuthToken: cfg.Activity.HermesAuthToken,
					Timeout:   cfg.Activity.FeishuCmdNLTimeout,
				})
				exec.Planner = planClient
				log.Info().Msg("feishu cmd planner=enabled(独立于 NL)")
			}
			listener := &feishucmd.Listener{
				AppID: cfg.Activity.FeishuAppID, AppSecret: cfg.Activity.FeishuAppSecret, Exec: exec,
			}
			go func() {
				if err := listener.Run(ctx); err != nil && ctx.Err() == nil {
					log.Error().Err(err).Msg("feishu cmd listener exited")
				}
			}()
			log.Info().Int("whitelist", len(wl)).Msg("feishu cmd listener=enabled")
		}
	} else {
		log.Info().Msg("feishu cmd listener=disabled (FEISHU_CMD_WHITELIST empty)")
	}

	w := worker.New(tc, cfg.TemporalTaskQueue, worker.Options{})
	w.RegisterWorkflowWithOptions(wf.DeviceTestWorkflow, workflow.RegisterOptions{
		Name: wf.DeviceTestWorkflowName,
	})
	w.RegisterWorkflowWithOptions(wf.EvidenceLifecycleWorkflow, workflow.RegisterOptions{
		Name: wf.EvidenceLifecycleWorkflowName,
	})
	w.RegisterActivity(acts)

	// ---- MinIO 生命周期清理:每天 UTC 3:00 扫一次 ----
	scheduleClient := tc.ScheduleClient()
	scheduleHandle, err := scheduleClient.Create(ctx, client.ScheduleOptions{
		ID: "evidence-lifecycle-daily",
		Spec: client.ScheduleSpec{
			CronExpressions: []string{"0 3 * * *"},
		},
		Action: &client.ScheduleWorkflowAction{
			ID:        "evidence-lifecycle",
			Workflow:  wf.EvidenceLifecycleWorkflowName,
			TaskQueue: cfg.TemporalTaskQueue,
		},
		Overlap: enumspb.SCHEDULE_OVERLAP_POLICY_SKIP,
	})
	if err != nil {
		if !errors.Is(err, temporal.ErrScheduleAlreadyRunning) {
			log.Warn().Err(err).Msg("temporal: create evidence-lifecycle schedule failed")
		}
	} else {
		log.Info().Str("id", scheduleHandle.GetID()).Msg("temporal: evidence-lifecycle schedule created")
	}

	// ---- Client 回调 HTTP 服务(§8.2) ----
	cb := callbacks.New(st, tc, &log, cfg.Activity.LeaseSeconds).WithDeviceCaps(cfg.DeviceCapabilities).WithSOCAliases(cfg.DeviceSOCAliases)
	// 按需签发(差距 #8):MinIO 未配置时 Presign 为 nil,端点返回 503,Agent 回退。
	if signer, err := presign.NewSigner(presign.Config{
		Endpoint: cfg.Activity.MinIOEndpoint, PublicEndpoint: cfg.Activity.MinIOPublicEndpoint,
		AccessKey: cfg.Activity.MinIOAccessKey, SecretKey: cfg.Activity.MinIOSecretKey,
		Bucket: cfg.Activity.MinIOBucket, TTL: cfg.Activity.MinIOPresignTTL,
	}); err != nil {
		log.Warn().Err(err).Msg("presign signer init failed; upload-requests will return 503")
	} else {
		cb.Presign = signer
	}
	cb.UploadMaxFiles = cfg.Activity.UploadMaxFiles

	// ---- 受控命令接口(cmdapi):Hermes/MCP 侧的结构化指令通道(2026-08-07) ----
	// POST /api/v1/cmd {command,args} → 复用 feishucmd.Executor 执行逻辑。
	// Bearer 鉴权(CMD_API_TOKEN);Token 空 = 接口未启用(401)。
	// 挂在 callbacks 同一 listener 上(共享 8091 端口与 mTLS),避免新增暴露面。
	// cmdapi 用独立的 TextOnly Executor 副本:无卡片、无飞书发送(devices 等
	// 卡片优先指令返回文本),与飞书 listener 的共享 exec 零竞态。
	cmdExec := *exec
	cmdExec.TextOnly = true
	cmdExec.CardSender = nil
	cmdExec.Sender = nil
	cmdExec.Whitelist = nil
	cmdMux := cb.Mux()
	cmdMux.Handle("/api/v1/cmd", &cmdapi.Handler{
		Token: cfg.Activity.CmdAPIToken,
		Exec:  &cmdExec,
	})
	callbackSrv := &http.Server{
		Addr:              cfg.CallbacksAddr,
		Handler:           cmdMux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	ln, err := net.Listen("tcp", cfg.CallbacksAddr)
	if err != nil {
		log.Fatal().Err(err).Str("addr", cfg.CallbacksAddr).Msg("listen callbacks addr")
	}
	// Phase 3 mTLS(§12):三件套齐全时在 listener 上加 TLS 层,
	// 否则保持纯 HTTP(兼容旧 Agent / 开发环境)。
	if tlsCfg, err := mtls.ServerConfig(
		cfg.Activity.MTLSCAFile, cfg.Activity.MTLSCertFile, cfg.Activity.MTLSKeyFile,
	); err != nil {
		log.Fatal().Err(err).Msg("mtls server config")
	} else if tlsCfg != nil {
		ln = tls.NewListener(ln, tlsCfg)
		log.Info().Msg("callbacks: mTLS enabled (client cert required)")
	} else {
		log.Warn().Msg("callbacks: mTLS not configured, using plain HTTP")
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
