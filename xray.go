//go:build xray

package main

import (
	"context"
	"log"

	"github.com/salt-lake/kd-vps-agent/collect"
	"github.com/salt-lake/kd-vps-agent/command"
	"github.com/salt-lake/kd-vps-agent/xray"
)

func setupXray(ctx context.Context, cfg Config, d *command.Dispatcher) {
	if cfg.APIBase == "" || cfg.ScriptToken == "" {
		log.Println("xray sync disabled: API_BASE or SCRIPT_TOKEN not set")
		return
	}
	syncer := xray.NewXrayUserSync(
		cfg.APIBase, cfg.ScriptToken,
		cfg.XrayContainer, cfg.XrayAPIAddr,
		cfg.XrayInboundTag, cfg.XrayConfigPath,
	)
	syncer.Start(ctx)
	d.Register(command.NewXrayUserAddHandler(syncer))
	d.Register(command.NewXrayUserRemoveHandler(syncer))
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
		collect.NewXrayProvider(cfg.XrayContainer, cfg.XrayAPIAddr),
	}
}

func startDailyJobs(_ context.Context, _ Config) {}
