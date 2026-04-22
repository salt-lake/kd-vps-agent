//go:build xray

package ratelimit

// TierState 单个 tier 的已应用状态快照。
type TierState struct {
	MarkID    int
	PoolMbps  int
	PortRange string // xray inbound 监听端口（含范围），iptables 按此做源端口匹配打 mark
}

// DiffState 返回新旧状态的三态差分：add / change / remove。
// mark_id 或 port_range 变化按 remove+add 处理（这两个变化都要重建 iptables 规则）。
// 仅 pool_mbps 变化走 change（只要 tc class change，iptables/filter 不动）。
func DiffState(oldState, newState map[string]TierState) (add, change, remove []string) {
	for name, nt := range newState {
		ot, ok := oldState[name]
		if !ok {
			add = append(add, name)
			continue
		}
		if ot.MarkID != nt.MarkID || ot.PortRange != nt.PortRange {
			// mark 或端口变了：先 remove 旧，再 add 新（iptables/class/filter 都要重建）
			remove = append(remove, name)
			add = append(add, name)
			continue
		}
		if ot.PoolMbps != nt.PoolMbps {
			change = append(change, name)
		}
	}
	for name := range oldState {
		if _, ok := newState[name]; !ok {
			remove = append(remove, name)
		}
	}
	return
}
