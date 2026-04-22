//go:build xray

package ratelimit

import "testing"

func TestParseIfaceFromIPRouteOutput(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "standard output",
			output: "1.1.1.1 via 10.0.0.1 dev eth0 src 10.0.0.123 uid 0",
			want:   "eth0",
		},
		{
			name:   "ens-style iface",
			output: "1.1.1.1 via 192.168.1.1 dev ens3 src 192.168.1.10 uid 1000",
			want:   "ens3",
		},
		{
			name:   "no dev keyword",
			output: "unreachable",
			want:   "",
		},
		{
			name:   "empty",
			output: "",
			want:   "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseIfaceFromIPRouteOutput(tc.output)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
