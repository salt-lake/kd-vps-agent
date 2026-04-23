//go:build !xray

package main

import (
	"context"
	_ "embed"
	"log"
	"os/exec"

	"github.com/nats-io/nats.go"
	"github.com/salt-lake/kd-vps-agent/collect"
	"github.com/salt-lake/kd-vps-agent/command"
	"github.com/salt-lake/kd-vps-agent/update"
)

//go:embed version-ikev2.txt
var versionFile string

const assetName = "node-agent-ikev2"
const buildSuffix = "-ikev2"

func setupXray(_ context.Context, _ Config, _ *command.Dispatcher, _ *nats.Conn) {}

func buildProviders(cfg Config) []collect.MetricProvider {
	return []collect.MetricProvider{
		collect.NewSysProvider(),
		collect.NewTrafficProvider(cfg.Iface),
		collect.NewSwanProvider(cfg.SwanContainer),
	}
}

func startDailyJobs(ctx context.Context, cfg Config) {
	go dailyScheduler(ctx, 2, hostJitter(cfg.Host), func() {
		log.Println("daily self update check: start")
		update.CheckAndUpdate(Version, assetName)
	})
	go dailyScheduler(ctx, 4, hostJitter(cfg.Host), func() {
		log.Println("daily clear charon log: start")
		clearCharonLog(cfg.SwanContainer)
	})
}

func clearCharonLog(container string) {
	out, err := exec.Command("docker", "exec", container,
		"sh", "-c", "test -f /var/log/charon.log && truncate -s 0 /var/log/charon.log || true",
	).CombinedOutput()
	if err != nil {
		log.Printf("clearCharonLog: container=%s err=%v output=%s", container, err, out)
	}
}
