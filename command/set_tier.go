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

const rateLimitScriptPath = "/kuando/scripts-xray/rate_limit.sh"

type setTierReq struct {
	SvipOnly bool `json:"svipOnly"`
}

// SetTierHandler 处理 set_tier 指令：切换节点限速档位（svip / vip）。
// 不重启 xray，只重写 /etc/node-agent.env 中的 NODE_SVIP_ONLY 并重跑 rate_limit.sh，
// 由后者 systemctl restart tc-per-ip.service 重新应用 tc 规则。
type SetTierHandler struct{}

func (SetTierHandler) Name() string { return "set_tier" }

func (h SetTierHandler) Handle(data []byte) ([]byte, error) {
	var req setTierReq
	if err := json.Unmarshal(data, &req); err != nil {
		return errResp("invalid payload: " + err.Error()), nil
	}

	envValue := "false"
	if req.SvipOnly {
		envValue = "true"
	}

	if err := upsertNodeAgentEnv("NODE_SVIP_ONLY", envValue); err != nil {
		log.Printf("set_tier: write env file err=%v", err)
		return errResp("write env file failed: " + err.Error()), nil
	}

	if _, err := os.Stat(rateLimitScriptPath); err != nil {
		log.Printf("set_tier: rate_limit.sh not found at %s: %v", rateLimitScriptPath, err)
		return errResp("rate_limit.sh not found: " + err.Error()), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", rateLimitScriptPath)
	cmd.Env = append(os.Environ(), "NODE_SVIP_ONLY="+envValue)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("set_tier: rate_limit.sh err=%v output=%s", err, out)
		return errResp(fmt.Sprintf("rate_limit.sh failed: %v, output: %s", err, out)), nil
	}

	tier := "vip"
	if req.SvipOnly {
		tier = "svip"
	}
	log.Printf("set_tier: applied tier=%s", tier)
	return okResp("tier set to " + tier), nil
}

// upsertNodeAgentEnv 替换或追加 /etc/node-agent.env 中 key=value 行；原子写。
// 与 update_config.go 中的 syncAgentEnvFile 不同：那个只更新已存在的 key，这里在缺失时会追加。
func upsertNodeAgentEnv(key, value string) error {
	data, err := os.ReadFile(agentEnvFilePath)
	if err != nil {
		return fmt.Errorf("read env file: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	prefix := key + "="
	found := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			lines[i] = key + "=" + value
			found = true
			break
		}
	}
	if !found {
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, key+"="+value, "")
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
