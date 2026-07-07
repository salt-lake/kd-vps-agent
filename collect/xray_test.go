//go:build xray

package collect

import (
	"encoding/json"
	"testing"
)

func TestParsePort(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []portRange
		wantErr bool
	}{
		// 向后兼容：旧格式
		{"int port", `443`, []portRange{{443, 443}}, false},
		{"string single", `"443"`, []portRange{{443, 443}}, false},
		{"string range", `"56771-56774"`, []portRange{{56771, 56774}}, false},
		// 新增：逗号列表
		{"range plus port", `"56771-56774,443"`, []portRange{{56771, 56774}, {443, 443}}, false},
		{"two single ports", `"443,8443"`, []portRange{{443, 443}, {8443, 8443}}, false},
		{"port plus range", `"443,1000-2000"`, []portRange{{443, 443}, {1000, 2000}}, false},
		{"with spaces", `"56771-56774, 443"`, []portRange{{56771, 56774}, {443, 443}}, false},
		// 非法
		{"garbage", `"abc"`, nil, true},
		{"bad segment", `"23330,abc"`, nil, true},
		{"bad range", `"a-b"`, nil, true},
		{"empty string", `""`, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePort(json.RawMessage(tc.raw))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("segments = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("seg[%d] = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestBuildSsPortFilter(t *testing.T) {
	tests := []struct {
		name string
		prs  []portRange
		want string
	}{
		// 单段：与旧格式逐字节一致（无括号）
		{"single port", []portRange{{443, 443}}, "sport = :443"},
		{"single range", []portRange{{23327, 23330}}, "sport >= :23327 and sport <= :23330"},
		// 多段：括号带空格 + or
		{"range plus port", []portRange{{23327, 23330}, {443, 443}},
			"( sport >= :23327 and sport <= :23330 ) or ( sport = :443 )"},
		{"two ports", []portRange{{443, 443}, {8443, 8443}},
			"( sport = :443 ) or ( sport = :8443 )"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildSsPortFilter(tc.prs); got != tc.want {
				t.Errorf("filter = %q, want %q", got, tc.want)
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
