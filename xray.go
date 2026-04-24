//go:build xray

package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/salt-lake/kd-vps-agent/collect"
	"github.com/salt-lake/kd-vps-agent/command"
	"github.com/salt-lake/kd-vps-agent/ratelimit"
	"github.com/salt-lake/kd-vps-agent/update"
	"github.com/salt-lake/kd-vps-agent/xray"
)

// migrateReportSubject agent 迁移完成后向后端回报的 NATS subject。
// 和 kd-vps-backend/app/node/application/lifecycle/xray_migrate_tier_report.go 保持一致。
const migrateReportSubject = "node.report.xray.tier-migrated"

//go:embed version-xray.txt
var versionFile string

const assetName = "node-agent-xray"
const buildSuffix = "-xray"

func setupXray(ctx context.Context, cfg Config, d *command.Dispatcher, nc *nats.Conn) {
	if cfg.APIBase == "" || cfg.ScriptToken == "" {
		log.Println("xray sync disabled: API_BASE or SCRIPT_TOKEN not set")
		return
	}
	syncer := xray.NewXrayUserSync(
		cfg.APIBase, cfg.ScriptToken,
		cfg.XrayAPIAddr, cfg.XrayInboundTag, cfg.XrayConfigPath,
	)
	tempSync := xray.NewTempUserSync(cfg.APIBase, cfg.ScriptToken, syncer)
	// tempSync 要等 syncer 首次 fetchUsers 成功（tiers 就绪）后再 AddUser，
	// 否则迁移后节点上空 tier 会路由到已不存在的老 "proxy" inbound。
	tempSync.SetReadyChannel(syncer.StartupReady())
	syncer.SetTempSync(tempSync)

	// 限速能力：若启用，初始化 ratelimit manager 并注入 syncer
	if cfg.RatelimitEnabled {
		iface := resolveRatelimitIface(ctx, cfg)
		rl := ratelimit.NewManager(iface, execShell)
		syncer.SetRatelimit(rl)
		log.Printf("ratelimit manager enabled on iface=%s", iface)
	} else {
		log.Println("ratelimit disabled via RATELIMIT_ENABLED=false")
	}

	// 注入迁移回报：后端订阅 node.report.xray.tier-migrated 翻转 tb_node.xray_tier_migrated。
	// 丢失上报会让后端永远停在 migrated=false，分享链接生成器用 reality 端口，SVIP 用户连错 inbound。
	// 所以这里 Publish 后必须 Flush 把消息挤到 socket，而不是仅落入客户端缓冲。
	syncer.SetMigrateReporter(func(success bool, errMsg string) {
		payload, err := json.Marshal(map[string]any{
			"host":    cfg.Host,
			"success": success,
			"error":   errMsg,
		})
		if err != nil {
			log.Printf("migrate report: marshal payload err=%v", err)
			return
		}
		if err := nc.Publish(migrateReportSubject, payload); err != nil {
			log.Printf("migrate report: publish err=%v", err)
			return
		}
		if err := nc.FlushTimeout(2 * time.Second); err != nil {
			log.Printf("migrate report: flush err=%v", err)
		}
	})

	syncer.Start(ctx)
	tempSync.Start(ctx)
	d.Register(command.NewXrayUserAddHandler(syncer))
	d.Register(command.NewXrayUserRemoveHandler(syncer))
	d.Register(command.NewXrayUpdateHandler(ctx, syncer, cfg.XrayConfigPath))
	d.Register(command.NewXrayFullSyncHandler(syncer))
	d.Register(command.NewXrayMigrateTierHandler(syncer))
	go dailyScheduler(ctx, 3, hostJitter(cfg.Host), func() {
		log.Println("daily full sync: start")
		if err := syncer.FullSync(); err != nil {
			log.Printf("xray full sync: %v", err)
		}
	})
}

func buildProviders(cfg Config) []collect.MetricProvider {
	providers := []collect.MetricProvider{
		collect.NewSysProvider(),
		collect.NewTrafficProvider(cfg.Iface),
		collect.NewXrayProvider(cfg.XrayAPIAddr, cfg.XrayConfigPath, cfg.XrayInboundTag),
	}
	if cfg.RatelimitEnabled {
		iface := resolveRatelimitIface(context.Background(), cfg)
		providers = append(providers, collect.NewTcStatsProvider(iface, true))
	}
	return providers
}

func startDailyJobs(ctx context.Context, cfg Config) {
	go dailyScheduler(ctx, 2, hostJitter(cfg.Host), func() {
		log.Println("daily self update check: start")
		update.CheckAndUpdate(Version, assetName)
	})
}

// resolveRatelimitIface 按优先级决定 tc 工作的网卡名。
// RATELIMIT_IFACE 显式配置 > ratelimit.DetectIface 自动探测。
func resolveRatelimitIface(ctx context.Context, cfg Config) string {
	if cfg.RatelimitIface != "" {
		return cfg.RatelimitIface
	}
	return ratelimit.DetectIface(ctx)
}

// execShell 供 ratelimit.Manager 注入，运行 tc 等命令，失败时附带 combined output 方便排障。
func execShell(cmd string, args ...string) error {
	out, err := exec.Command(cmd, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %v, output=%s", cmd, args, err, string(out))
	}
	return nil
}
