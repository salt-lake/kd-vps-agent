//go:build xray

package ratelimit

import (
	"fmt"
	"strings"
	"sync"
)

// TierConfig 传给 Apply 的输入。
type TierConfig struct {
	MarkID   int
	PoolMbps int
}

// ExecFunc 执行单条命令的函数类型，便于测试时注入 mock。
// cmd 通常是 "tc"，args 是后续参数。
type ExecFunc func(cmd string, args ...string) error

// Manager tc 规则管理器。
type Manager struct {
	iface string
	exec  ExecFunc

	mu            sync.Mutex
	rootInstalled bool
	state         map[string]TierState
}

// NewManager 构造。iface 为目标网卡名，exec 为执行器（生产用 exec.Command 封装，测试用 mock）。
func NewManager(iface string, exec ExecFunc) *Manager {
	return &Manager{
		iface: iface,
		exec:  exec,
		state: make(map[string]TierState),
	}
}

// Apply 幂等下发 tc 规则。
// 1) 首次调用：先下 root qdisc，再遍历每 tier 下 class/qdisc/filter。
// 2) 后续调用：diff 状态，只下变化的命令。
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
		newState[name] = TierState{MarkID: t.MarkID, PoolMbps: t.PoolMbps}
	}

	add, change, remove := DiffState(m.state, newState)

	for _, name := range remove {
		ot := m.state[name]
		cid := ClassIDMinorFromMark(ot.MarkID)
		// 先删 filter，再删 class（顺序避免依赖错误）
		_ = m.runTC([]string{"filter", "del", "dev", m.iface, "protocol", "ip", "parent", "1:", "prio", "1", "handle", fmt.Sprintf("%d", ot.MarkID), "fw"})
		_ = m.runTC([]string{"class", "del", "dev", m.iface, "classid", fmt.Sprintf("1:%d", cid)})
		delete(m.state, name)
	}

	for _, name := range add {
		nt := newState[name]
		cid := ClassIDMinorFromMark(nt.MarkID)
		if err := m.runTC(buildTierClassArgs(m.iface, cid, nt.PoolMbps)); err != nil {
			return fmt.Errorf("add class %s: %w", name, err)
		}
		if err := m.runTC(buildTierLeafQdiscArgs(m.iface, cid)); err != nil {
			return fmt.Errorf("add leaf qdisc %s: %w", name, err)
		}
		if err := m.runTC(buildTierFilterArgs(m.iface, nt.MarkID, cid)); err != nil {
			return fmt.Errorf("add filter %s: %w", name, err)
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

// Disable 拆除所有 tc 规则（qdisc del root 会级联清理 class/filter）。
func (m *Manager) Disable() error {
	m.mu.Lock()
	defer m.mu.Unlock()

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
