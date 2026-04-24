//go:build xray

package collect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReadProxyInbounds(t *testing.T) {
	tests := []struct {
		name   string
		config map[string]any
		want   []proxyInbound
	}{
		{
			name: "single proxy (unmigrated)",
			config: map[string]any{
				"inbounds": []map[string]any{
					{"tag": "api", "port": 10085},
					{"tag": "proxy", "port": "30000-30003"},
				},
			},
			want: []proxyInbound{{Tag: "proxy", PortRange: portRange{30000, 30003}}},
		},
		{
			name: "multi proxy (migrated)",
			config: map[string]any{
				"inbounds": []map[string]any{
					{"tag": "api", "port": 10085},
					{"tag": "proxy-vip", "port": "39414-39417"},
					{"tag": "proxy-svip", "port": "20360-20363"},
				},
			},
			want: []proxyInbound{
				{Tag: "proxy-vip", PortRange: portRange{39414, 39417}},
				{Tag: "proxy-svip", PortRange: portRange{20360, 20363}},
			},
		},
		{
			name: "int port",
			config: map[string]any{
				"inbounds": []map[string]any{
					{"tag": "proxy", "port": 443},
				},
			},
			want: []proxyInbound{{Tag: "proxy", PortRange: portRange{443, 443}}},
		},
		{
			name: "skips non-proxy tags",
			config: map[string]any{
				"inbounds": []map[string]any{
					{"tag": "api", "port": 10085},
					{"tag": "direct", "port": 8080},
					{"tag": "blocked", "port": 9090},
				},
			},
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			data, _ := json.Marshal(tc.config)
			if err := os.WriteFile(path, data, 0644); err != nil {
				t.Fatal(err)
			}
			got, err := readProxyInbounds(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d inbounds, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("inbound[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestReadProxyInbounds_BadInputs(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		if _, err := readProxyInbounds("/nonexistent/config.json"); err == nil {
			t.Error("expected error")
		}
	})
	t.Run("malformed JSON", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		_ = os.WriteFile(path, []byte("not json"), 0644)
		if _, err := readProxyInbounds(path); err == nil {
			t.Error("expected parse error")
		}
	})
	t.Run("empty inbounds", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		_ = os.WriteFile(path, []byte(`{"inbounds":[]}`), 0644)
		got, err := readProxyInbounds(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("expected 0 inbounds, got %d", len(got))
		}
	})
}

func TestAggregateConnCounts(t *testing.T) {
	inbounds := []proxyInbound{
		{Tag: "proxy-vip", PortRange: portRange{39414, 39417}},
		{Tag: "proxy-svip", PortRange: portRange{20360, 20363}},
	}
	// 模拟 ss 返回：vip 端口 3 个 IP（含一个和 svip 重叠），svip 端口 2 个 IP
	stub := func(pr portRange) map[string]struct{} {
		if pr.Start == 39414 {
			return map[string]struct{}{"1.1.1.1": {}, "2.2.2.2": {}, "3.3.3.3": {}}
		}
		if pr.Start == 20360 {
			return map[string]struct{}{"3.3.3.3": {}, "4.4.4.4": {}}
		}
		return nil
	}
	total, byTag := aggregateConnCountsWith(inbounds, stub)

	// 全局唯一 IP：1.1.1.1, 2.2.2.2, 3.3.3.3, 4.4.4.4 = 4
	if total != "4" {
		t.Errorf("total = %q, want 4", total)
	}
	if byTag["proxy-vip"] != "3" {
		t.Errorf("proxy-vip = %q, want 3", byTag["proxy-vip"])
	}
	if byTag["proxy-svip"] != "2" {
		t.Errorf("proxy-svip = %q, want 2", byTag["proxy-svip"])
	}
}

func TestAggregateConnCounts_SingleInbound(t *testing.T) {
	// 单 "proxy" inbound 节点（兼容模式）
	inbounds := []proxyInbound{{Tag: "proxy", PortRange: portRange{30000, 30003}}}
	stub := func(pr portRange) map[string]struct{} {
		return map[string]struct{}{"1.1.1.1": {}, "2.2.2.2": {}}
	}
	total, byTag := aggregateConnCountsWith(inbounds, stub)
	if total != "2" {
		t.Errorf("total = %q, want 2", total)
	}
	if byTag["proxy"] != "2" {
		t.Errorf("proxy = %q, want 2", byTag["proxy"])
	}
}

func TestAggregateConnCounts_NoInbounds(t *testing.T) {
	total, byTag := aggregateConnCountsWith(nil, nil)
	if total != "0" {
		t.Errorf("total = %q, want 0", total)
	}
	if len(byTag) != 0 {
		t.Errorf("byTag should be empty, got %v", byTag)
	}
}

func TestParsePort(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    portRange
		wantErr bool
	}{
		{"int", `443`, portRange{443, 443}, false},
		{"string single", `"8080"`, portRange{8080, 8080}, false},
		{"string range", `"30000-30003"`, portRange{30000, 30003}, false},
		{"invalid range", `"abc-def"`, portRange{}, true},
		{"invalid int in string", `"notanumber"`, portRange{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePort(json.RawMessage(tc.raw))
			if tc.wantErr {
				if err == nil {
					t.Error("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestXrayVersionRe(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		wantV  string
		wantOK bool
	}{
		{
			name:   "normal version line",
			input:  "Xray 1.8.4 (Xray, Penetrates Everything) custom (go1.21.0 linux/amd64)",
			wantV:  "1.8.4",
			wantOK: true,
		},
		{
			name:   "no match",
			input:  "some other output without version",
			wantOK: false,
		},
		{
			name:   "xray lowercase no match",
			input:  "xray 1.8.4",
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := xrayVersionRe.FindStringSubmatch(tc.input)
			if tc.wantOK {
				if m == nil {
					t.Fatalf("expected match, got nil")
				}
				if m[1] != tc.wantV {
					t.Errorf("version = %q, want %q", m[1], tc.wantV)
				}
			} else {
				if m != nil {
					t.Errorf("expected no match, got %v", m)
				}
			}
		})
	}
}
