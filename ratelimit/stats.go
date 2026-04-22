//go:build xray

package ratelimit

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

// TierStats tc class 的统计快照。对应 Payload.TcStats 的 value。
type TierStats struct {
	ClassID      string `json:"classId"`
	SentBytes    uint64 `json:"sent"`
	Dropped      uint64 `json:"dropped"`
	Overlimits   uint64 `json:"overlimits"`
	BacklogBytes uint64 `json:"backlog"`
}

type tcClassEntry struct {
	Handle string `json:"handle"`
	Stats  struct {
		Bytes      uint64 `json:"bytes"`
		Drops      uint64 `json:"drops"`
		Overlimits uint64 `json:"overlimits"`
		Backlog    uint64 `json:"backlog"`
	} `json:"stats"`
}

// ParseTcStatsJSON 解析 `tc -s -j class show dev X` 的 JSON 输出。
// 返回 map[classid]TierStats，其中 classid 形如 "1:10"。
func ParseTcStatsJSON(raw []byte) (map[string]TierStats, error) {
	var entries []tcClassEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("parse tc json: %w", err)
	}
	out := make(map[string]TierStats, len(entries))
	for _, e := range entries {
		if e.Handle == "" {
			continue
		}
		out[e.Handle] = TierStats{
			ClassID:      e.Handle,
			SentBytes:    e.Stats.Bytes,
			Dropped:      e.Stats.Drops,
			Overlimits:   e.Stats.Overlimits,
			BacklogBytes: e.Stats.Backlog,
		}
	}
	return out, nil
}

// CollectTcStats 调用 tc -s -j class show 并解析。
func CollectTcStats(ctx context.Context, iface string) (map[string]TierStats, error) {
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c, "tc", "-s", "-j", "class", "show", "dev", iface).Output()
	if err != nil {
		return nil, fmt.Errorf("run tc: %w", err)
	}
	return ParseTcStatsJSON(out)
}
