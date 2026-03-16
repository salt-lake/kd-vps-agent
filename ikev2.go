//go:build !xray

package main

import (
	"context"
	"log"
	"os"
	"os/exec"

	"github.com/salt-lake/kd-vps-agent/collect"
	"github.com/salt-lake/kd-vps-agent/command"
)

const assetName = "node-agent-ikev2"

func setupXray(_ context.Context, _ Config, _ *command.Dispatcher) {}

func buildProviders(cfg Config) []collect.MetricProvider {
	return []collect.MetricProvider{
		collect.NewSysProvider(),
		collect.NewTrafficProvider(cfg.Iface),
		collect.NewSwanProvider(cfg.SwanContainer),
	}
}

func startDailyJobs(ctx context.Context, cfg Config) {
	go dailyScheduler(ctx, 4, hostJitter(cfg.Host), func() {
		log.Println("daily clear charon log: start")
		clearCharonLog(cfg.SwanContainer)
	})
}

func clearCharonLog(container string) {
	if container == "" || container == "none" {
		if err := os.Truncate("/var/log/charon.log", 0); err != nil && !os.IsNotExist(err) {
			log.Printf("clearCharonLog: truncate err=%v", err)
		}
		return
	}
	out, err := exec.Command("docker", "exec", container,
		"sh", "-c", "test -f /var/log/charon.log && truncate -s 0 /var/log/charon.log || true",
	).CombinedOutput()
	if err != nil {
		log.Printf("clearCharonLog: container=%s err=%v output=%s", container, err, out)
	}
}
