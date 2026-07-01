//go:build xray

package command

import (
	"encoding/json"
	"os"
	"testing"
)

const sampleXrayConfig = `{
  "log": {"loglevel": "info"},
  "inbounds": [{
    "tag": "proxy",
    "port": "25845-25848",
    "protocol": "vless",
    "settings": {"clients": [{"id": "u1", "flow": "xtls-rprx-vision"}], "decryption": "none"},
    "streamSettings": {
      "network": "tcp",
      "security": "reality",
      "realitySettings": {
        "show": false,
        "dest": "www.microsoft.com:443",
        "serverNames": ["www.microsoft.com"],
        "privateKey": "PRIV",
        "shortIds": ["c024", "e8ac49f0"]
      }
    }
  }],
  "outbounds": [{"protocol": "freedom"}]
}`

func TestSetXrayDest_ReplacesDestAndServerNames(t *testing.T) {
	p := writeTempConfig(t, sampleXrayConfig)
	if err := setXrayDest(p, "www.cloudflare.com:443", "www.cloudflare.com"); err != nil {
		t.Fatalf("setXrayDest: %v", err)
	}
	data, _ := os.ReadFile(p)
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse: %v", err)
	}
	rs, err := realitySettings(cfg)
	if err != nil {
		t.Fatalf("realitySettings: %v", err)
	}
	if rs["dest"] != "www.cloudflare.com:443" {
		t.Errorf("dest = %v, want www.cloudflare.com:443", rs["dest"])
	}
	if sn := firstServerName(rs); sn != "www.cloudflare.com" {
		t.Errorf("serverName = %q, want www.cloudflare.com", sn)
	}
}

func TestSetXrayDest_PreservesKeysAndShortIDs(t *testing.T) {
	p := writeTempConfig(t, sampleXrayConfig)
	if err := setXrayDest(p, "www.apple.com:443", "www.apple.com"); err != nil {
		t.Fatalf("setXrayDest: %v", err)
	}
	data, _ := os.ReadFile(p)
	var cfg map[string]any
	_ = json.Unmarshal(data, &cfg)
	rs, _ := realitySettings(cfg)
	if rs["privateKey"] != "PRIV" {
		t.Errorf("privateKey changed: %v", rs["privateKey"])
	}
	if sid := firstShortID(rs); sid != "c024" {
		t.Errorf("shortId[0] = %q, want c024", sid)
	}
	if port := firstInboundPort(cfg); port != "25845" {
		t.Errorf("first port = %q, want 25845", port)
	}
}

func TestSetXrayDest_ErrorsOnMalformedConfig(t *testing.T) {
	p := writeTempConfig(t, `{"inbounds":[]}`)
	if err := setXrayDest(p, "x:443", "x"); err == nil {
		t.Error("expected error on empty inbounds, got nil")
	}
}
