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
