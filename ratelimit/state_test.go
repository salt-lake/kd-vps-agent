//go:build xray

package ratelimit

import (
	"reflect"
	"sort"
	"testing"
)

func TestDiffState(t *testing.T) {
	oldState := map[string]TierState{
		"vip":  {MarkID: 1, PoolMbps: 100},
		"svip": {MarkID: 2, PoolMbps: 500},
	}
	newState := map[string]TierState{
		"vip":   {MarkID: 1, PoolMbps: 200}, // pool 改了
		"svip":  {MarkID: 2, PoolMbps: 500}, // 没变
		"trial": {MarkID: 3, PoolMbps: 50},  // 新增
	}

	add, change, remove := DiffState(oldState, newState)
	sort.Strings(add)
	sort.Strings(change)
	sort.Strings(remove)

	if !reflect.DeepEqual(add, []string{"trial"}) {
		t.Errorf("add got %v, want [trial]", add)
	}
	if !reflect.DeepEqual(change, []string{"vip"}) {
		t.Errorf("change got %v, want [vip]", change)
	}
	if len(remove) != 0 {
		t.Errorf("remove got %v, want empty", remove)
	}
}

func TestDiffState_RemoveOnly(t *testing.T) {
	oldState := map[string]TierState{"vip": {MarkID: 1, PoolMbps: 100}}
	newState := map[string]TierState{}

	add, change, remove := DiffState(oldState, newState)
	if len(add) != 0 || len(change) != 0 {
		t.Errorf("add/change should be empty")
	}
	if !reflect.DeepEqual(remove, []string{"vip"}) {
		t.Errorf("remove got %v, want [vip]", remove)
	}
}

func TestDiffState_MarkIDChangeCountsAsRemoveAdd(t *testing.T) {
	// mark_id 变化：旧 tier 要 remove（旧 classid 不再用），新 tier 要 add（新 classid）
	oldState := map[string]TierState{"vip": {MarkID: 1, PoolMbps: 100, PortRange: "443"}}
	newState := map[string]TierState{"vip": {MarkID: 5, PoolMbps: 100, PortRange: "443"}}

	add, change, remove := DiffState(oldState, newState)
	if !reflect.DeepEqual(add, []string{"vip"}) || !reflect.DeepEqual(remove, []string{"vip"}) || len(change) != 0 {
		t.Errorf("mark change: add=%v change=%v remove=%v", add, change, remove)
	}
}

func TestDiffState_PortRangeChangeCountsAsRemoveAdd(t *testing.T) {
	// port range 变化：iptables 源端口匹配规则要重建，同样走 remove+add
	oldState := map[string]TierState{"vip": {MarkID: 1, PoolMbps: 100, PortRange: "34521-34524"}}
	newState := map[string]TierState{"vip": {MarkID: 1, PoolMbps: 100, PortRange: "45000-45003"}}

	add, change, remove := DiffState(oldState, newState)
	if !reflect.DeepEqual(add, []string{"vip"}) || !reflect.DeepEqual(remove, []string{"vip"}) || len(change) != 0 {
		t.Errorf("port change: add=%v change=%v remove=%v", add, change, remove)
	}
}
