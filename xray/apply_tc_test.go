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

// 预迁移场景：s.tiers 非空（后端下发了），但 xray config 里没有对应 inbound tag。
// maybeClearTiersIfUnmigrated 应该清空缓存退回兼容模式。
func TestMaybeClearTiersIfUnmigrated_ClearsWhenInboundMissing(t *testing.T) {
	// xray config 只有老单 inbound "proxy"
	configPath := writeConfigWithInbounds(t, []map[string]any{
		{"tag": "proxy", "port": "33456"},
	})

	s := &XrayUserSync{
		configPath: configPath,
		tiers: map[string]TierConfig{
			"vip":  {MarkID: 1, InboundTag: "proxy-vip", PoolMbps: 100},
			"svip": {MarkID: 2, InboundTag: "proxy-svip", PoolMbps: 500},
		},
		defaultTier: "vip",
	}

	s.maybeClearTiersIfUnmigrated()

	if len(s.tiers) != 0 {
		t.Errorf("tiers should be cleared when config is single-inbound, got %v", s.tiers)
	}
	if s.defaultTier != "" {
		t.Errorf("defaultTier should be cleared, got %q", s.defaultTier)
	}
}

// 已迁移场景：config 里所有 tier inboundTag 都存在。不应清空。
func TestMaybeClearTiersIfUnmigrated_KeepsWhenAllPresent(t *testing.T) {
	configPath := writeConfigWithInbounds(t, []map[string]any{
		{"tag": "proxy-vip", "port": "443"},
		{"tag": "proxy-svip", "port": "8443"},
	})

	s := &XrayUserSync{
		configPath: configPath,
		tiers: map[string]TierConfig{
			"vip":  {MarkID: 1, InboundTag: "proxy-vip", PoolMbps: 100},
			"svip": {MarkID: 2, InboundTag: "proxy-svip", PoolMbps: 500},
		},
		defaultTier: "vip",
	}

	s.maybeClearTiersIfUnmigrated()

	if len(s.tiers) != 2 {
		t.Errorf("tiers should be preserved when config is multi-inbound, got %v", s.tiers)
	}
	if s.defaultTier != "vip" {
		t.Errorf("defaultTier should be preserved, got %q", s.defaultTier)
	}
}

// 部分迁移（一个 tier 的 inbound 存在、另一个缺失）— 保守处理：视为未迁移清空。
func TestMaybeClearTiersIfUnmigrated_PartialMissing(t *testing.T) {
	configPath := writeConfigWithInbounds(t, []map[string]any{
		{"tag": "proxy-vip", "port": "443"},
		// 缺 proxy-svip
	})

	s := &XrayUserSync{
		configPath: configPath,
		tiers: map[string]TierConfig{
			"vip":  {InboundTag: "proxy-vip"},
			"svip": {InboundTag: "proxy-svip"},
		},
		defaultTier: "vip",
	}

	s.maybeClearTiersIfUnmigrated()

	if len(s.tiers) != 0 {
		t.Errorf("partial missing should clear tiers, got %v", s.tiers)
	}
}

// 兼容模式（tiers 本来就是空）不应 panic，也不该报错
func TestMaybeClearTiersIfUnmigrated_NoOpWhenEmpty(t *testing.T) {
	s := &XrayUserSync{
		configPath: "/nonexistent/config.json",
		tiers:      map[string]TierConfig{},
	}
	s.maybeClearTiersIfUnmigrated() // 不 panic 就算过
}
