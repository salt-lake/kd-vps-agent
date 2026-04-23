//go:build xray

package xray

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/salt-lake/kd-vps-agent/ratelimit"
)

// migrateTierPayload 对应 xray_migrate_tier 指令的 data 部分。
type migrateTierPayload struct {
	Tiers           map[string]migrateTierEntry `json:"tiers"`
	DefaultTier     string                      `json:"defaultTier"`
	MigrateExisting bool                        `json:"migrateExisting"`
}

type migrateTierEntry struct {
	MarkID     int    `json:"markId"`
	InboundTag string `json:"inboundTag"`
	PortRange  string `json:"portRange"`
	PoolMbps   int    `json:"poolMbps"`
}

// MigrateToTiers 是对 doMigrate 的 wrapper：无论成功失败都通过 reporter 回报。
// 返回值是 doMigrate 的原始错误（便于 handler 把 error message 发回 NATS reply）。
func (s *XrayUserSync) MigrateToTiers(raw []byte) error {
	err := s.doMigrate(raw)

	s.mu.Lock()
	reporter := s.reporter
	s.mu.Unlock()
	if reporter != nil {
		if err != nil {
			reporter(false, err.Error())
		} else {
			reporter(true, "")
		}
	}
	return err
}

// doMigrate 执行一次性结构性迁移的核心逻辑。
// 1. 解析 payload
// 2. 若 config 已双 inbound（幂等）→ 仅刷新缓存 + tc
// 3. 否则：备份 config → 拉用户 → 写新 config → 开防火墙 → restart xray → 重注入 → apply tc
func (s *XrayUserSync) doMigrate(raw []byte) error {
	var p migrateTierPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("parse migrate payload: %w", err)
	}
	if len(p.Tiers) == 0 {
		return fmt.Errorf("migrate payload: tiers is empty")
	}
	if _, ok := p.Tiers[p.DefaultTier]; !ok {
		return fmt.Errorf("migrate payload: defaultTier %q not in tiers", p.DefaultTier)
	}

	// 1. 幂等检测
	if s.configAlreadyMultiInbound(p) {
		log.Printf("xray_migrate: config already multi-inbound, only refreshing tiers + tc")
		s.cacheTiers(p)
		return s.applyTC(p)
	}

	// 2. 备份 config
	orig, err := os.ReadFile(s.configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	backupPath := fmt.Sprintf("%s.bak.%d", s.configPath, time.Now().Unix())
	if err := os.WriteFile(backupPath, orig, 0644); err != nil {
		return fmt.Errorf("backup config: %w", err)
	}
	log.Printf("xray_migrate: config backup -> %s", backupPath)

	// 3. 拉全量用户
	users, err := s.fetchUsers()
	if err != nil {
		return fmt.Errorf("fetch users: %w", err)
	}

	// 4. 更新 tiers + defaultTier 缓存
	s.cacheTiers(p)

	// 5. 生成并写入新 config
	if err := s.writeMultiInboundConfig(orig, p, users); err != nil {
		// 失败：尝试恢复备份
		_ = os.WriteFile(s.configPath, orig, 0644)
		return fmt.Errorf("write multi-inbound config: %w", err)
	}

	// 6. 开防火墙（对每个 tier 的 portRange 加 iptables ACCEPT）
	for _, t := range p.Tiers {
		if err := openFirewallPort(t.PortRange); err != nil {
			log.Printf("xray_migrate: open firewall %s err=%v (continuing)", t.PortRange, err)
		}
	}

	// 7. 重启 xray（systemd 服务，与现有代码一致）
	if err := restartXrayService(); err != nil {
		return fmt.Errorf("restart xray: %w", err)
	}

	// 8. 等 xray 就绪并重注入
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s.syncAfterRestart(ctx)

	// 9. 应用 tc 规则
	if err := s.applyTC(p); err != nil {
		return fmt.Errorf("apply tc: %w", err)
	}

	log.Printf("xray_migrate: migration done, tiers=%d", len(p.Tiers))
	return nil
}

// cacheTiers 更新 s.tiers 和 s.defaultTier。
func (s *XrayUserSync) cacheTiers(p migrateTierPayload) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tiers = make(map[string]TierConfig, len(p.Tiers))
	for name, t := range p.Tiers {
		s.tiers[name] = TierConfig{MarkID: t.MarkID, InboundTag: t.InboundTag, PoolMbps: t.PoolMbps}
	}
	s.defaultTier = p.DefaultTier
}

