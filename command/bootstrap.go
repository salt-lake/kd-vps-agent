package command

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
)

type bootstrapReq struct {
	NodeID     int    `json:"node_id"`
	ScriptName string `json:"script_name"`
}

type BootstrapHandler struct{}

func (BootstrapHandler) Name() string { return "bootstrap" }

func (BootstrapHandler) Handle(data []byte) ([]byte, error) {
	var req bootstrapReq
	if err := json.Unmarshal(data, &req); err != nil {
		return errResp("invalid payload: " + err.Error()), nil
	}
	if req.NodeID == 0 || req.ScriptName == "" {
		return errResp("node_id and script_name are required"), nil
	}

	apiBase := strings.TrimRight(os.Getenv("API_BASE"), "/")
	token := os.Getenv("SCRIPT_TOKEN")
	if token == "" {
		token = os.Getenv("NATS_AUTH_TOKEN")
	}
	natsURL := os.Getenv("NATS_URL")
	natsToken := os.Getenv("NATS_AUTH_TOKEN")
	protocol := os.Getenv("NODE_PROTOCOL")
	if protocol == "" {
		protocol = "ikev2"
	}

	if apiBase == "" || token == "" {
		return errResp("API_BASE or token not configured"), nil
	}

	cmd := fmt.Sprintf(
		`nohup bash -c 'curl -fsSL -H "Authorization: Bearer %s" -H "X-Node-ID: %d" "%s/api/scripts/bootstrap" | bash -s -- --node-id %d --token %s --script-name %s --api-base %s --nats-url %s --nats-token %s --protocol %s' > /tmp/bootstrap_%d.log 2>&1 &`,
		token, req.NodeID, apiBase, req.NodeID, token, req.ScriptName, apiBase, natsURL, natsToken, protocol, req.NodeID,
	)

	if err := exec.Command("bash", "-c", cmd).Start(); err != nil {
		log.Printf("bootstrap: node_id=%d err=%v", req.NodeID, err)
		return errResp(fmt.Sprintf("start bootstrap failed: %v", err)), nil
	}
	return okResp("ok"), nil
}
