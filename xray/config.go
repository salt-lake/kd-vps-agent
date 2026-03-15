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

// writeConfig 读取 configPath，替换指定 inbound 的 clients 列表，写回文件。
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

	clients := []map[string]string{
		{"id": defaultUUID, "email": "default@test", "flow": "xtls-rprx-vision"},
	}
	for _, u := range users {
		if u.UUID == defaultUUID {
			continue
		}
		clients = append(clients, map[string]string{"id": u.UUID, "email": emailFromUUID(u.UUID), "flow": "xtls-rprx-vision"})
	}
	clientsJSON, _ := json.Marshal(clients)

	for i, inbound := range inbounds {
		var tag string
		if t, ok := inbound["tag"]; ok {
			_ = json.Unmarshal(t, &tag)
		}
		if tag != s.inboundTag {
			continue
		}

		var settings map[string]json.RawMessage
		if inbound["settings"] != nil {
			if err := json.Unmarshal(inbound["settings"], &settings); err != nil {
				return fmt.Errorf("parse inbound settings: %w", err)
			}
		} else {
			settings = make(map[string]json.RawMessage)
		}
		settings["clients"] = clientsJSON
		settingsJSON, _ := json.Marshal(settings)
		inbounds[i]["settings"] = settingsJSON
		break
	}

	newInboundsJSON, _ := json.Marshal(inbounds)
	raw["inbounds"] = newInboundsJSON

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return os.WriteFile(s.configPath, out, 0644)
}
