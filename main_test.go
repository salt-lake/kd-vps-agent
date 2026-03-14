package main

import (
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
	}{
		{"2m", 2 * time.Minute},
		{"30s", 30 * time.Second},
		{"1h", time.Hour},
		{"", 2 * time.Minute},       // 空字符串 → 默认
		{"invalid", 2 * time.Minute}, // 非法 → 默认
		{"0s", 2 * time.Minute},      // 零值 → 默认
		{"-1m", 2 * time.Minute},     // 负值 → 默认
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := parseDuration(tc.input)
			if got != tc.want {
				t.Errorf("parseDuration(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestEnvOr(t *testing.T) {
	t.Run("key not set", func(t *testing.T) {
		t.Setenv("TEST_ENVOR_KEY", "")
		got := envOr("TEST_ENVOR_KEY", "default_val")
		if got != "default_val" {
			t.Errorf("got %q, want %q", got, "default_val")
		}
	})
	t.Run("key set", func(t *testing.T) {
		t.Setenv("TEST_ENVOR_KEY", "custom")
		got := envOr("TEST_ENVOR_KEY", "default_val")
		if got != "custom" {
			t.Errorf("got %q, want %q", got, "custom")
		}
	})
}

func TestLoadConfig_Defaults(t *testing.T) {
	// 清理所有相关环境变量，确保测试默认值
	for _, k := range []string{
		"NATS_URL", "NATS_AUTH_TOKEN", "NODE_HOST", "API_BASE",
		"SCRIPT_TOKEN", "NODE_PROTOCOL", "SWAN_CONTAINER",
		"XRAY_CONTAINER", "XRAY_API_ADDR", "XRAY_INBOUND_TAG",
		"XRAY_CONFIG_PATH", "NET_IFACE", "REPORT_INTERVAL",
	} {
		t.Setenv(k, "")
	}

	cfg := LoadConfig()

	if cfg.Protocol != "ikev2" {
		t.Errorf("Protocol default = %q, want %q", cfg.Protocol, "ikev2")
	}
	if cfg.SwanContainer != "strongswan" {
		t.Errorf("SwanContainer default = %q, want %q", cfg.SwanContainer, "strongswan")
	}
	if cfg.XrayContainer != "xray" {
		t.Errorf("XrayContainer default = %q, want %q", cfg.XrayContainer, "xray")
	}
	if cfg.XrayAPIAddr != "127.0.0.1:10085" {
		t.Errorf("XrayAPIAddr default = %q, want %q", cfg.XrayAPIAddr, "127.0.0.1:10085")
	}
	if cfg.XrayInboundTag != "proxy" {
		t.Errorf("XrayInboundTag default = %q, want %q", cfg.XrayInboundTag, "proxy")
	}
	if cfg.XrayConfigPath != "/etc/xray/config.json" {
		t.Errorf("XrayConfigPath default = %q, want %q", cfg.XrayConfigPath, "/etc/xray/config.json")
	}
	if cfg.ReportInterval != 2*time.Minute {
		t.Errorf("ReportInterval default = %v, want %v", cfg.ReportInterval, 2*time.Minute)
	}
}

func TestLoadConfig_EnvOverride(t *testing.T) {
	t.Setenv("NODE_HOST", "1.2.3.4")
	t.Setenv("NODE_PROTOCOL", "xray")
	t.Setenv("NATS_AUTH_TOKEN", "secret")
	t.Setenv("API_BASE", "https://api.example.com/")
	t.Setenv("REPORT_INTERVAL", "5m")
	t.Setenv("SWAN_CONTAINER", "my-swan")

	cfg := LoadConfig()

	if cfg.Host != "1.2.3.4" {
		t.Errorf("Host = %q, want %q", cfg.Host, "1.2.3.4")
	}
	if cfg.Protocol != "xray" {
		t.Errorf("Protocol = %q, want %q", cfg.Protocol, "xray")
	}
	if cfg.NATSToken != "secret" {
		t.Errorf("NATSToken = %q, want %q", cfg.NATSToken, "secret")
	}
	// API_BASE 末尾 / 应被 TrimRight 去掉
	if cfg.APIBase != "https://api.example.com" {
		t.Errorf("APIBase = %q, want %q", cfg.APIBase, "https://api.example.com")
	}
	if cfg.ReportInterval != 5*time.Minute {
		t.Errorf("ReportInterval = %v, want %v", cfg.ReportInterval, 5*time.Minute)
	}
	if cfg.SwanContainer != "my-swan" {
		t.Errorf("SwanContainer = %q, want %q", cfg.SwanContainer, "my-swan")
	}
}

func TestLoadConfig_ScriptTokenFallback(t *testing.T) {
	t.Setenv("NATS_AUTH_TOKEN", "nats-token")
	t.Setenv("SCRIPT_TOKEN", "")

	cfg := LoadConfig()
	// SCRIPT_TOKEN 未设置时，应 fallback 到 NATS_AUTH_TOKEN
	if cfg.ScriptToken != "nats-token" {
		t.Errorf("ScriptToken = %q, want %q", cfg.ScriptToken, "nats-token")
	}
}

func TestBuildProviders_Lengths(t *testing.T) {
	tests := []struct {
		protocol string
		wantLen  int
	}{
		{"ikev2", 3}, // sys + traffic + swan
		{"xray", 3},  // sys + traffic + xray
	}
	for _, tc := range tests {
		t.Run(tc.protocol, func(t *testing.T) {
			cfg := Config{Protocol: tc.protocol}
			got := buildProviders(cfg)
			if len(got) != tc.wantLen {
				t.Errorf("buildProviders(%q) len=%d, want %d", tc.protocol, len(got), tc.wantLen)
			}
		})
	}
}
