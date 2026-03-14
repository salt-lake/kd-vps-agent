package collect

import (
	"testing"
)

func TestParseSwanConnCount(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "empty output",
			output: "",
			want:   "0",
		},
		{
			name: "no ESTABLISHED lines",
			output: `Connections:
     net-net:  192.168.1.0/24...192.168.2.0/24
Security Associations (0 up, 0 connecting):
  none`,
			want: "0",
		},
		{
			name: "two IPv4 connections",
			output: `Security Associations (2 up, 0 connecting):
  net-net[1]: ESTABLISHED 1 minute ago, 10.0.0.1[server]...1.2.3.4[client]
  net-net[2]: ESTABLISHED 2 minutes ago, 10.0.0.1[server]...5.6.7.8[client]`,
			want: "2",
		},
		{
			name: "mix IPv4 and non-IPv4",
			output: `Security Associations (2 up, 0 connecting):
  net-net[1]: ESTABLISHED 1 minute ago, 10.0.0.1[server]...1.2.3.4[client]
  net-net[2]: ESTABLISHED 2 minutes ago, 10.0.0.1[server]...myhost.example.com[client]`,
			want: "1",
		},
		{
			name: "ESTABLISHED line without matching peer pattern",
			output: `  bad[1]: ESTABLISHED but no peer info`,
			want:   "0",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSwanConnCount(tc.output)
			if got != tc.want {
				t.Errorf("parseSwanConnCount() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSwanVersionRe(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		wantV  string
		wantOK bool
	}{
		{
			name:   "normal version string",
			input:  "Linux strongSwan U6.0.4/K5.15.0-generic",
			wantV:  "6.0.4",
			wantOK: true,
		},
		{
			name:   "no match",
			input:  "some other version output",
			wantOK: false,
		},
		{
			name:   "different version",
			input:  "Linux strongSwan U5.9.1/K4.19.0",
			wantV:  "5.9.1",
			wantOK: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := swanVersionRe.FindStringSubmatch(tc.input)
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
