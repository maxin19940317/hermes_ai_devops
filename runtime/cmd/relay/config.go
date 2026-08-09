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

	// 积压监控(第四批)。BacklogInterval=0 关闭定期报告。
	BacklogInterval time.Duration
	StuckAttempts   int
	BacklogWarnAge  time.Duration

	// 设备隔离通知(spec §9.2/§9.3)。与 cmd/worker 同款飞书双模配置——
	// app 三件套齐全优先,否则 webhook 兜底,都没配 → Notifier=nil。
	// DeviceNotifyDisabled 是显式关闭(RELAY_DEVICE_NOTIFY=off);
	// 未显式关闭且 Notifier 为 nil 时,relay 让 device-quarantined 事件保持
	// pending(不静默 MarkPublished),见 internal/relay/relay.go deliver()。
	FeishuWebhookURL     string
	FeishuAppID          string
	FeishuAppSecret      string
	FeishuReceiveID      string
	FeishuReceiveIDType  string
	DeviceNotifyDisabled bool
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
	// 积压报告间隔。缺省 1 分钟:比投递轮询稀疏得多,足够及时又不刷日志。
	backlogInterval, err := envDuration("RELAY_BACKLOG_INTERVAL", time.Minute)
	if err != nil {
		return Config{}, err
	}
	// attempts >= 此值算"卡住"。3 与 §10 的机械重试上限同量级:
	// 重试三次仍投不出去的行,不会靠再等一轮自愈。
	stuckAttempts, err := envInt("RELAY_STUCK_ATTEMPTS", 3)
	if err != nil {
		return Config{}, err
	}
	// 最老未投递行超过此年龄升级为 warn。5 分钟:正常投递是秒级,
	// 积压到分钟级说明 Temporal 侧或 DB 侧有事。
	backlogWarnAge, err := envDuration("RELAY_BACKLOG_WARN_AGE", 5*time.Minute)
	if err != nil {
		return Config{}, err
	}
	return Config{
		TemporalAddress: env("TEMPORAL_ADDRESS", "127.0.0.1:7233"),
		DatabaseURL:     getenv("DATABASE_URL"),
		BatchSize:       batchSize,
		PollInterval:    pollInterval,
		MaxBackoff:      maxBackoff,
		BacklogInterval: backlogInterval,
		StuckAttempts:   stuckAttempts,
		BacklogWarnAge:  backlogWarnAge,

		FeishuWebhookURL:    getenv("FEISHU_WEBHOOK_URL"),
		FeishuAppID:         getenv("FEISHU_APP_ID"),
		FeishuAppSecret:     getenv("FEISHU_APP_SECRET"),
		FeishuReceiveID:     getenv("FEISHU_RECEIVE_ID"),      // open_id 单聊 / chat_id 群
		FeishuReceiveIDType: getenv("FEISHU_RECEIVE_ID_TYPE"), // 空 → chat_id
		// 显式关闭(spec §9.3 第三行):值为 "off" 才算关闭,其余(含空)都是
		// "未显式关闭",走"未配置则保持 pending"分支——默认必须偏向不丢通知。
		DeviceNotifyDisabled: getenv("RELAY_DEVICE_NOTIFY") == "off",
	}, nil
}
