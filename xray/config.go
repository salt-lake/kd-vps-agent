//go:build xray

package xray

import (
	"encoding/json"
	"fmt"
	"os"
)

func emailFromUUID(uuid string) string {
	return fmt.Sprintf("xray@%s", uuid)
}

// writeConfig 读取 configPath，按 tier 分组用户写入对应 inbound 的 clients。
// 兼容模式（s.tiers 为空）下退化为单 inbound 老逻辑。
func (s *XrayUserSync) writeConfig(users []userDTO) error {
	data, err := os.ReadFile(s.configPath)
	if err != nil {
		return fmt.Errorf("read config %s: %w", s.configPath, err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	inboundsRaw, ok := raw["inbounds"]
	if !ok {
		return fmt.Errorf("config has no inbounds")
	}

	var inbounds []map[string]json.RawMessage
	if err := json.Unmarshal(inboundsRaw, &inbounds); err != nil {
		return fmt.Errorf("parse inbounds: %w", err)
	}

	// 快照 tiers / defaultTier / inboundTag
	s.mu.Lock()
	tiersCopy := make(map[string]TierConfig, len(s.tiers))
	for k, v := range s.tiers {
		tiersCopy[k] = v
	}
	defaultTier := s.defaultTier
	singleInboundMode := len(tiersCopy) == 0
	fallbackTag := s.inboundTag
	s.mu.Unlock()

	// 按 inbound tag 分组 clients；defaultUUID 加到每个 tier inbound
	byTag := map[string][]map[string]string{}
	defaultClient := map[string]string{"id": defaultUUID, "email": "default@test", "flow": "xtls-rprx-vision"}
	if singleInboundMode {
		byTag[fallbackTag] = []map[string]string{defaultClient}
	} else {
		for _, t := range tiersCopy {
			byTag[t.InboundTag] = []map[string]string{defaultClient}
		}
	}

	for _, u := range users {
		if u.UUID == defaultUUID {
			continue
		}
		var tag string
		if singleInboundMode {
			tag = fallbackTag
		} else {
			tier := u.Tier
			if _, ok := tiersCopy[tier]; !ok {
				tier = defaultTier
			}
			t, ok := tiersCopy[tier]
			if !ok {
				continue // tier 找不到则跳过此用户
			}
			tag = t.InboundTag
		}
		byTag[tag] = append(byTag[tag], map[string]string{
			"id": u.UUID, "email": emailFromUUID(u.UUID), "flow": "xtls-rprx-vision",
		})
	}

	// 写回每个匹配 tag 的 inbound
	for i, inbound := range inbounds {
		var tag string
		if t, ok := inbound["tag"]; ok {
			if err := json.Unmarshal(t, &tag); err != nil {
				return fmt.Errorf("parse inbound tag: %w", err)
			}
		}
		clients, matched := byTag[tag]
		if !matched {
			continue
		}

		var settings map[string]json.RawMessage
		if inbound["settings"] != nil {
			if err := json.Unmarshal(inbound["settings"], &settings); err != nil {
				return fmt.Errorf("parse inbound settings: %w", err)
			}
		} else {
			settings = map[string]json.RawMessage{}
		}
		clientsJSON, err := json.Marshal(clients)
		if err != nil {
			return fmt.Errorf("marshal clients: %w", err)
		}
		settings["clients"] = clientsJSON
		settingsJSON, err := json.Marshal(settings)
		if err != nil {
			return fmt.Errorf("marshal inbound settings: %w", err)
		}
		inbounds[i]["settings"] = settingsJSON
	}

	newInboundsJSON, err := json.Marshal(inbounds)
	if err != nil {
		return fmt.Errorf("marshal inbounds: %w", err)
	}
	raw["inbounds"] = newInboundsJSON

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return os.WriteFile(s.configPath, out, 0644)
}
