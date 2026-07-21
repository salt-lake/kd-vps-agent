//go:build xray

package collect

import (
	"testing"

	statscmd "github.com/salt-lake/kd-vps-agent/xray/proto/app/stats/command"
)

func TestParseUserCounter(t *testing.T) {
	cases := []struct {
		name   string
		uuid   string
		uplink bool
		ok     bool
	}{
		{"user>>>xray@0e6ac7ff-8888-4444-9999-c72fd2a0efc1>>>traffic>>>uplink", "0e6ac7ff-8888-4444-9999-c72fd2a0efc1", true, true},
		{"user>>>xray@abc>>>traffic>>>downlink", "abc", false, true},
		{"inbound>>>reality-in>>>traffic>>>uplink", "", false, false},   // 非 user 计数器
		{"user>>>default@test>>>traffic>>>uplink", "", false, false},    // 静态占位用户，非本系统注入
		{"user>>>xray@>>>traffic>>>uplink", "", false, false},           // 空 uuid
		{"user>>>xray@abc>>>traffic>>>sideways", "", false, false},      // 未知方向
		{"garbage", "", false, false},
	}
	for _, c := range cases {
		uuid, uplink, ok := parseUserCounter(c.name)
		if uuid != c.uuid || uplink != c.uplink || ok != c.ok {
			t.Errorf("parseUserCounter(%q) = (%q,%v,%v), want (%q,%v,%v)",
				c.name, uuid, uplink, ok, c.uuid, c.uplink, c.ok)
		}
	}
}

func TestAggregateUserStats(t *testing.T) {
	stats := []*statscmd.Stat{
		{Name: "user>>>xray@u1>>>traffic>>>uplink", Value: 100},
		{Name: "user>>>xray@u1>>>traffic>>>downlink", Value: 200},
		{Name: "user>>>xray@u2>>>traffic>>>uplink", Value: 0},  // 零值丢弃
		{Name: "inbound>>>api>>>traffic>>>uplink", Value: 999}, // 非用户计数器
	}
	got := aggregateUserStats(stats)
	if len(got) != 1 {
		t.Fatalf("len=%d want 1: %v", len(got), got)
	}
	if got["u1"] != [2]int64{100, 200} {
		t.Errorf("u1=%v want [100 200]", got["u1"])
	}
	if aggregateUserStats(nil) != nil {
		t.Error("empty input should return nil")
	}
}
