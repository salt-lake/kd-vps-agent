//go:build xray

package ratelimit

import (
	"fmt"
	"strings"
	"testing"
)

// recordingExec 记录所有 exec 调用的参数，用于断言。
type recordingExec struct {
	calls [][]string
	err   error // 设置后对所有调用返回错误
}

func (r *recordingExec) run(cmd string, args ...string) error {
	combined := append([]string{cmd}, args...)
	r.calls = append(r.calls, combined)
	return r.err
}

func TestManager_ApplyFirstTime(t *testing.T) {
	rec := &recordingExec{}
	m := NewManager("eth0", rec.run)

	tiers := map[string]TierConfig{
		"vip":  {MarkID: 1, PoolMbps: 100},
		"svip": {MarkID: 2, PoolMbps: 500},
	}
	if err := m.Apply(tiers); err != nil {
		t.Fatalf("Apply err: %v", err)
	}

	// 首次 Apply 应包含 root qdisc + 每 tier 3 条（class / fq_codel / filter）
	// 总 tc 调用 = 1 + 2*3 = 7
	tcCalls := 0
	for _, c := range rec.calls {
		if c[0] == "tc" {
			tcCalls++
		}
	}
	if tcCalls != 7 {
		t.Errorf("expected 7 tc calls, got %d (calls=%v)", tcCalls, rec.calls)
	}
}

func TestManager_ApplyIdempotent(t *testing.T) {
	rec := &recordingExec{}
	m := NewManager("eth0", rec.run)

	tiers := map[string]TierConfig{
		"vip": {MarkID: 1, PoolMbps: 100},
	}
	if err := m.Apply(tiers); err != nil {
		t.Fatal(err)
	}
	firstCount := len(rec.calls)

	// 同样配置再 Apply 一次，不应有新命令
	if err := m.Apply(tiers); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != firstCount {
		t.Errorf("second Apply should be no-op; calls grew from %d to %d", firstCount, len(rec.calls))
	}
}

func TestManager_ApplyPoolChange(t *testing.T) {
	rec := &recordingExec{}
	m := NewManager("eth0", rec.run)

	tiers1 := map[string]TierConfig{"vip": {MarkID: 1, PoolMbps: 100}}
	_ = m.Apply(tiers1)

	baseline := len(rec.calls)
	tiers2 := map[string]TierConfig{"vip": {MarkID: 1, PoolMbps: 200}}
	if err := m.Apply(tiers2); err != nil {
		t.Fatal(err)
	}

	// 改 pool 只应执行 class replace 一条命令
	newCalls := rec.calls[baseline:]
	if len(newCalls) != 1 {
		t.Errorf("pool change should emit 1 cmd, got %d: %v", len(newCalls), newCalls)
	}
	argStr := strings.Join(newCalls[0], " ")
	if !strings.Contains(argStr, "class") || !strings.Contains(argStr, "200mbit") {
		t.Errorf("expected class change to 200mbit, got %q", argStr)
	}
}

func TestManager_ApplyExecError(t *testing.T) {
	rec := &recordingExec{err: fmt.Errorf("mock err")}
	m := NewManager("eth0", rec.run)

	tiers := map[string]TierConfig{"vip": {MarkID: 1, PoolMbps: 100}}
	if err := m.Apply(tiers); err == nil {
		t.Error("expected error, got nil")
	}
}

func TestManager_Disable(t *testing.T) {
	rec := &recordingExec{}
	m := NewManager("eth0", rec.run)

	tiers := map[string]TierConfig{"vip": {MarkID: 1, PoolMbps: 100}}
	_ = m.Apply(tiers)
	baseline := len(rec.calls)

	if err := m.Disable(); err != nil {
		t.Fatal(err)
	}
	calls := rec.calls[baseline:]
	if len(calls) != 1 || calls[0][0] != "tc" || !strings.Contains(strings.Join(calls[0], " "), "qdisc del") {
		t.Errorf("expected single 'tc qdisc del' call, got %v", calls)
	}

	// Disable 后再 Apply 应该重新下发所有规则
	if err := m.Apply(tiers); err != nil {
		t.Fatal(err)
	}
	afterReApply := len(rec.calls) - baseline - 1
	if afterReApply != 4 { // 1 root qdisc + 3 tier
		t.Errorf("re-apply after Disable should emit 4 cmds, got %d", afterReApply)
	}
}