// configAlreadyMultiInbound 检查 config 是否已经包含所有 tier 的 inbound tag。
func (s *XrayUserSync) configAlreadyMultiInbound(p migrateTierPayload) bool {
	data, err := os.ReadFile(s.configPath)
	if err != nil {
		return false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	var inbounds []map[string]json.RawMessage
	if err := json.Unmarshal(raw["inbounds"], &inbounds); err != nil {
		return false
	}
	existingTags := map[string]bool{}
	for _, ib := range inbounds {
		var tag string
		_ = json.Unmarshal(ib["tag"], &tag)
		existingTags[tag] = true
	}
	for _, t := range p.Tiers {
		if !existingTags[t.InboundTag] {
			return false
		}
	}
	return true
}

// applyTC 把 payload 里的 tiers 转成 ratelimit.TierConfig 下发给 ratelimit manager。
// PortRange 透传给 ratelimit 用于 iptables 源端口匹配。
func (s *XrayUserSync) applyTC(p migrateTierPayload) error {
	s.mu.Lock()
	rl := s.ratelimit
	s.mu.Unlock()
	if rl == nil {
		log.Printf("xray_migrate: ratelimit manager not set, skip tc apply")
		return nil
	}
	tiers := make(map[string]ratelimit.TierConfig, len(p.Tiers))
	for name, t := range p.Tiers {
		tiers[name] = ratelimit.TierConfig{MarkID: t.MarkID, PoolMbps: t.PoolMbps, PortRange: t.PortRange}
	}
	return rl.Apply(tiers)
}

// writeMultiInboundConfig 基于原 config，按 payload 里的 tiers 生成多 inbound 结构：
// - 为每个 tier 生成一个 inbound（复用原 streamSettings，仅 tag + port + clients 不同）
// - outbounds 和 routing 保持原样不动（打标改由 iptables 完成，xray 侧无需感知 tier）
// - clients 按 user.tier（缺失则 defaultTier）分组写入对应 inbound
func (s *XrayUserSync) writeMultiInboundConfig(orig []byte, p migrateTierPayload, users []userDTO) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(orig, &raw); err != nil {
		return fmt.Errorf("parse orig config: %w", err)
	}

	var origInbounds []map[string]json.RawMessage
	if err := json.Unmarshal(raw["inbounds"], &origInbounds); err != nil {
		return fmt.Errorf("parse inbounds: %w", err)
	}
	if len(origInbounds) == 0 {
		return fmt.Errorf("no inbound in config to use as template")
	}
	template := origInbounds[0]

	// 按 inbound tag 分组 clients（defaultUUID 写入每个 tier inbound）
	byTag := map[string][]map[string]string{}
	defaultClient := map[string]string{"id": defaultUUID, "email": "default@test", "flow": "xtls-rprx-vision"}
	for _, t := range p.Tiers {
		byTag[t.InboundTag] = []map[string]string{defaultClient}
	}
	for _, u := range users {
		if u.UUID == defaultUUID {
			continue
		}
		tier := u.Tier
		if _, ok := p.Tiers[tier]; !ok {
			tier = p.DefaultTier
		}
		tag := p.Tiers[tier].InboundTag
		byTag[tag] = append(byTag[tag], map[string]string{
			"id": u.UUID, "email": emailFromUUID(u.UUID), "flow": "xtls-rprx-vision",
		})
	}

	// 生成新 inbounds 数组（每 tier 一个，基于第一个原 inbound 作模板）
	newInbounds := []map[string]json.RawMessage{}
	for tierName, t := range p.Tiers {
		ib := map[string]json.RawMessage{}
		for k, v := range template {
			ib[k] = v
		}
		tagJSON, _ := json.Marshal(t.InboundTag)
		portJSON, _ := json.Marshal(t.PortRange)
		ib["tag"] = tagJSON
		ib["port"] = portJSON

		var settings map[string]json.RawMessage
		if ib["settings"] != nil {
			_ = json.Unmarshal(ib["settings"], &settings)
		} else {
			settings = map[string]json.RawMessage{}
		}
		clientsJSON, _ := json.Marshal(byTag[t.InboundTag])
		settings["clients"] = clientsJSON
		if _, ok := settings["decryption"]; !ok {
			settings["decryption"] = json.RawMessage(`"none"`)
		}
		settingsJSON, _ := json.Marshal(settings)
		ib["settings"] = settingsJSON

		newInbounds = append(newInbounds, ib)
		_ = tierName
	}
	newInboundsJSON, err := json.Marshal(newInbounds)
	if err != nil {
		return fmt.Errorf("marshal new inbounds: %w", err)
	}
	raw["inbounds"] = newInboundsJSON

	// outbounds 和 routing 不修改：限速通过 iptables 按源端口打标，xray 内部不需要 tier 感知
	// （保留原 `direct` / `blocked` 兜底 outbound，routing.rules 保持为空）

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return os.WriteFile(s.configPath, out, 0644)
}

// openFirewallPort 对一个 port range（如 "45000-45003" 或单端口 "443"）加 iptables ACCEPT 规则。
// 幂等：先 -C 检查已存在则跳过，否则 -I 插入。
func openFirewallPort(portRange string) error {
	pr := strings.Replace(portRange, "-", ":", 1) // iptables multiport 用 `:` 分隔
	if err := exec.Command("iptables", "-C", "INPUT", "-p", "tcp", "--dport", pr, "-j", "ACCEPT").Run(); err == nil {
		return nil // 已存在
	}
	return exec.Command("iptables", "-I", "INPUT", "-p", "tcp", "--dport", pr, "-j", "ACCEPT").Run()
}

// restartXrayService 重启 xray（systemd 服务），与 schedule.go 里的重启方式一致。
func restartXrayService() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "restart", "xray").CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl restart xray: %w, output: %s", err, out)
	}
	return nil
}

