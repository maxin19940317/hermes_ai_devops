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
	hermesTimeoutSec, err := envInt("HERMES_TIMEOUT_SEC", 60)
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
			LeaseSeconds:      leaseSeconds,
			QuarantineAfter:   quarantineAfter,
			CallbackBaseURL:   callbackBaseURL,
			ArtifactAuthType:  env("ARTIFACT_AUTH_TYPE", "job_token"),
			ArtifactAuthToken: getenv("ARTIFACT_AUTH_TOKEN"),
			FeishuWebhookURL:  getenv("FEISHU_WEBHOOK_URL"),
			// §3.7:MINIO_ENDPOINT 或凭据为空即禁用预签名(优雅降级)。
			MinIOEndpoint:       getenv("MINIO_ENDPOINT"),
			MinIOPublicEndpoint: getenv("MINIO_PUBLIC_ENDPOINT"),
			MinIOAccessKey:      getenv("MINIO_ACCESS_KEY"),
			MinIOSecretKey:      getenv("MINIO_SECRET_KEY"),
			MinIOBucket:         env("MINIO_BUCKET", "hermes-evidence"),
			MinIOPresignTTL:     presignTTL,
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
