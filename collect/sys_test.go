package collect

import (
	"testing"
)

func TestParseMemInfo(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "normal meminfo",
			data: `MemTotal:       16384000 kB
MemFree:         1000000 kB
MemAvailable:    4096000 kB
Buffers:          512000 kB
Cached:          2048000 kB
`,
			// used = 16384000 - 4096000 = 12288000, pct = 12288000*100/16384000 = 75
			want: "75",
		},
		{
			name: "total zero",
			data: `MemFree: 1000 kB
MemAvailable: 500 kB
`,
			want: "",
		},
		{
			name: "100% used",
			data: `MemTotal: 1000 kB
MemAvailable: 0 kB
`,
			want: "100",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseMemInfo(tc.data)
			if got != tc.want {
				t.Errorf("parseMemInfo() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseCPUStat(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		wantIdle  uint64
		wantTotal uint64
	}{
		{
			name: "normal cpu line",
			// cpu  user nice system idle iowait irq softirq steal ...
			// cpu  100  0    50     200  10     0   0       0
			// total = 100+0+50+200+10+0+0+0 = 360, idle = 200
			data:      "cpu  100 0 50 200 10 0 0 0\ncpu0 50 0 25 100 5 0 0 0\n",
			wantIdle:  200,
			wantTotal: 360,
		},
		{
			name:      "no cpu line",
			data:      "cpuinfo\nsome other content\n",
			wantIdle:  0,
			wantTotal: 0,
		},
		{
			name:      "empty",
			data:      "",
			wantIdle:  0,
			wantTotal: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idle, total := parseCPUStat(tc.data)
			if idle != tc.wantIdle || total != tc.wantTotal {
				t.Errorf("parseCPUStat() idle=%d total=%d, want idle=%d total=%d",
					idle, total, tc.wantIdle, tc.wantTotal)
			}
		})
	}
}
