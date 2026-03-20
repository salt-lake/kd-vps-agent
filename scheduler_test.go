package main

import (
	"testing"
	"time"
)

func TestHostJitter_LastOctetMod60(t *testing.T) {
	cases := []struct {
		host string
		want time.Duration
	}{
		{"1.2.3.4", 4 * time.Second},
		{"10.0.0.59", 59 * time.Second},
		{"10.0.0.60", 0 * time.Second}, // 60 % 60 == 0
		{"10.0.0.61", 1 * time.Second},
		{"10.0.0.120", 0 * time.Second}, // 120 % 60 == 0
		{"192.168.1.255", 15 * time.Second}, // 255 % 60 == 15
	}
	for _, c := range cases {
		got := hostJitter(c.host)
		if got != c.want {
			t.Errorf("hostJitter(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestHostJitter_NonNumericLastOctet(t *testing.T) {
	// 非 IP 格式，last segment 非数字，返回 0
	cases := []string{"hostname", "abc.def", "1.2.3.x"}
	for _, h := range cases {
		got := hostJitter(h)
		if got != 0 {
			t.Errorf("hostJitter(%q) = %v, want 0", h, got)
		}
	}
}

func TestHostJitter_EmptyString(t *testing.T) {
	// Split("", ".") 返回 [""]，Atoi("") 报错，应返回 0
	if got := hostJitter(""); got != 0 {
		t.Errorf("hostJitter(%q) = %v, want 0", "", got)
	}
}
