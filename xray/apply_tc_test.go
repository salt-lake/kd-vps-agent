//go:build xray

package xray

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// helper: 写一个含给定 inbounds 的最小 xray config 到临时文件
func writeConfigWithInbounds(t *testing.T, inbounds []map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	data, err := json.Marshal(map[string]any{"inbounds": inbounds})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		t.Fatal(err)
	}
	return configPath
}

func TestReadTierPortsFromConfig_Multi(t *testing.T) {
	configPath := writeConfigWithInbounds(t, []map[string]any{
		{"tag": "proxy-vip", "port": "34521-34524"},
		{"tag": "proxy-svip", "port": "45000-45003"},
	})
	tiers := map[string]TierConfig{
		"vip":  {InboundTag: "proxy-vip", MarkID: 1, PoolMbps: 100},
		"svip": {InboundTag: "proxy-svip", MarkID: 2, PoolMbps: 500},
	}
	ports, err := readTierPortsFromConfig(configPath, tiers)
	if err != nil {
		t.Fatal(err)
	}
	if ports["vip"] != "34521-34524" {
		t.Errorf("vip port = %q, want 34521-34524", ports["vip"])
	}
	if ports["svip"] != "45000-45003" {
		t.Errorf("svip port = %q, want 45000-45003", ports["svip"])
	}
}

func TestReadTierPortsFromConfig_IntPort(t *testing.T) {
	// xray 也允许 port 是整数而非字符串
	configPath := writeConfigWithInbounds(t, []map[string]any{
		{"tag": "proxy-vip", "port": 443},
	})
	tiers := map[string]TierConfig{
		"vip": {InboundTag: "proxy-vip", MarkID: 1, PoolMbps: 100},
	}
	ports, err := readTierPortsFromConfig(configPath, tiers)
	if err != nil {
		t.Fatal(err)
	}
	if ports["vip"] != "443" {
		t.Errorf("vip port = %q, want 443 (from integer)", ports["vip"])
	}
}

func TestReadTierPortsFromConfig_MissingTier(t *testing.T) {
	// config 里只有 proxy-vip，但期望 tiers 含 svip → svip 在结果里缺席
	configPath := writeConfigWithInbounds(t, []map[string]any{
		{"tag": "proxy-vip", "port": "443"},
	})
	tiers := map[string]TierConfig{
		"vip":  {InboundTag: "proxy-vip", MarkID: 1, PoolMbps: 100},
		"svip": {InboundTag: "proxy-svip", MarkID: 2, PoolMbps: 500},
	}
	ports, err := readTierPortsFromConfig(configPath, tiers)
	if err != nil {
		t.Fatal(err)
	}
	if ports["vip"] != "443" {
		t.Errorf("vip should be present")
	}
	if _, ok := ports["svip"]; ok {
		t.Errorf("svip should NOT be present when config lacks its inbound, got %q", ports["svip"])
	}
}

func TestReadTierPortsFromConfig_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	_ = os.WriteFile(configPath, []byte("not json"), 0644)
	_, err := readTierPortsFromConfig(configPath, map[string]TierConfig{"vip": {InboundTag: "proxy-vip"}})
	if err == nil {
		t.Error("expected parse error on malformed config")
	}
}

func TestReadTierPortsFromConfig_MissingFile(t *testing.T) {
	_, err := readTierPortsFromConfig("/nonexistent/config.json", map[string]TierConfig{"vip": {InboundTag: "proxy-vip"}})
	if err == nil {
		t.Error("expected read error on missing file")
	}
}
