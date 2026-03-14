package command

import (
	"log"

	"github.com/salt-lake/kd-vps-agent/update"
)

// SelfUpdateHandler 处理 agent:self_update 指令，主动触发自更新
type SelfUpdateHandler struct {
	CurrentVersion string
}

func (h SelfUpdateHandler) Name() string { return "agent:self_update" }

func (h SelfUpdateHandler) Handle(_ []byte) ([]byte, error) {
	log.Println("self_update: triggered via command")
	if err := update.TryUpdate(h.CurrentVersion); err != nil {
		log.Printf("self_update: failed: %v", err)
		return errResp(err.Error()), nil
	}
	return okResp("update triggered, restarting"), nil
}
