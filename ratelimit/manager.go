//go:build xray

package ratelimit

import (
	"fmt"
	"strings"
	"sync"
)

// TierConfig 传给 Apply 的输入。
type TierConfig struct {
	MarkID    int
	PoolMbps  int
	PortRange string // xray inbound 监听端口（范围，如 "34521-34524"），iptables 按此做源端口匹配
}

// ExecFunc 执行单条命令的函数类型，便于测试时注入 mock。
// 生产环境里包一层 exec.Command，测试用 mock 实现。
type ExecFunc func(cmd string, args ...string) error

// Manager 协调 tc 和 iptables 规则，共同实现 tier 限速。
// 策略：iptables -t mangle OUTPUT --sport <portRange> → 打 mark → tc fw filter → HTB class 限速 → fq_codel 公平。
type Manager struct {
	iface string
	exec  ExecFunc

	mu            sync.Mutex
	rootInstalled bool
	state         map[string]TierState
}

// NewManager 构造。iface 为目标网卡名，exec 为执行器。
func NewManager(iface string, exec ExecFunc) *Manager {
	return &Manager{
		iface: iface,
		exec:  exec,
		state: make(map[string]TierState),
	}
}

// Apply 幂等下发 tc 和 iptables 规则。
// - 首次调用：建 root qdisc；为每 tier 建 class+leaf qdisc+filter，并下 iptables mark 规则
// - 后续调用：diff 状态，只动有变化的 tier
func (m *Manager) Apply(tiers map[string]TierConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.rootInstalled {
		if err := m.runTC(buildRootQdiscArgs(m.iface)); err != nil {
			return fmt.Errorf("root qdisc: %w", err)
		}
		m.rootInstalled = true
	}

	newState := make(map[string]TierState, len(tiers))
	for name, t := range tiers {
		newState[name] = TierState{MarkID: t.MarkID, PoolMbps: t.PoolMbps, PortRange: t.PortRange}
	}

	add, change, remove := DiffState(m.state, newState)

	for _, name := range remove {
		m.removeTier(name, m.state[name])
		delete(m.state, name)
	}

	for _, name := range add {
		nt := newState[name]
		if err := m.addTier(name, nt); err != nil {
			return err
		}
		m.state[name] = nt
	}

	for _, name := range change {
		nt := newState[name]
		cid := ClassIDMinorFromMark(nt.MarkID)
		if err := m.runTC(buildTierClassArgs(m.iface, cid, nt.PoolMbps)); err != nil {
			return fmt.Errorf("change class %s: %w", name, err)
		}
		m.state[name] = nt
	}

	return nil
}

// addTier 为新 tier 下 tc class + leaf qdisc + filter + iptables mark。
func (m *Manager) addTier(name string, t TierState) error {
	cid := ClassIDMinorFromMark(t.MarkID)
	if err := m.runTC(buildTierClassArgs(m.iface, cid, t.PoolMbps)); err != nil {
		return fmt.Errorf("add class %s: %w", name, err)
	}
	if err := m.runTC(buildTierLeafQdiscArgs(m.iface, cid)); err != nil {
		return fmt.Errorf("add leaf qdisc %s: %w", name, err)
	}
	if err := m.runTC(buildTierFilterArgs(m.iface, t.MarkID, cid)); err != nil {
		return fmt.Errorf("add filter %s: %w", name, err)
	}
	// iptables：幂等地下 mark 规则。先清掉所有旧副本（可能 agent 重启前遗留），再追加一次。
	if err := m.ensureIptablesMark(t.PortRange, t.MarkID); err != nil {
		return fmt.Errorf("add iptables mark %s: %w", name, err)
	}
	return nil
}

// removeTier 拆除 tier 的 filter/class/iptables 规则（容错，失败不阻塞）。
func (m *Manager) removeTier(name string, t TierState) {
	cid := ClassIDMinorFromMark(t.MarkID)
	// 先删 filter，再删 class
	_ = m.runTC([]string{"filter", "del", "dev", m.iface, "protocol", "ip", "parent", "1:", "prio", "1", "handle", fmt.Sprintf("%d", t.MarkID), "fw"})
	_ = m.runTC([]string{"class", "del", "dev", m.iface, "classid", fmt.Sprintf("1:%d", cid)})
	// 再反复 -D 直到 iptables 里没有该规则
	m.deleteIptablesMark(t.PortRange, t.MarkID)
	_ = name // 仅供日志/调试
}

// ensureIptablesMark 确保恰好有一条 iptables mark 规则：先全部删除同 (sport, mark) 的规则，再追加一次。
// 这样处理 agent 重启后残留、或手动修改过的状态，最终达到"exactly one"。
func (m *Manager) ensureIptablesMark(portRange string, mark int) error {
	m.deleteIptablesMark(portRange, mark)
	return m.exec("iptables", buildIptablesAddMarkArgs(portRange, mark)...)
}

// deleteIptablesMark 循环 -D 直到 -C 返回非零（不再匹配）。
func (m *Manager) deleteIptablesMark(portRange string, mark int) {
	for i := 0; i < 64; i++ { // 上限 64 次防死循环（正常只有 0 或 1 条）
		if err := m.exec("iptables", buildIptablesCheckMarkArgs(portRange, mark)...); err != nil {
			return
		}
		if err := m.exec("iptables", buildIptablesDelMarkArgs(portRange, mark)...); err != nil {
			return
		}
	}
}

// Disable 拆除所有 tc + iptables 规则（tc qdisc del 级联 class/filter；iptables 逐条 -D）。
func (m *Manager) Disable() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for name, t := range m.state {
		m.deleteIptablesMark(t.PortRange, t.MarkID)
		_ = name
	}

	err := m.runTC(buildTeardownArgs(m.iface))
	if err != nil && !isNoSuchFileErr(err) {
		return err
	}
	m.rootInstalled = false
	m.state = make(map[string]TierState)
	return nil
}

// runTC 调用注入的 exec，第一个参数固定为 "tc"。
func (m *Manager) runTC(args []string) error {
	return m.exec("tc", args...)
}

func isNoSuchFileErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "No such file")
}
