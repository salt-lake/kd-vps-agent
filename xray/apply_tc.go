//go:build xray

package xray

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/salt-lake/kd-vps-agent/ratelimit"
)

// maybeClearTiersIfUnmigrated 检测"后端已下发 tier 配置，但本地 xray config 还是单 inbound"
// 的预迁移状态。这种情况下 tier 路由会失败（"handler not found"），干脆把 s.tiers 清空
// 让 agent 退回兼容模式。xray_migrate_tier 指令落地后 cacheTiers 会重新填上。
func (s *XrayUserSync) maybeClearTiersIfUnmigrated() {
	s.mu.Lock()
	if len(s.tiers) == 0 {
		s.mu.Unlock()
		return
	}
	tiersCopy := make(map[string]TierConfig, len(s.tiers))
	for k, v := range s.tiers {
		tiersCopy[k] = v
	}
	s.mu.Unlock()

	ports, err := readTierPortsFromConfig(s.configPath, tiersCopy)
	if err != nil {
		// 读配置失败不动，让下游按既有缓存继续
		return
	}
	// 要求所有 tier 的 inboundTag 都在 xray config 里存在；否则视为未迁移
	for name := range tiersCopy {
		if _, ok := ports[name]; !ok {
			s.mu.Lock()
			s.tiers = map[string]TierConfig{}
			s.defaultTier = ""
			s.mu.Unlock()
			log.Printf("xray_sync: config not fully migrated to multi-inbound (tier=%s missing), running in compat mode until xray_migrate_tier indicator", name)
			return
		}
	}
}

// applyTCFromState 按当前 tiers 缓存 + 读 xray config 拿到每 tier 的 portRange 后，下发 ratelimit 规则。
// 供 agent 启动 / 节点重启后恢复 iptables + tc 状态；也在稳态同步到 tiers 变化（如 pool_mbps 调整）后调用。
// 兼容模式（s.tiers 为空）或 ratelimit 未注入时，静默跳过。
func (s *XrayUserSync) applyTCFromState() error {
	s.mu.Lock()
	rl := s.ratelimit
	tiersCopy := make(map[string]TierConfig, len(s.tiers))
	for k, v := range s.tiers {
		tiersCopy[k] = v
	}
	s.mu.Unlock()

	if rl == nil || len(tiersCopy) == 0 {
		return nil
	}

	ports, err := readTierPortsFromConfig(s.configPath, tiersCopy)
	if err != nil {
		return fmt.Errorf("read tier ports from config: %w", err)
	}

	rlTiers := make(map[string]ratelimit.TierConfig, len(tiersCopy))
	for name, t := range tiersCopy {
		portRange := ports[name]
		if portRange == "" {
			// 对应 inboundTag 在 config 里找不到 → 跳过这个 tier（log 一下，避免 ratelimit 下发空 sport）
			log.Printf("xray_sync: tier=%s inboundTag=%s not found in xray config, skip tc apply for this tier", name, t.InboundTag)
			continue
		}
		rlTiers[name] = ratelimit.TierConfig{
			MarkID:    t.MarkID,
			PoolMbps:  t.PoolMbps,
			PortRange: portRange,
		}
	}
	if len(rlTiers) == 0 {
		return nil
	}
	return rl.Apply(rlTiers)
}

// readTierPortsFromConfig 读取 xray config，按 inbound tag 匹配每个 tier 的监听端口。
// 返回 map[tierName]portRange（如 "34521-34524"），匹配不到的 tier 在 map 里不出现。
// xray 的 port 字段可能是 string（含范围）或 int，两种格式都兼容。
func readTierPortsFromConfig(configPath string, tiers map[string]TierConfig) (map[string]string, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	inboundsRaw, ok := raw["inbounds"]
	if !ok {
		return map[string]string{}, nil
	}
	var inbounds []map[string]json.RawMessage
	if err := json.Unmarshal(inboundsRaw, &inbounds); err != nil {
		return nil, fmt.Errorf("parse inbounds: %w", err)
	}

	// 反向索引 inboundTag → tierName，加速匹配
	tagToTier := make(map[string]string, len(tiers))
	for name, t := range tiers {
		tagToTier[t.InboundTag] = name
	}

	out := make(map[string]string, len(tiers))
	for _, ib := range inbounds {
		var tag string
		if rawTag, ok := ib["tag"]; ok {
			_ = json.Unmarshal(rawTag, &tag)
		}
		tierName, matched := tagToTier[tag]
		if !matched {
			continue
		}
		port, err := parsePortField(ib["port"])
		if err != nil {
			log.Printf("xray_sync: parse port for inbound %s: %v", tag, err)
			continue
		}
		if port != "" {
			out[tierName] = port
		}
	}
	return out, nil
}

// parsePortField 解析 xray inbound.port 字段：可能是 "34521-34524"、"443"（字符串），也可能是 443（整数）。
func parsePortField(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return strconv.Itoa(n), nil
	}
	return "", fmt.Errorf("port is neither string nor int: %s", string(raw))
}
