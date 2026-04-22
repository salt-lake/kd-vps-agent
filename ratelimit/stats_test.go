//go:build xray

package ratelimit

import "testing"

// 真实 tc -s -j class show 输出（iproute2 5.10+ 格式）
const tcJSONSample = `[
  {
    "class": "htb",
    "handle": "1:10",
    "parent": "1:",
    "rate": 26214400,
    "ceil": 26214400,
    "stats": {
      "bytes": 123456789,
      "packets": 12345,
      "drops": 67,
      "overlimits": 89,
      "requeues": 0,
      "backlog": 2048,
      "qlen": 3
    }
  },
  {
    "class": "htb",
    "handle": "1:20",
    "parent": "1:",
    "stats": {
      "bytes": 987654321,
      "packets": 98765,
      "drops": 1,
      "overlimits": 2,
      "backlog": 0
    }
  }
]`

func TestParseTcStatsJSON(t *testing.T) {
	stats, err := ParseTcStatsJSON([]byte(tcJSONSample))
	if err != nil {
		t.Fatalf("parse err: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("expected 2 classes, got %d", len(stats))
	}
	vip, ok := stats["1:10"]
	if !ok {
		t.Fatal("1:10 missing")
	}
	if vip.SentBytes != 123456789 || vip.Dropped != 67 || vip.Overlimits != 89 || vip.BacklogBytes != 2048 {
		t.Errorf("1:10 mismatch: %+v", vip)
	}
	svip, ok := stats["1:20"]
	if !ok {
		t.Fatal("1:20 missing")
	}
	if svip.SentBytes != 987654321 || svip.Dropped != 1 {
		t.Errorf("1:20 mismatch: %+v", svip)
	}
}

func TestParseTcStatsJSON_Empty(t *testing.T) {
	stats, err := ParseTcStatsJSON([]byte("[]"))
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 0 {
		t.Errorf("expected empty, got %d", len(stats))
	}
}

func TestParseTcStatsJSON_Malformed(t *testing.T) {
	_, err := ParseTcStatsJSON([]byte("not json"))
	if err == nil {
		t.Error("expected error on malformed input")
	}
}
