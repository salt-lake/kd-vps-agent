//go:build xray

package main

import (
	"context"
	"log"

	"github.com/salt-lake/kd-vps-agent/collect"
	"github.com/salt-lake/kd-vps-agent/command"
	"github.com/salt-lake/kd-vps-agent/xray"
)

const assetName = "node-agent-xray"

func setupXray(ctx context.Context, cfg Config, d *command.Dispatcher) {
	if cfg.APIBase == "" || cfg.ScriptToken == "" {
		log.Println("xray sync disabled: API_BASE or SCRIPT_TOKEN not set")
		return
	}
	syncer := xray.NewXrayUserSync(
		cfg.APIBase, cfg.ScriptToken,
		cfg.XrayAPIAddr, cfg.XrayInboundTag, cfg.XrayConfigPath,
	)
	tempSync := xray.NewTempUserSync(cfg.APIBase, cfg.ScriptToken, syncer)
	syncer.SetTempSync(tempSync)
	syncer.Start(ctx)
	tempSync.Start(ctx)
	d.Register(command.NewXrayUserAddHandler(syncer))
	d.Register(command.NewXrayUserRemoveHandler(syncer))
	d.Register(command.NewXrayUpdateHandler(ctx, syncer, cfg.XrayConfigPath))
	go dailyScheduler(ctx, 3, hostJitter(cfg.Host), func() {
		log.Println("daily full sync: start")
		if err := syncer.FullSync(); err != nil {
			log.Printf("xray full sync: %v", err)
		}
	})
}

func buildProviders(cfg Config) []collect.MetricProvider {
	return []collect.MetricProvider{
		collect.NewSysProvider(),
		collect.NewTrafficProvider(cfg.Iface),
		collect.NewXrayProvider(cfg.XrayAPIAddr),
	}
}

func startDailyJobs(_ context.Context, _ Config) {}
