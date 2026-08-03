package main

import (
	"fmt"
	"strconv"
	"time"

	"hermes-devops/runtime/internal/activity"
)

// Config 是 worker 进程装配所需的全部参数,由环境变量派生(§12.6)。
// 缺省值取 CLAUDE.md §10;必填项缺失时 fail fast(不吞错误静默用错误配置启动)。
type Config struct {
	TemporalAddress    string
	TemporalTaskQueue  string
	DatabaseURL        string // 空 → 内存 store(仅开发,重启即失)
	CallbacksAddr      string
	VariantsConfigPath string
	Activity           activity.Config
	SpecDefaults       activity.SpecDefaults
}

// loadConfig 从 getenv(通常是 os.Getenv)派生 Config;
// 以函数注入而非直接读 os.Environ 是为了让配置解析可单测。
func loadConfig(getenv func(string) string) (Config, error) {
	env := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	envInt := func(key string, def int) (int, error) {
		v := getenv(key)
		if v == "" {
			return def, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("%s: 非法整数 %q: %w", key, v, err)
		}
		return n, nil
	}
	envDuration := func(key string, def time.Duration) (time.Duration, error) {
		v := getenv(key)
		if v == "" {
			return def, nil
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return 0, fmt.Errorf("%s: 非法时长 %q: %w", key, v, err)
		}
		return d, nil
	}
	envFloat := func(key string, def float64) (float64, error) {
		v := getenv(key)
		if v == "" {
			return def, nil
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: 非法浮点数 %q: %w", key, v, err)
		}
		return f, nil
	}

	variantsPath := getenv("VARIANTS_CONFIG")
	if variantsPath == "" {
		return Config{}, fmt.Errorf("VARIANTS_CONFIG 必填(variants.yaml 路径)")
	}
	callbackBaseURL := getenv("CALLBACK_BASE_URL")
	if callbackBaseURL == "" {
		return Config{}, fmt.Errorf("CALLBACK_BASE_URL 必填(派单载荷 callback_base_url,§8.1)")
	}

	leaseSeconds, err := envInt("LEASE_SECONDS", 120)
	if err != nil {
		return Config{}, err
	}
	quarantineAfter, err := envInt("QUARANTINE_AFTER", 3)
	if err != nil {
		return Config{}, err
	}
	maxInfraRetries, err := envInt("MAX_INFRA_RETRIES", 2)
	if err != nil {
		return Config{}, err
	}
	hardTimeoutMargin, err := envInt("HARD_TIMEOUT_MARGIN_SEC", 1200)
	if err != nil {
		return Config{}, err
	}
	deviceWaitRounds, err := envInt("DEVICE_WAIT_ROUNDS", 20)
	if err != nil {
		return Config{}, err
	}
	deviceWaitSeconds, err := envInt("DEVICE_WAIT_SECONDS", 30)
	if err != nil {
		return Config{}, err
	}
	presignTTL, err := envDuration("MINIO_PRESIGN_TTL", time.Hour)
	if err != nil {
		return Config{}, err
	}
	// 单次按需签发请求的文件数上限(差距 #8)。超限整体拒绝,不截断。
	uploadMaxFiles, err := envInt("UPLOAD_REQUEST_MAX_FILES", 64)
	if err != nil {
		return Config{}, err
	}
	hermesTimeoutSec, err := envInt("HERMES_TIMEOUT_SEC", 60)
	if err != nil {
		return Config{}, err
	}
	// 翻译超时不复用 HERMES_TIMEOUT_SEC:bridge 实测 -t "" 冷/热约 76s/13s,
	// 分析用 60s 起步,交互用需要单独调(设计文档 §6)。
	nlTimeoutSec, err := envInt("FEISHU_CMD_NL_TIMEOUT_SEC", 60)
	if err != nil {
		return Config{}, err
	}
	escMinConf, err := envFloat("ESCALATION_MIN_CONFIDENCE", 0.7)
	if err != nil {
		return Config{}, err
	}

	return Config{
		TemporalAddress:    env("TEMPORAL_ADDRESS", "127.0.0.1:7233"),
		TemporalTaskQueue:  env("TEMPORAL_TASK_QUEUE", "device-test"), // 须与 trigger 缺省一致
		DatabaseURL:        getenv("DATABASE_URL"),
		CallbacksAddr:      env("WORKER_CALLBACKS_ADDR", ":8091"),
		VariantsConfigPath: variantsPath,
		Activity: activity.Config{
			LeaseSeconds:         leaseSeconds,
			QuarantineAfter:      quarantineAfter,
			CallbackBaseURL:      callbackBaseURL,
			ArtifactAuthType:     env("ARTIFACT_AUTH_TYPE", "job_token"),
			ArtifactAuthToken:    getenv("ARTIFACT_AUTH_TOKEN"),
			ArtifactAuthUsername: getenv("ARTIFACT_AUTH_USERNAME"), // 仅 basic(Deploy Token)使用
			FeishuWebhookURL:     getenv("FEISHU_WEBHOOK_URL"),
			// 飞书双模:三件套齐全走企业自建应用,否则回退 webhook(见 feishu.NewSender)
			FeishuAppID:         getenv("FEISHU_APP_ID"),
			FeishuAppSecret:     getenv("FEISHU_APP_SECRET"),
			FeishuReceiveID:     getenv("FEISHU_RECEIVE_ID"),      // open_id 单聊 / chat_id 群
			FeishuReceiveIDType: getenv("FEISHU_RECEIVE_ID_TYPE"), // 空 → chat_id
			// 飞书指令 listener 白名单(逗号分隔 open_id;空 = listener 不启动)
			FeishuCmdWhitelist: getenv("FEISHU_CMD_WHITELIST"),
			// DevOps → PM 升级通道(设计 §8):空 = 升级禁用(现状)
			EscalationEndpoint:      getenv("ESCALATION_ENDPOINT"),
			EscalationToken:         getenv("ESCALATION_TOKEN"),
			EscalationMinConfidence: escMinConf,
			// §12 Phase 2:自然语言翻译旁路总开关(缺省关,灰度)。
			FeishuCmdNL:        getenv("FEISHU_CMD_NL") == "true",
			FeishuCmdNLTimeout: time.Duration(nlTimeoutSec) * time.Second,
			MinAgentVersion:    getenv("MIN_AGENT_VERSION"),
			// Phase 3 mTLS(§12):三件套齐全时启用 TLS server/client auth。
			MTLSCAFile:   getenv("MTLS_CA_FILE"),
			MTLSCertFile: getenv("MTLS_CERT_FILE"),
			MTLSKeyFile:  getenv("MTLS_KEY_FILE"),
			// §3.7:MINIO_ENDPOINT 或凭据为空即禁用预签名(优雅降级)。
			MinIOEndpoint:       getenv("MINIO_ENDPOINT"),
			MinIOPublicEndpoint: getenv("MINIO_PUBLIC_ENDPOINT"),
			MinIOAccessKey:      getenv("MINIO_ACCESS_KEY"),
			MinIOSecretKey:      getenv("MINIO_SECRET_KEY"),
			MinIOBucket:         env("MINIO_BUCKET", "hermes-evidence"),
			MinIOPresignTTL:     presignTTL,
			UploadMaxFiles:      uploadMaxFiles,
			// §12 Phase 2:HERMES_ENDPOINT 空 → Analyzer 禁用,规则引擎保底。
			HermesEndpoint:  getenv("HERMES_ENDPOINT"),
			HermesAuthToken: getenv("HERMES_AUTH_TOKEN"),
			HermesModel:     getenv("HERMES_MODEL"),
			HermesTimeout:   time.Duration(hermesTimeoutSec) * time.Second,
		},
		SpecDefaults: activity.SpecDefaults{
			MaxInfraRetries:   maxInfraRetries,
			LeaseSeconds:      leaseSeconds,
			HardTimeoutMargin: hardTimeoutMargin,
			DeviceWaitRounds:  deviceWaitRounds,
			DeviceWaitSeconds: deviceWaitSeconds,
		},
	}, nil
}
