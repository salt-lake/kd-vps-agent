package command

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

const serviceFilePath = "/etc/systemd/system/node-agent.service"
const agentEnvFilePath = "/etc/node-agent.env"

// serviceKeyToEnvKey 将 service 文件中的环境变量名映射到 /etc/node-agent.env 中的 key 名。
// 未在此 map 中的 key（如 SENTRY_DSN）不写入 env 文件。
var serviceKeyToEnvKey = map[string]string{
	"NATS_URL":        "NATS_URL",
	"NATS_AUTH_TOKEN": "NATS_TOKEN",
	"API_BASE":        "API_BASE",
	"SCRIPT_TOKEN":    "TOKEN",
	"NODE_ID":         "NODE_ID",
	"REPORT_INTERVAL": "INTERVAL",
}

// allowedEnvKeys 限制可远程更新的环境变量，防止误操作改动节点协议等固定配置。
var allowedEnvKeys = map[string]bool{
	"NATS_URL":        true,
	"NATS_AUTH_TOKEN": true,
	"API_BASE":        true,
	"SCRIPT_TOKEN":    true,
	"NODE_ID":         true,
	"REPORT_INTERVAL": true,
	"SENTRY_DSN":      true,
}

type updateConfigReq struct {
	Env map[string]string `json:"env"`
}

// UpdateConfigHandler 处理 update_config 指令：更新 systemd service 中的环境变量并重启 agent。
type UpdateConfigHandler struct{}

func (UpdateConfigHandler) Name() string { return "update_config" }

func (h UpdateConfigHandler) Handle(data []byte) ([]byte, error) {
	var req updateConfigReq
	if err := json.Unmarshal(data, &req); err != nil {
		return errResp("invalid payload: " + err.Error()), nil
	}
	if len(req.Env) == 0 {
		return errResp("env is required"), nil
	}

	for k, v := range req.Env {
		if !allowedEnvKeys[k] {
			return errResp(fmt.Sprintf("key %q is not allowed", k)), nil
		}
		if strings.ContainsAny(v, "\n\r\"") {
			return errResp(fmt.Sprintf("value for %q contains invalid characters", k)), nil
		}
	}

	if err := updateServiceEnv(req.Env); err != nil {
		log.Printf("update_config: update service file err=%v", err)
		return errResp("update service file failed: " + err.Error()), nil
	}
	if err := syncAgentEnvFile(req.Env); err != nil {
		log.Printf("update_config: sync env file err=%v (non-fatal)", err)
	}

	log.Printf("update_config: updated keys=%v, scheduling restart", keys(req.Env))
	go func() {
		// 短暂延迟，让响应先发回后端
		time.Sleep(500 * time.Millisecond)
		reloadCtx, reloadCancel := context.WithTimeout(context.Background(), 15*time.Second)
		out, err := exec.CommandContext(reloadCtx, "systemctl", "daemon-reload").CombinedOutput()
		reloadCancel()
		if err != nil {
			log.Printf("update_config: daemon-reload failed: %v, output: %s", err, out)
			return
		}
		restartCtx, restartCancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err = exec.CommandContext(restartCtx, "systemctl", "restart", "node-agent").CombinedOutput()
		restartCancel()
		if err != nil {
			log.Printf("update_config: restart failed: %v, output: %s", err, out)
		}
	}()

	return okResp("config updated, restarting"), nil
}

// updateServiceEnv 原子替换 service 文件中匹配的 Environment= 行。
// 只更新已存在的 key；不存在于文件中的 key 会返回错误。
func updateServiceEnv(env map[string]string) error {
	data, err := os.ReadFile(serviceFilePath)
	if err != nil {
		return fmt.Errorf("read service file: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	updated := make(map[string]bool, len(env))
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "Environment=") {
			continue
		}
		rest := strings.TrimPrefix(trimmed, "Environment=")
		eqIdx := strings.Index(rest, "=")
		if eqIdx < 0 {
			continue
		}
		key := rest[:eqIdx]
		if newVal, ok := env[key]; ok {
			lines[i] = "Environment=" + key + "=" + newVal
			updated[key] = true
		}
	}

	for k := range env {
		if !updated[k] {
			return fmt.Errorf("key %q not found in service file", k)
		}
	}

	tmp := serviceFilePath + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")), 0644); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := os.Rename(tmp, serviceFilePath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename service file: %w", err)
	}
	return nil
}

// syncAgentEnvFile 将本次更新的 key 同步回 /etc/node-agent.env，
// 避免节点重新 bootstrap 时 agent_install.sh 用旧值覆盖 update_config 的改动。
// 文件中不存在的 key 不追加（bootstrap 没写的字段保持不变）。
func syncAgentEnvFile(env map[string]string) error {
	data, err := os.ReadFile(agentEnvFilePath)
	if err != nil {
		return fmt.Errorf("read env file: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		eqIdx := strings.Index(line, "=")
		if eqIdx < 0 {
			continue
		}
		fileKey := line[:eqIdx]
		for svcKey, envKey := range serviceKeyToEnvKey {
			if fileKey == envKey {
				if newVal, ok := env[svcKey]; ok {
					lines[i] = envKey + "=" + newVal
				}
				break
			}
		}
	}

	tmp := agentEnvFilePath + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")), 0600); err != nil {
		return fmt.Errorf("write temp env file: %w", err)
	}
	if err := os.Rename(tmp, agentEnvFilePath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename env file: %w", err)
	}
	return nil
}

func keys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
