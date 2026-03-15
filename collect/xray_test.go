//go:build xray

package collect

import (
	"testing"
)

func TestCountXrayOnline(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   int
	}{
		{
			name:   "empty string",
			output: "",
			want:   0,
		},
		{
			name: "all downlink zero",
			// Each entry is on one line: name contains >>>traffic>>>downlink and value: 0
			output: `  name:"user>>>alice@example.com>>>traffic>>>downlink"  value: 0 >
  name:"user>>>bob@example.com>>>traffic>>>downlink"  value: 0 >`,
			want: 0,
		},
		{
			name: "all downlink non-zero",
			output: `  name:"user>>>alice@example.com>>>traffic>>>downlink"  value: 12345 >
  name:"user>>>bob@example.com>>>traffic>>>downlink"  value: 67890 >`,
			want: 2,
		},
		{
			name: "mixed zero and non-zero",
			output: `  name:"user>>>alice@example.com>>>traffic>>>downlink"  value: 0 >
  name:"user>>>bob@example.com>>>traffic>>>downlink"  value: 999 >
  name:"user>>>carol@example.com>>>traffic>>>downlink"  value: 0 >
  name:"user>>>dave@example.com>>>traffic>>>downlink"  value: 1 >`,
			want: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := countXrayOnline(tc.output)
			if got != tc.want {
				t.Errorf("countXrayOnline() = %d, want %d", got, tc.want)
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
