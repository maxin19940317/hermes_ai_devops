package main

import "testing"

func lookup(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadConfigAppliesDefaults(t *testing.T) {
	cfg, err := loadConfig(lookup(map[string]string{
		"VARIANTS_CONFIG":   "../../ci/variants.yaml",
		"CALLBACK_BASE_URL": "https://runtime.example:8091",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	// §10 缺省值:任务租约 120s;设备隔离阈值连续 3 次;任务级机械重试 max 2(仅 INFRA)
	if cfg.TemporalAddress != "127.0.0.1:7233" {
		t.Errorf("TemporalAddress = %q", cfg.TemporalAddress)
	}
	if cfg.TemporalTaskQueue != "device-test" {
		t.Errorf("TemporalTaskQueue = %q, 必须与 trigger 缺省一致", cfg.TemporalTaskQueue)
	}
	if cfg.CallbacksAddr != ":8091" {
		t.Errorf("CallbacksAddr = %q", cfg.CallbacksAddr)
	}
	if cfg.DatabaseURL != "" {
		t.Errorf("DatabaseURL = %q, want empty(开发模式内存 store)", cfg.DatabaseURL)
	}
	if cfg.Activity.LeaseSeconds != 120 {
		t.Errorf("LeaseSeconds = %d, want 120", cfg.Activity.LeaseSeconds)
	}
	if cfg.Activity.QuarantineAfter != 3 {
		t.Errorf("QuarantineAfter = %d, want 3", cfg.Activity.QuarantineAfter)
	}
	if cfg.Activity.CallbackBaseURL != "https://runtime.example:8091" {
		t.Errorf("CallbackBaseURL = %q", cfg.Activity.CallbackBaseURL)
	}
	if cfg.Activity.ArtifactAuthType != "job_token" {
		t.Errorf("ArtifactAuthType = %q, want job_token", cfg.Activity.ArtifactAuthType)
	}
	if cfg.Activity.FeishuWebhookURL != "" {
		t.Errorf("FeishuWebhookURL = %q, want empty(未配置飞书时静默,§notify.go)", cfg.Activity.FeishuWebhookURL)
	}
	if cfg.SpecDefaults.MaxInfraRetries != 2 || cfg.SpecDefaults.LeaseSeconds != 120 ||
		cfg.SpecDefaults.HardTimeoutMargin != 1200 || cfg.SpecDefaults.DeviceWaitRounds != 20 ||
		cfg.SpecDefaults.DeviceWaitSeconds != 30 {
		t.Errorf("SpecDefaults = %+v", cfg.SpecDefaults)
	}
	if cfg.VariantsConfigPath != "../../ci/variants.yaml" {
		t.Errorf("VariantsConfigPath = %q", cfg.VariantsConfigPath)
	}
}

func TestLoadConfigOverridesFromEnv(t *testing.T) {
	cfg, err := loadConfig(lookup(map[string]string{
		"VARIANTS_CONFIG":       "v.yaml",
		"CALLBACK_BASE_URL":     "https://runtime.example",
		"TEMPORAL_ADDRESS":      "temporal.internal:7233",
		"TEMPORAL_TASK_QUEUE":   "custom-queue",
		"DATABASE_URL":          "postgres://x/y",
		"WORKER_CALLBACKS_ADDR": ":9999",
		"LEASE_SECONDS":         "60",
		"QUARANTINE_AFTER":      "5",
		"MAX_INFRA_RETRIES":     "1",
		"ARTIFACT_AUTH_TYPE":    "bearer",
		"ARTIFACT_AUTH_TOKEN":   "secret-tok",
		"FEISHU_WEBHOOK_URL":    "https://open.feishu.cn/hook/x",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.TemporalAddress != "temporal.internal:7233" {
		t.Errorf("TemporalAddress = %q", cfg.TemporalAddress)
	}
	if cfg.TemporalTaskQueue != "custom-queue" {
		t.Errorf("TemporalTaskQueue = %q", cfg.TemporalTaskQueue)
	}
	if cfg.DatabaseURL != "postgres://x/y" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.CallbacksAddr != ":9999" {
		t.Errorf("CallbacksAddr = %q", cfg.CallbacksAddr)
	}
	if cfg.Activity.LeaseSeconds != 60 || cfg.SpecDefaults.LeaseSeconds != 60 {
		t.Errorf("LeaseSeconds override 未生效: activity=%d spec=%d",
			cfg.Activity.LeaseSeconds, cfg.SpecDefaults.LeaseSeconds)
	}
	if cfg.Activity.QuarantineAfter != 5 {
		t.Errorf("QuarantineAfter = %d", cfg.Activity.QuarantineAfter)
	}
	if cfg.SpecDefaults.MaxInfraRetries != 1 {
		t.Errorf("MaxInfraRetries = %d", cfg.SpecDefaults.MaxInfraRetries)
	}
	if cfg.Activity.ArtifactAuthType != "bearer" || cfg.Activity.ArtifactAuthToken != "secret-tok" {
		t.Errorf("artifact auth = %q/%q", cfg.Activity.ArtifactAuthType, cfg.Activity.ArtifactAuthToken)
	}
	if cfg.Activity.FeishuWebhookURL != "https://open.feishu.cn/hook/x" {
		t.Errorf("FeishuWebhookURL = %q", cfg.Activity.FeishuWebhookURL)
	}
}

func TestLoadConfigRequiresVariantsConfig(t *testing.T) {
	_, err := loadConfig(lookup(map[string]string{
		"CALLBACK_BASE_URL": "https://runtime.example",
	}))
	if err == nil {
		t.Fatal("VARIANTS_CONFIG 缺失应报错(fail fast)")
	}
}

func TestLoadConfigRequiresCallbackBaseURL(t *testing.T) {
	_, err := loadConfig(lookup(map[string]string{
		"VARIANTS_CONFIG": "v.yaml",
	}))
	if err == nil {
		t.Fatal("CALLBACK_BASE_URL 缺失应报错(fail fast;派单载荷需要,§8.1)")
	}
}

func TestLoadConfigRejectsInvalidInt(t *testing.T) {
	_, err := loadConfig(lookup(map[string]string{
		"VARIANTS_CONFIG":   "v.yaml",
		"CALLBACK_BASE_URL": "https://runtime.example",
		"LEASE_SECONDS":     "not-a-number",
	}))
	if err == nil {
		t.Fatal("非法整数应报错")
	}
}
