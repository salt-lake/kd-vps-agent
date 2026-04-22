//go:build xray

package collect

import (
	"context"
	"log"

	"github.com/salt-lake/kd-vps-agent/ratelimit"
)

// TcStatsProvider 采集 tc class 统计写入 Payload.TcStats（仅 xray 限速启用时）。
type TcStatsProvider struct {
	fetcher func() (map[string]TierStatsDTO, error)
}

// NewTcStatsProvider 构造。iface 为目标网卡名；enabled=false 时 Collect 是 no-op。
func NewTcStatsProvider(iface string, enabled bool) *TcStatsProvider {
	if !enabled {
		return &TcStatsProvider{fetcher: func() (map[string]TierStatsDTO, error) { return nil, nil }}
	}
	return &TcStatsProvider{fetcher: func() (map[string]TierStatsDTO, error) {
		raw, err := ratelimit.CollectTcStats(context.Background(), iface)
		if err != nil {
			return nil, err
		}
		out := make(map[string]TierStatsDTO, len(raw))
		for k, v := range raw {
			out[k] = TierStatsDTO{
				ClassID:      v.ClassID,
				SentBytes:    v.SentBytes,
				Dropped:      v.Dropped,
				Overlimits:   v.Overlimits,
				BacklogBytes: v.BacklogBytes,
			}
		}
		return out, nil
	}}
}

// Collect 填充 Payload.TcStats。失败/空都不写字段（保持 nil，JSON omitempty 省略）。
func (p *TcStatsProvider) Collect(pl *Payload) {
	stats, err := p.fetcher()
	if err != nil {
		log.Printf("tc stats: collect err=%v (skip)", err)
		return
	}
	if len(stats) == 0 {
		return
	}
	pl.TcStats = stats
}
