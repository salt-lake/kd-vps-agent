package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/salt-lake/kd-vps-agent/collect"
	"github.com/salt-lake/kd-vps-agent/command"
	"github.com/salt-lake/kd-vps-agent/sync"
	"github.com/salt-lake/kd-vps-agent/update"
	"gopkg.in/natefinch/lumberjack.v2"
)

//go:embed version.txt
var versionFile string

var Version = strings.TrimSpace(versionFile)

// Config 集中管理所有环境变量，新增配置只需改 LoadConfig。
type Config struct {
	NATSUrl        string
	NATSToken      string
	Host           string
	APIBase        string
	ScriptToken    string
	Protocol       string
	SwanContainer  string
	XrayContainer  string
	XrayAPIAddr    string
	XrayInboundTag string
	XrayConfigPath string
	Iface          string
	ReportInterval time.Duration
}

func LoadConfig() Config {
	token := os.Getenv("NATS_AUTH_TOKEN")
	return Config{
		NATSUrl:        envOr("NATS_URL", nats.DefaultURL),
		NATSToken:      token,
		Host:           os.Getenv("NODE_HOST"),
		APIBase:        strings.TrimRight(os.Getenv("API_BASE"), "/"),
		ScriptToken:    envOr("SCRIPT_TOKEN", token),
		Protocol:       envOr("NODE_PROTOCOL", "ikev2"),
		SwanContainer:  envOr("SWAN_CONTAINER", "strongswan"),
		XrayContainer:  envOr("XRAY_CONTAINER", "xray"),
		XrayAPIAddr:    envOr("XRAY_API_ADDR", "127.0.0.1:10085"),
		XrayInboundTag: envOr("XRAY_INBOUND_TAG", "proxy"),
		XrayConfigPath: envOr("XRAY_CONFIG_PATH", "/etc/xray/config.json"),
		Iface:          collect.DetectPrimaryIface(),
		ReportInterval: parseDuration(envOr("REPORT_INTERVAL", "2m")),
	}
}

func main() {
	cfg := LoadConfig()

	log.SetOutput(&lumberjack.Logger{
		Filename:   "/var/log/node-agent.log",
		MaxSize:    20,   // MB
		MaxBackups: 3,
		Compress:   true,
	})
	log.SetFlags(log.LstdFlags)
	log.Printf("node-agent version=%s host=%s", Version, cfg.Host)

	if cfg.Host == "" {
		log.Fatal("NODE_HOST is required")
	}
	if cfg.Protocol != "ikev2" && cfg.Protocol != "xray" {
		log.Fatalf("unknown NODE_PROTOCOL=%s (supported: ikev2, xray)", cfg.Protocol)
	}

	nc, err := newNATSConn(cfg.NATSUrl, cfg.NATSToken)
	if err != nil {
		log.Fatalf("connect nats failed after 10 attempts: %v", err)
	}
	defer nc.Drain()

	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		cancel()
	}()

	// IP 中的点替换为 - 避免与 NATS subject 分隔符冲突
	hostKey := strings.ReplaceAll(cfg.Host, ".", "-")
	reportSubject := "node.report." + hostKey
	cmdSubject := "node.cmd." + hostKey

	dispatcher := command.NewDispatcher()
	dispatcher.Register(command.DockerRestartHandler{})
	dispatcher.Register(command.BootstrapHandler{})
	if cfg.APIBase != "" && cfg.ScriptToken != "" {
		dispatcher.Register(command.SelfUpdateHandler{
			APIBase:        cfg.APIBase,
			Token:          cfg.ScriptToken,
			CurrentVersion: Version,
		})
	}

	var xraySyncer *sync.XrayUserSync
	if cfg.Protocol == "xray" && cfg.APIBase != "" && cfg.ScriptToken != "" {
		xraySyncer = sync.NewXrayUserSync(
			cfg.APIBase, cfg.ScriptToken,
			cfg.XrayContainer, cfg.XrayAPIAddr,
			cfg.XrayInboundTag, cfg.XrayConfigPath,
		)
		xraySyncer.Start(ctx)
		dispatcher.Register(command.NewXrayUserAddHandler(xraySyncer))
		dispatcher.Register(command.NewXrayUserRemoveHandler(xraySyncer))
	}

	collector := collect.NewCollector(buildProviders(cfg)...)

	// 订阅节点专属指令
	if _, err := nc.Subscribe(cmdSubject, dispatcher.Dispatch); err != nil {
		log.Fatalf("subscribe cmd failed: %v", err)
	}

	// 订阅协议分组广播
	protoSubject := "node.cmd.proto." + cfg.Protocol
	if _, err := nc.Subscribe(protoSubject, dispatcher.Dispatch); err != nil {
		log.Fatalf("subscribe proto broadcast failed: %v", err)
	}

	// 每日定时任务（03:00 全量对齐 / 04:00 清空 charon.log）
	go startDailyJobs(ctx, cfg.SwanContainer, xraySyncer)

	// 立即上报一次
	p := collector.Collect()
	p.AV = Version
	publish(nc, reportSubject, p)

	ticker := time.NewTicker(cfg.ReportInterval)
	defer ticker.Stop()

	// 自更新定时器
	updateC := make(<-chan time.Time)
	if cfg.APIBase != "" && cfg.ScriptToken != "" {
		t := time.NewTicker(1 * time.Hour)
		defer t.Stop()
		updateC = t.C
		update.CheckAndUpdate(cfg.APIBase, cfg.ScriptToken, Version)
	}

	for {
		select {
		case <-ticker.C:
			p := collector.Collect()
			p.AV = Version
			publish(nc, reportSubject, p)
		case <-updateC:
			update.CheckAndUpdate(cfg.APIBase, cfg.ScriptToken, Version)
		case <-ctx.Done():
			log.Println("shutting down")
			return
		}
	}
}

// newNATSConn 带 10 次重试的 NATS 连接工厂。
func newNATSConn(url, token string) (*nats.Conn, error) {
	opts := []nats.Option{
		nats.Name("kd-node-agent"),
		nats.ReconnectWait(5 * time.Second),
		nats.MaxReconnects(-1),
	}
	if token != "" {
		opts = append(opts, nats.Token(token))
	}
	var nc *nats.Conn
	var err error
	for i := 0; i < 10; i++ {
		nc, err = nats.Connect(url, opts...)
		if err == nil {
			return nc, nil
		}
		log.Printf("connect nats failed (attempt %d/10): %v, retrying in 10s", i+1, err)
		time.Sleep(10 * time.Second)
	}
	return nil, fmt.Errorf("last error: %w", err)
}

// buildProviders 根据 Config 组装采集器列表，main 不感知协议细节。
func buildProviders(cfg Config) []collect.MetricProvider {
	providers := []collect.MetricProvider{
		collect.NewSysProvider(),
		collect.NewTrafficProvider(cfg.Iface),
	}
	switch cfg.Protocol {
	case "ikev2":
		providers = append(providers, collect.NewSwanProvider(cfg.SwanContainer))
	case "xray":
		providers = append(providers, collect.NewXrayProvider(cfg.XrayContainer, cfg.XrayAPIAddr))
	}
	return providers
}

func publish(nc *nats.Conn, subject string, p collect.Payload) {
	b, err := json.Marshal(p)
	if err != nil {
		log.Printf("publish: marshal failed: %v", err)
		return
	}
	if err := nc.Publish(subject, b); err != nil {
		log.Printf("publish: failed subject=%s err=%v", subject, err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 2 * time.Minute
	}
	return d
}
