//go:build xray

package ratelimit

import (
	"fmt"
	"strings"
	"testing"
)

// recordingExec 记录调用参数并模拟 iptables 规则表状态。
// 对 iptables -A/-D/-C 维护 key=(sport,mark) 的计数，使 mock 行为贴近真实 iptables：
//   - -A: 计数 +1（返回 nil）
//   - -D: 计数 -1（若计数>0）；否则返回 "not found"
//   - -C: 计数>0 返回 nil；否则返回 "not found"
type recordingExec struct {
	calls    [][]string
	iptRules map[string]int
	err      error // 对非 iptables 命令的默认错误
}

func newRecordingExec() *recordingExec {
	return &recordingExec{iptRules: map[string]int{}}
}

func (r *recordingExec) run(cmd string, args ...string) error {
	combined := append([]string{cmd}, args...)
	r.calls = append(r.calls, combined)

	if cmd != "iptables" {
		return r.err
	}
	// 解析 iptables 语义：--sport X  -j MARK --set-mark Y
	action := ""
	sport := ""
	mark := ""
	for i, a := range args {
		switch a {
		case "-A", "-D", "-C":
			action = a
		case "--sport":
			if i+1 < len(args) {
				sport = args[i+1]
			}
		case "--set-mark":
			if i+1 < len(args) {
				mark = args[i+1]
			}
		}
	}
	key := sport + "|" + mark
	switch action {
	case "-A":
		r.iptRules[key]++
		return nil
	case "-D":
		if r.iptRules[key] > 0 {
			r.iptRules[key]--
			return nil
		}
		return fmt.Errorf("iptables: rule not found")
	case "-C":
		if r.iptRules[key] > 0 {
			return nil
		}
		return fmt.Errorf("iptables: no such rule")
	}
	return r.err
}

func (r *recordingExec) callsMatching(cmd string) [][]string {
	var out [][]string
	for _, c := range r.calls {
		if c[0] == cmd {
			out = append(out, c)
		}
	}
	return out
}

func TestManager_ApplyFirstTime(t *testing.T) {
	rec := newRecordingExec()
	m := NewManager("eth0", rec.run)

	tiers := map[string]TierConfig{
		"vip":  {MarkID: 1, PoolMbps: 100, PortRange: "34521-34524"},
		"svip": {MarkID: 2, PoolMbps: 500, PortRange: "45000-45003"},
	}
	if err := m.Apply(tiers); err != nil {
		t.Fatalf("Apply err: %v", err)
	}

	// 首次 Apply：1 root qdisc + 2 tier * (class + leaf qdisc + filter) = 7 tc 调用
	if got := len(rec.callsMatching("tc")); got != 7 {
		t.Errorf("expected 7 tc calls, got %d", got)
	}
	// iptables 调用：每 tier -C（miss）+ -A = 2 次；共 4
	if got := len(rec.callsMatching("iptables")); got != 4 {
		t.Errorf("expected 4 iptables calls, got %d: %v", got, rec.callsMatching("iptables"))
	}
	// iptables 表里应该各有一条规则（mark=1 sport=34521:34524, mark=2 sport=45000:45003）
	if rec.iptRules["34521:34524|1"] != 1 {
		t.Errorf("vip rule count = %d, want 1", rec.iptRules["34521:34524|1"])
	}
	if rec.iptRules["45000:45003|2"] != 1 {
		t.Errorf("svip rule count = %d, want 1", rec.iptRules["45000:45003|2"])
	}
}

func TestManager_ApplyIdempotent(t *testing.T) {
	rec := newRecordingExec()
	m := NewManager("eth0", rec.run)

	tiers := map[string]TierConfig{
		"vip": {MarkID: 1, PoolMbps: 100, PortRange: "443"},
	}
	if err := m.Apply(tiers); err != nil {
		t.Fatal(err)
	}
	firstCount := len(rec.calls)

	// 同样配置再 Apply 一次：diff 为空，不应有新命令
	if err := m.Apply(tiers); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != firstCount {
		t.Errorf("second Apply should be no-op; calls grew from %d to %d", firstCount, len(rec.calls))
	}
	// iptables 表仍然只有一条（幂等）
	if rec.iptRules["443|1"] != 1 {
		t.Errorf("rule count = %d, want 1 (idempotent)", rec.iptRules["443|1"])
	}
}

func TestManager_ApplyPoolChange(t *testing.T) {
	rec := newRecordingExec()
	m := NewManager("eth0", rec.run)

	_ = m.Apply(map[string]TierConfig{"vip": {MarkID: 1, PoolMbps: 100, PortRange: "443"}})
	baseline := len(rec.calls)

	// 只改 pool
	_ = m.Apply(map[string]TierConfig{"vip": {MarkID: 1, PoolMbps: 200, PortRange: "443"}})
	newCalls := rec.calls[baseline:]

	if len(newCalls) != 1 {
		t.Errorf("pool change should emit 1 cmd, got %d: %v", len(newCalls), newCalls)
	}
	if newCalls[0][0] != "tc" || !strings.Contains(strings.Join(newCalls[0], " "), "200mbit") {
		t.Errorf("expected tc class change to 200mbit, got %v", newCalls[0])
	}
}

