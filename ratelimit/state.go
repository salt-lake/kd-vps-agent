//go:build xray

package ratelimit

// TierState 单个 tier 的已应用状态快照。
type TierState struct {
	MarkID   int
	PoolMbps int
}

// DiffState 返回新旧状态的三态差分：add / change / remove。
// mark_id 变化按 remove+add 处理，避免 classid 漂移。
func DiffState(oldState, newState map[string]TierState) (add, change, remove []string) {
	for name, nt := range newState {
		ot, ok := oldState[name]
		if !ok {
			add = append(add, name)
			continue
		}
		if ot.MarkID != nt.MarkID {
			// mark 变了：先 remove 旧，再 add 新
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
