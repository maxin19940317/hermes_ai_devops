package main

import (
	"fmt"
	"strconv"
	"time"
)

// Config 是 relay 进程装配所需的全部参数,由环境变量派生(见 main.go 头注释)。
type Config struct {
	TemporalAddress string
	DatabaseURL     string // 空 → 内存 store(仅开发)
	BatchSize       int
	PollInterval    time.Duration
	MaxBackoff      time.Duration
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

	batchSize, err := envInt("RELAY_BATCH_SIZE", 100)
	if err != nil {
		return Config{}, err
	}
	pollInterval, err := envDuration("RELAY_POLL_INTERVAL", time.Second)
	if err != nil {
		return Config{}, err
	}
	maxBackoff, err := envDuration("RELAY_MAX_BACKOFF", 30*time.Second)
	if err != nil {
		return Config{}, err
	}
	return Config{
		TemporalAddress: env("TEMPORAL_ADDRESS", "127.0.0.1:7233"),
		DatabaseURL:     getenv("DATABASE_URL"),
		BatchSize:       batchSize,
		PollInterval:    pollInterval,
		MaxBackoff:      maxBackoff,
	}, nil
}