func TestManager_ApplyPortRangeChange_RebuildsIptables(t *testing.T) {
	rec := newRecordingExec()
	m := NewManager("eth0", rec.run)

	_ = m.Apply(map[string]TierConfig{"vip": {MarkID: 1, PoolMbps: 100, PortRange: "443"}})
	baseline := len(rec.calls)

	_ = m.Apply(map[string]TierConfig{"vip": {MarkID: 1, PoolMbps: 100, PortRange: "8443"}})
	newCalls := rec.calls[baseline:]

	hasIptablesDel443 := false
	hasIptablesAdd8443 := false
	for _, c := range newCalls {
		line := strings.Join(c, " ")
		if c[0] == "iptables" && strings.Contains(line, "--sport 443") && strings.Contains(line, "-D") {
			hasIptablesDel443 = true
		}
		if c[0] == "iptables" && strings.Contains(line, "--sport 8443") && strings.Contains(line, "-A") {
			hasIptablesAdd8443 = true
		}
	}
	if !hasIptablesDel443 {
		t.Errorf("expected iptables -D for old port 443, calls=%v", newCalls)
	}
	if !hasIptablesAdd8443 {
		t.Errorf("expected iptables -A for new port 8443, calls=%v", newCalls)
	}

	// 最终状态：443 规则已删，8443 规则已加
	if rec.iptRules["443|1"] != 0 {
		t.Errorf("old port 443 should have 0 rules, got %d", rec.iptRules["443|1"])
	}
	if rec.iptRules["8443|1"] != 1 {
		t.Errorf("new port 8443 should have 1 rule, got %d", rec.iptRules["8443|1"])
	}
}

func TestManager_ApplyExecError(t *testing.T) {
	rec := &recordingExec{err: fmt.Errorf("mock err"), iptRules: map[string]int{}}
	m := NewManager("eth0", rec.run)

	tiers := map[string]TierConfig{"vip": {MarkID: 1, PoolMbps: 100, PortRange: "443"}}
	if err := m.Apply(tiers); err == nil {
		t.Error("expected error, got nil")
	}
}

func TestManager_Disable(t *testing.T) {
	rec := newRecordingExec()
	m := NewManager("eth0", rec.run)

	_ = m.Apply(map[string]TierConfig{"vip": {MarkID: 1, PoolMbps: 100, PortRange: "443"}})

	if err := m.Disable(); err != nil {
		t.Fatal(err)
	}

	// iptables 表应清空
	if rec.iptRules["443|1"] != 0 {
		t.Errorf("iptables rule should be cleaned on Disable, count=%d", rec.iptRules["443|1"])
	}

	// 必须发出 tc qdisc del
	hasTCDel := false
	for _, c := range rec.calls {
		if c[0] == "tc" && strings.Contains(strings.Join(c, " "), "qdisc del") {
			hasTCDel = true
		}
	}
	if !hasTCDel {
		t.Errorf("expected tc qdisc del on Disable")
	}

	// Disable 后再 Apply 应该重新下发所有规则
	if err := m.Apply(map[string]TierConfig{"vip": {MarkID: 1, PoolMbps: 100, PortRange: "443"}}); err != nil {
		t.Fatal(err)
	}
	if rec.iptRules["443|1"] != 1 {
		t.Errorf("after re-Apply rule count = %d, want 1", rec.iptRules["443|1"])
	}
}

// agent 重启后残留：iptables 里已经有 2 条相同规则（历史遗留），Apply 应清干净并留下恰好一条
func TestManager_EnsureIptables_CleansLeftover(t *testing.T) {
	rec := newRecordingExec()
	// 预置 2 条残留规则
	rec.iptRules["443|1"] = 2

	m := NewManager("eth0", rec.run)
	_ = m.Apply(map[string]TierConfig{"vip": {MarkID: 1, PoolMbps: 100, PortRange: "443"}})

	// 最终表里应该有恰好 1 条
	if rec.iptRules["443|1"] != 1 {
		t.Errorf("expected exactly 1 rule after Apply (cleaned leftover), got %d", rec.iptRules["443|1"])
	}
	// 必须有 -D 调用清理残留
	delCount := 0
	for _, c := range rec.calls {
		if c[0] == "iptables" && len(c) > 3 && c[3] == "-D" {
			delCount++
		}
	}
	if delCount != 2 {
		t.Errorf("expected 2 -D calls to clean 2 leftover rules, got %d", delCount)
	}
}
