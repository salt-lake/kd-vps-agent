//go:build xray

package sync

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

var destCandidates = []string{
	"www.apple.com:443",
	"www.microsoft.com:443",
	"www.cloudflare.com:443",
	"www.amazon.com:443",
}

func emailFromUUID(uuid string) string {
	return fmt.Sprintf("xray@%s", uuid)
}

// tcpReachable 探测 hostport 是否 TCP 可达（3 秒超时）。
func tcpReachable(hostport string) bool {
	conn, err := net.DialTimeout("tcp", hostport, 3*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// CheckDest 检测当前 Reality dest 是否可达；不可达时从候选列表选新 dest 并更新配置重启 xray。
func (s *XrayUserSync) CheckDest() error {
	data, err := os.ReadFile(s.configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	var inbounds []map[string]json.RawMessage
	if err := json.Unmarshal(raw["inbounds"], &inbounds); err != nil {
		return fmt.Errorf("parse inbounds: %w", err)
	}

	idx := -1
	var currentDest string
	for i, inbound := range inbounds {
		var tag string
		if t, ok := inbound["tag"]; ok {
			_ = json.Unmarshal(t, &tag)
		}
		if tag != s.inboundTag {
			continue
		}
		idx = i
		var ss map[string]json.RawMessage
		if inbound["streamSettings"] != nil {
			if err := json.Unmarshal(inbound["streamSettings"], &ss); err != nil {
				break
			}
		}
		var rs map[string]json.RawMessage
		if ss["realitySettings"] != nil {
			if err := json.Unmarshal(ss["realitySettings"], &rs); err != nil {
				break
			}
		}
		if rs["dest"] != nil {
			_ = json.Unmarshal(rs["dest"], &currentDest)
		}
		break
	}

	if idx == -1 || currentDest == "" {
		return fmt.Errorf("proxy inbound or dest not found in config")
	}

	if tcpReachable(currentDest) {
		return nil
	}

	log.Printf("xray_sync: dest=%s unreachable, searching candidates", currentDest)

	newDest := ""
	for _, c := range destCandidates {
		if c == currentDest {
			continue
		}
		if tcpReachable(c) {
			newDest = c
			break
		}
	}
	if newDest == "" {
		return fmt.Errorf("no reachable dest found in candidates")
	}

	serverName := strings.SplitN(newDest, ":", 2)[0]

	var ss map[string]json.RawMessage
	if err := json.Unmarshal(inbounds[idx]["streamSettings"], &ss); err != nil {
		return fmt.Errorf("parse streamSettings: %w", err)
	}
	var rs map[string]json.RawMessage
	if err := json.Unmarshal(ss["realitySettings"], &rs); err != nil {
		return fmt.Errorf("parse realitySettings: %w", err)
	}

	destJSON, _ := json.Marshal(newDest)
	serverNamesJSON, _ := json.Marshal([]string{serverName})
	rs["dest"] = destJSON
	rs["serverNames"] = serverNamesJSON

	rsJSON, _ := json.Marshal(rs)
	ss["realitySettings"] = rsJSON
	ssJSON, _ := json.Marshal(ss)
	inbounds[idx]["streamSettings"] = ssJSON

	newInboundsJSON, _ := json.Marshal(inbounds)
	raw["inbounds"] = newInboundsJSON

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(s.configPath, out, 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	log.Printf("xray_sync: updated dest=%s serverName=%s, restarting container", newDest, serverName)
	if out, err := exec.Command("docker", "restart", s.container).CombinedOutput(); err != nil {
		return fmt.Errorf("docker restart: %v, output: %s", err, out)
	}
	return nil
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
