package main

import (
	"os"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/salt-lake/kd-vps-agent/collect"
)

type Config struct {
	NATSUrl        string
	NATSToken      string
	Host           string
	NodeID         string
	APIBase        string
	ScriptToken    string
	Protocol       string
	SwanContainer  string
	SwanImage      string
	XrayAPIAddr    string
	XrayInboundTag string
	XrayConfigPath string
	HTTPAPIAddr    string
	Iface          string
	ReportInterval time.Duration
}

func LoadConfig() Config {
	token := os.Getenv("NATS_AUTH_TOKEN")
	return Config{
		NATSUrl:        envOr("NATS_URL", nats.DefaultURL),
		NATSToken:      token,
		Host:           os.Getenv("NODE_HOST"),
		NodeID:         os.Getenv("NODE_ID"),
		APIBase:        strings.TrimRight(os.Getenv("API_BASE"), "/"),
		ScriptToken:    envOr("SCRIPT_TOKEN", token),
		Protocol:       envOr("NODE_PROTOCOL", "ikev2"),
		SwanContainer:  envOr("SWAN_CONTAINER", "strongswan"),
		SwanImage:      envOr("SWAN_IMAGE", "mooc1988/swan:latest"),
		XrayAPIAddr:    envOr("XRAY_API_ADDR", "127.0.0.1:10085"),
		XrayInboundTag: envOr("XRAY_INBOUND_TAG", "proxy"),
		XrayConfigPath: envOr("XRAY_CONFIG_PATH", "/etc/xray/config.json"),
		HTTPAPIAddr:    envOr("HTTP_API_ADDR", ":8080"),
		Iface:          collect.DetectPrimaryIface(),
		ReportInterval: parseDuration(envOr("REPORT_INTERVAL", "2m")),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 2 * time.Minute
	}
	return d
}
