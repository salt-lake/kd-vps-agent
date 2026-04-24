//go:build xray

package command

import (
	"log"
)

// TierMigrator 定义 command 包对迁移能力的依赖，由 xray 包实现。
type TierMigrator interface {
	// MigrateToTiers 执行一次性迁移：读当前 config、备份、按 payload 重写、restart xray、重注入用户、应用 tc 规则。
	// payload 是原始 JSON，具体解析由 migrator 内实现。
	MigrateToTiers(payload []byte) error
}

// XrayMigrateTierHandler 处理 xray_migrate_tier 指令。
type XrayMigrateTierHandler struct {
	migrator TierMigrator
}

func NewXrayMigrateTierHandler(m TierMigrator) XrayMigrateTierHandler {
	return XrayMigrateTierHandler{migrator: m}
}

func (XrayMigrateTierHandler) Name() string { return "xray_migrate_tier" }

func (h XrayMigrateTierHandler) Handle(data []byte) ([]byte, error) {
	if h.migrator == nil {
		return errResp("tier migrator not available"), nil
	}
	// 基本形态校验：payload 必须是 JSON object
	if len(data) == 0 || data[0] != '{' {
		return errResp("invalid payload: not an object"), nil
	}
	if err := h.migrator.MigrateToTiers(data); err != nil {
		log.Printf("xray_migrate_tier: err=%v", err)
		return errResp(err.Error()), nil
	}
	return okResp("ok"), nil
}
