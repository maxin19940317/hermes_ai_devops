package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func envOf(pairs ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return func(k string) string { return m[k] }
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig("", envOf(
		"AGENT_CLIENT_ID", "c1",
		"AGENT_RUNTIME_CALLBACK_URL", "http://runtime:18091",
		"AGENT_BASE_URL", "http://agent:8480",
		"AGENT_ADB_PATH", "/usr/bin/adb",
	))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ListenAddr != ":8480" || cfg.Version != "dev" ||
		cfg.RunsRoot != "./agent-runs" || cfg.DBPath != "./agent.db" ||
		cfg.HeartbeatInterval != 10*time.Second {
		t.Errorf("defaults = %+v", cfg)
	}
}

func TestLoadConfigMissingRequired(t *testing.T) {
	if _, err := loadConfig("", envOf()); err == nil {
		t.Fatal("缺必填项应报错")
	} else {
		for _, key := range []string{"AGENT_CLIENT_ID", "AGENT_RUNTIME_CALLBACK_URL", "AGENT_BASE_URL", "AGENT_ADB_PATH"} {
			if !strings.Contains(err.Error(), key) {
				t.Errorf("错误信息应列出 %s: %v", key, err)
			}
		}
	}
}

func TestLoadConfigFileAndEnvOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.conf")
	content := "# 注释行\n" +
		"AGENT_CLIENT_ID=from-file\n" +
		"AGENT_RUNTIME_CALLBACK_URL=http://file:18091\n" +
		"AGENT_BASE_URL=http://file:8480\n" +
		"AGENT_ADB_PATH=/opt/adb\n" +
		"AGENT_LISTEN_ADDR=:9999\n" +
		"AGENT_HEARTBEAT_INTERVAL=30s\n" +
		"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// 文件值生效;AGENT_CLIENT_ID 被环境变量覆盖
	cfg, err := loadConfig(path, envOf("AGENT_CLIENT_ID", "from-env"))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ClientID != "from-env" {
		t.Errorf("env 应覆盖配置文件: %q", cfg.ClientID)
	}
	if cfg.RuntimeCallbackURL != "http://file:18091" || cfg.ListenAddr != ":9999" ||
		cfg.HeartbeatInterval != 30*time.Second {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadConfigBadDuration(t *testing.T) {
	_, err := loadConfig("", envOf(
		"AGENT_CLIENT_ID", "c1",
		"AGENT_RUNTIME_CALLBACK_URL", "http://runtime:18091",
		"AGENT_BASE_URL", "http://agent:8480",
		"AGENT_ADB_PATH", "/usr/bin/adb",
		"AGENT_HEARTBEAT_INTERVAL", "soon",
	))
	if err == nil {
		t.Fatal("非法 duration 应报错")
	}
}

func TestParseSOCAliases(t *testing.T) {
	cases := []struct {
		raw     string
		want    map[string]string
		wantErr bool
	}{
		{"", nil, false},
		{"  ", nil, false},
		{"trinket:QCM6125", map[string]string{"trinket": "QCM6125"}, false},
		{"trinket:QCM6125, kalama:SM8550", map[string]string{"trinket": "QCM6125", "kalama": "SM8550"}, false},
		{"trinket", nil, true},
		{"trinket:", nil, true},
		{":QCM6125", nil, true},
	}
	for _, c := range cases {
		got, err := parseSOCAliases(c.raw)
		if (err != nil) != c.wantErr {
			t.Errorf("parseSOCAliases(%q) err = %v, wantErr %v", c.raw, err, c.wantErr)
			continue
		}
		if c.wantErr {
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("parseSOCAliases(%q) = %v, want %v", c.raw, got, c.want)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("parseSOCAliases(%q)[%q] = %q, want %q", c.raw, k, got[k], v)
			}
		}
	}
}
