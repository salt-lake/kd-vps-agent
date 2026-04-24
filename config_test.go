package main

import (
	"testing"
	"time"
)

// ---- envOr ----

func TestEnvOr_EnvSet(t *testing.T) {
	t.Setenv("TEST_ENVOR_KEY", "custom")
	if got := envOr("TEST_ENVOR_KEY", "default"); got != "custom" {
		t.Errorf("got %q, want %q", got, "custom")
	}
}

func TestEnvOr_EnvEmpty(t *testing.T) {
	t.Setenv("TEST_ENVOR_KEY", "")
	if got := envOr("TEST_ENVOR_KEY", "default"); got != "default" {
		t.Errorf("got %q, want %q", got, "default")
	}
}

func TestEnvOr_EnvAbsent(t *testing.T) {
	t.Setenv("TEST_ENVOR_KEY", "")
	if got := envOr("TEST_ENVOR_ABSENT", "fallback"); got != "fallback" {
		t.Errorf("got %q, want %q", got, "fallback")
	}
}

// ---- parseDuration ----

func TestParseDuration_Valid(t *testing.T) {
	cases := []struct {
		input string
		want  time.Duration
	}{
		{"1m", time.Minute},
		{"30s", 30 * time.Second},
		{"2m30s", 2*time.Minute + 30*time.Second},
		{"1h", time.Hour},
	}
	for _, c := range cases {
		got := parseDuration(c.input)
		if got != c.want {
			t.Errorf("parseDuration(%q) = %v, want %v", c.input, got, c.want)
		}
	}
}

func TestParseDuration_Invalid_FallsBack(t *testing.T) {
	cases := []string{"", "not-a-duration", "abc", "-1m", "0"}
	for _, s := range cases {
		got := parseDuration(s)
		if got != 2*time.Minute {
			t.Errorf("parseDuration(%q) = %v, want 2m", s, got)
		}
	}
}

// ---- LoadConfig ----

func TestLoadConfig_Defaults(t *testing.T) {
	// 清除所有相关环境变量，让 LoadConfig 全部走默认值
	for _, k := range []string{
		"NATS_URL", "NATS_AUTH_TOKEN", "NODE_HOST", "NODE_ID",
		"API_BASE", "SCRIPT_TOKEN", "NODE_PROTOCOL",
		"SWAN_CONTAINER", "SWAN_IMAGE",
		"XRAY_API_ADDR", "XRAY_INBOUND_TAG", "XRAY_CONFIG_PATH",
		"REPORT_INTERVAL",
	} {
		t.Setenv(k, "")
	}

	cfg := LoadConfig()

	if cfg.Protocol != "ikev2" {
		t.Errorf("Protocol = %q, want %q", cfg.Protocol, "ikev2")
	}
	if cfg.SwanContainer != "strongswan" {
		t.Errorf("SwanContainer = %q, want %q", cfg.SwanContainer, "strongswan")
	}
	if cfg.SwanImage != "mooc1988/swan:latest" {
		t.Errorf("SwanImage = %q, want %q", cfg.SwanImage, "mooc1988/swan:latest")
	}
	if cfg.XrayAPIAddr != "127.0.0.1:10085" {
		t.Errorf("XrayAPIAddr = %q, want %q", cfg.XrayAPIAddr, "127.0.0.1:10085")
	}
	if cfg.XrayInboundTag != "proxy" {
		t.Errorf("XrayInboundTag = %q, want %q", cfg.XrayInboundTag, "proxy")
	}
	if cfg.ReportInterval != 2*time.Minute {
		t.Errorf("ReportInterval = %v, want 2m", cfg.ReportInterval)
	}
}

func TestLoadConfig_EnvOverrides(t *testing.T) {
	t.Setenv("NODE_HOST", "1.2.3.4")
	t.Setenv("NODE_PROTOCOL", "xray")
	t.Setenv("NATS_AUTH_TOKEN", "secret")
	t.Setenv("SCRIPT_TOKEN", "")       // 空时应 fallback 到 NATS_AUTH_TOKEN
	t.Setenv("API_BASE", "https://api.example.com/") // 尾部斜杠应被去除
	t.Setenv("REPORT_INTERVAL", "5m")

	cfg := LoadConfig()

	if cfg.Host != "1.2.3.4" {
		t.Errorf("Host = %q, want %q", cfg.Host, "1.2.3.4")
	}
	if cfg.Protocol != "xray" {
		t.Errorf("Protocol = %q, want %q", cfg.Protocol, "xray")
	}
	if cfg.ScriptToken != "secret" {
		t.Errorf("ScriptToken = %q, want %q (should fall back to NATS_AUTH_TOKEN)", cfg.ScriptToken, "secret")
	}
	if cfg.APIBase != "https://api.example.com" {
		t.Errorf("APIBase = %q, want trailing slash stripped", cfg.APIBase)
	}
	if cfg.ReportInterval != 5*time.Minute {
		t.Errorf("ReportInterval = %v, want 5m", cfg.ReportInterval)
	}
}
