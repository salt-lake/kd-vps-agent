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

// 真实 iproute2 5.15 的 tc -s class show 文本输出（Ubuntu 22.04 上 -j 不生效）
const tcTextSample = `class htb 1:10 root leaf 10: prio 0 rate 100Mbit ceil 100Mbit burst 1600b cburst 1600b
 Sent 1626 bytes 23 pkt (dropped 5, overlimits 12 requeues 0)
 backlog 2048b 0p requeues 0
 lended: 23 borrowed: 0 giants: 0
 tokens: 1907 ctokens: 1907

class htb 1:20 root leaf 20: prio 0 rate 500Mbit ceil 500Mbit burst 1500b cburst 1500b
 Sent 9999 bytes 88 pkt (dropped 0, overlimits 0 requeues 0)
 backlog 0b 0p requeues 0
 lended: 0 borrowed: 0 giants: 0
 tokens: 390 ctokens: 390

class fq_codel 10:19 parent 10:
 (dropped 0, overlimits 0 requeues 0)
 backlog 0b 0p requeues 0
  deficit 1440 count 0 lastcount 0 ldelay 3us
`

func TestParseTcStatsText(t *testing.T) {
	stats := parseTcStatsText([]byte(tcTextSample))
	if len(stats) != 2 {
		t.Fatalf("expected 2 htb classes, got %d: %+v", len(stats), stats)
	}
	vip, ok := stats["1:10"]
	if !ok {
		t.Fatal("1:10 missing")
	}
	if vip.SentBytes != 1626 || vip.Dropped != 5 || vip.Overlimits != 12 || vip.BacklogBytes != 2048 {
		t.Errorf("1:10 mismatch: %+v", vip)
	}
	svip, ok := stats["1:20"]
	if !ok {
		t.Fatal("1:20 missing")
	}
	if svip.SentBytes != 9999 || svip.Dropped != 0 {
		t.Errorf("1:20 mismatch: %+v", svip)
	}
	// fq_codel 子 class 不应出现
	if _, ok := stats["10:19"]; ok {
		t.Error("fq_codel child class should be skipped")
	}
}

func TestParseTcStatsText_Empty(t *testing.T) {
	stats := parseTcStatsText([]byte(""))
	if len(stats) != 0 {
		t.Errorf("empty input should give empty map, got %+v", stats)
	}
}
