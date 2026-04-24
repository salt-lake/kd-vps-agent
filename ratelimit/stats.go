//go:build xray

package ratelimit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
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

// CollectTcStats 调用 tc -s 读取 class 统计。
// 优先用 -j（iproute2 5.19+），失败则回退文本解析（兼容 Ubuntu 22.04 自带的 5.15）。
func CollectTcStats(ctx context.Context, iface string) (map[string]TierStats, error) {
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c, "tc", "-s", "-j", "class", "show", "dev", iface).Output()
	if err != nil {
		return nil, fmt.Errorf("run tc: %w", err)
	}
	if stats, jerr := ParseTcStatsJSON(out); jerr == nil {
		return stats, nil
	}
	// 老版 iproute2：-j 被忽略，输出是纯文本
	return parseTcStatsText(out), nil
}

var (
	// 匹配 "class htb 1:10" 或 "class fq_codel 10:19"，我们只要 htb 的
	classHeaderRe = regexp.MustCompile(`^class\s+htb\s+(\d+:\d+)\s`)
	// 匹配 " Sent 123 bytes 45 pkt (dropped 6, overlimits 7 requeues 0)"
	sentLineRe = regexp.MustCompile(`Sent\s+(\d+)\s+bytes.*dropped\s+(\d+),\s+overlimits\s+(\d+)`)
	// 匹配 " backlog 0b 0p requeues 0"
	backlogLineRe = regexp.MustCompile(`backlog\s+(\d+)b`)
)

// parseTcStatsText 解析 `tc -s class show dev X` 的纯文本输出。
// 结构按 class 块划分：header 行以 "class htb 1:X" 开头，紧跟若干 stats 行。
// 只处理 htb class，fq_codel 子 class 跳过。
func parseTcStatsText(out []byte) map[string]TierStats {
	result := make(map[string]TierStats)
	scanner := bufio.NewScanner(bytes.NewReader(out))
	var current *TierStats
	flush := func() {
		if current != nil && current.ClassID != "" {
			result[current.ClassID] = *current
		}
		current = nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if m := classHeaderRe.FindStringSubmatch(line); m != nil {
			flush()
			current = &TierStats{ClassID: m[1]}
			continue
		}
		if strings.HasPrefix(line, "class ") {
			// 其他 class 类型（fq_codel 等），flush 上一个并跳过
			flush()
			continue
		}
		if current == nil {
			continue
		}
		if m := sentLineRe.FindStringSubmatch(line); m != nil {
			current.SentBytes = parseUint(m[1])
			current.Dropped = parseUint(m[2])
			current.Overlimits = parseUint(m[3])
			continue
		}
		if m := backlogLineRe.FindStringSubmatch(line); m != nil {
			current.BacklogBytes = parseUint(m[1])
		}
	}
	flush()
	return result
}

func parseUint(s string) uint64 {
	n, _ := strconv.ParseUint(s, 10, 64)
	return n
}
