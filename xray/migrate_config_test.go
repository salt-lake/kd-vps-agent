//go:build xray

package xray

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const origSingleInboundConfig = `{
  "log": {"loglevel":"info"},
  "inbounds": [{
    "tag": "proxy",
    "listen": "0.0.0.0",
    "port": "34521-34524",
    "protocol": "vless",
    "settings": {"clients": [{"id":"a1b2c3d4-0000-0000-0000-000000000001","email":"default@test","flow":"xtls-rprx-vision"}], "decryption":"none"},
    "streamSettings": {"network":"tcp","security":"reality","realitySettings":{"dest":"www.microsoft.com:443","serverNames":["www.microsoft.com"],"privateKey":"XXX","shortIds":["01234567"]}}
  }],
  "outbounds": [{"tag":"direct","protocol":"freedom"},{"tag":"blocked","protocol":"blackhole"}],
  "routing": {"rules": []}
}`

func TestWriteMultiInboundConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(origSingleInboundConfig), 0644); err != nil {
		t.Fatal(err)
	}

	s := &XrayUserSync{configPath: configPath}
	p := migrateTierPayload{
		Tiers: map[string]migrateTierEntry{
			"vip":  {MarkID: 1, InboundTag: "proxy-vip", PortRange: "34521-34524", PoolMbps: 100},
			"svip": {MarkID: 2, InboundTag: "proxy-svip", PortRange: "45000-45003", PoolMbps: 500},
		},
		DefaultTier: "vip",
	}
	users := []userDTO{
		{UUID: "user-vip-1", Tier: "vip"},
		{UUID: "user-svip-1", Tier: "svip"},
	}

	if err := s.writeMultiInboundConfig([]byte(origSingleInboundConfig), p, users); err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(configPath)
	var parsed map[string]interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}

	inbounds := parsed["inbounds"].([]interface{})
	if len(inbounds) != 2 {
		t.Fatalf("expected 2 inbounds, got %d", len(inbounds))
	}

	tags := map[string]bool{}
	portsByTag := map[string]string{}
	for _, ib := range inbounds {
		m := ib.(map[string]interface{})
		tag := m["tag"].(string)
		tags[tag] = true
		portsByTag[tag] = m["port"].(string)
	}
	if !tags["proxy-vip"] || !tags["proxy-svip"] {
		t.Errorf("missing tier inbounds, got %v", tags)
	}
	if portsByTag["proxy-vip"] != "34521-34524" {
		t.Errorf("proxy-vip port = %s, want 34521-34524", portsByTag["proxy-vip"])
	}
	if portsByTag["proxy-svip"] != "45000-45003" {
		t.Errorf("proxy-svip port = %s, want 45000-45003", portsByTag["proxy-svip"])
	}

	// outbounds 保持原 config 不变（不追加 direct-<tier>）
	// 限速通过 iptables 按源端口匹配，xray 侧不需要感知 tier
	outbounds := parsed["outbounds"].([]interface{})
	for _, ob := range outbounds {
		m := ob.(map[string]interface{})
		tag, _ := m["tag"].(string)
		if tag == "direct-vip" || tag == "direct-svip" {
			t.Errorf("outbound should not have per-tier direct-<tier>; got tag=%s", tag)
		}
		// 不应含 sockopt.mark
		if ss, ok := m["streamSettings"].(map[string]interface{}); ok {
			if sockopt, ok := ss["sockopt"].(map[string]interface{}); ok {
				if _, has := sockopt["mark"]; has {
					t.Errorf("outbound tag=%s should not have sockopt.mark (limit via iptables)", tag)
				}
			}
		}
	}

	// routing.rules 保持原样（空）—— 不做 tier 级路由
	routing := parsed["routing"].(map[string]interface{})
	rules := routing["rules"].([]interface{})
	if len(rules) != 0 {
		t.Errorf("routing.rules should be empty; got %d rules: %v", len(rules), rules)
	}

	// clients 分布：proxy-vip 应含 user-vip-1 + defaultUUID；proxy-svip 应含 user-svip-1 + defaultUUID
	allContent := string(raw)
	if !strings.Contains(allContent, "user-vip-1") || !strings.Contains(allContent, "user-svip-1") {
		t.Errorf("users not in config")
	}
}

func TestWriteMultiInboundConfig_MissingTierUsesDefault(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(origSingleInboundConfig), 0644); err != nil {
		t.Fatal(err)
	}

	s := &XrayUserSync{configPath: configPath}
	p := migrateTierPayload{
		Tiers: map[string]migrateTierEntry{
			"vip": {MarkID: 1, InboundTag: "proxy-vip", PortRange: "34521-34524", PoolMbps: 100},
		},
		DefaultTier: "vip",
	}
	// 用户 tier 为空（老数据）应归入 defaultTier=vip
	users := []userDTO{{UUID: "legacy-user", Tier: ""}}

	if err := s.writeMultiInboundConfig([]byte(origSingleInboundConfig), p, users); err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(configPath)
	if !strings.Contains(string(raw), "legacy-user") {
		t.Error("legacy user should have been placed in default tier inbound")
	}
}
