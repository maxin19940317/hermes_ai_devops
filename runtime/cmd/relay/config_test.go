package main

import (
	"testing"
	"time"
)

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TemporalAddress != "127.0.0.1:7233" || cfg.BatchSize != 100 ||
		cfg.PollInterval != time.Second || cfg.MaxBackoff != 30*time.Second ||
		cfg.DatabaseURL != "" {
		t.Errorf("defaults = %+v", cfg)
	}
	// 未设置任何 FEISHU_*/RELAY_DEVICE_NOTIFY 时:全部飞书字段留空,
	// DeviceNotifyDisabled 缺省 false——"未配置"不等于"已关闭"(spec §9.3)。
	if cfg.FeishuWebhookURL != "" || cfg.FeishuAppID != "" || cfg.FeishuAppSecret != "" ||
		cfg.FeishuReceiveID != "" || cfg.FeishuReceiveIDType != "" || cfg.DeviceNotifyDisabled {
		t.Errorf("feishu defaults = %+v", cfg)
	}
}

func TestLoadConfigFeishuAndDeviceNotify(t *testing.T) {
	env := map[string]string{
		"FEISHU_APP_ID":          "app1",
		"FEISHU_APP_SECRET":      "secret1",
		"FEISHU_RECEIVE_ID":      "oc_xxx",
		"FEISHU_RECEIVE_ID_TYPE": "chat_id",
		"FEISHU_WEBHOOK_URL":     "https://example.invalid/hook",
		"RELAY_DEVICE_NOTIFY":    "off",
	}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FeishuAppID != "app1" || cfg.FeishuAppSecret != "secret1" ||
		cfg.FeishuReceiveID != "oc_xxx" || cfg.FeishuReceiveIDType != "chat_id" ||
		cfg.FeishuWebhookURL != "https://example.invalid/hook" {
		t.Errorf("feishu overrides = %+v", cfg)
	}
	if !cfg.DeviceNotifyDisabled {
		t.Error("RELAY_DEVICE_NOTIFY=off 应解析为 DeviceNotifyDisabled=true")
	}

	// 非 "off" 的任意值(含拼写错误)都不算显式关闭——宁可保守地保持 pending
	// 也不能因为一个笔误就把隔离通知悄悄关掉。
	envTypo := map[string]string{"RELAY_DEVICE_NOTIFY": "false"}
	cfg2, err := loadConfig(func(k string) string { return envTypo[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.DeviceNotifyDisabled {
		t.Error(`RELAY_DEVICE_NOTIFY="false" 不应等价于 "off"`)
	}
}

func TestLoadConfigOverridesAndBadValues(t *testing.T) {
	env := map[string]string{
		"TEMPORAL_ADDRESS":    "temporal:7233",
		"DATABASE_URL":        "postgres://x",
		"RELAY_BATCH_SIZE":    "50",
		"RELAY_POLL_INTERVAL": "200ms",
		"RELAY_MAX_BACKOFF":   "1m",
	}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TemporalAddress != "temporal:7233" || cfg.DatabaseURL != "postgres://x" ||
		cfg.BatchSize != 50 || cfg.PollInterval != 200*time.Millisecond || cfg.MaxBackoff != time.Minute {
		t.Errorf("overrides = %+v", cfg)
	}

	bad := map[string]string{"RELAY_BATCH_SIZE": "abc"}
	if _, err := loadConfig(func(k string) string { return bad[k] }); err == nil {
		t.Error("非法整数应报错(fail fast,不吞错误静默启动)")
	}
	badDur := map[string]string{"RELAY_POLL_INTERVAL": "abc"}
	if _, err := loadConfig(func(k string) string { return badDur[k] }); err == nil {
		t.Error("非法时长应报错")
	}
}
