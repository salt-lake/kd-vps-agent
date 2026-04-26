//go:build xray

package collect

import (
	"testing"
)

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
