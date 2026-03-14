//go:build xray

package main

import (
	"context"
	"log"

	"github.com/salt-lake/kd-vps-agent/command"
	"github.com/salt-lake/kd-vps-agent/sync"
)

func setupXray(ctx context.Context, cfg Config, d *command.Dispatcher) func() {
	if cfg.APIBase == "" || cfg.ScriptToken == "" {
		log.Println("xray sync disabled: API_BASE or SCRIPT_TOKEN not set")
		return nil
	}
	syncer := sync.NewXrayUserSync(
		cfg.APIBase, cfg.ScriptToken,
		cfg.XrayContainer, cfg.XrayAPIAddr,
		cfg.XrayInboundTag, cfg.XrayConfigPath,
	)
	syncer.Start(ctx)
	d.Register(command.NewXrayUserAddHandler(syncer))
	d.Register(command.NewXrayUserRemoveHandler(syncer))
	return func() {
		if err := syncer.FullSync(); err != nil {
			log.Printf("xray full sync: %v", err)
		}
	}
}
