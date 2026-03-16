package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/salt-lake/kd-vps-agent/collect"
	"github.com/salt-lake/kd-vps-agent/command"
	"github.com/salt-lake/kd-vps-agent/update"
	"gopkg.in/natefinch/lumberjack.v2"
)

//go:embed version.txt
var versionFile string

var Version = strings.TrimSpace(versionFile)

type Config struct {
	NATSUrl        string
	NATSToken      string
	Host           string
	NodeID         string
	APIBase        string
	ScriptToken    string
	Protocol       string
	SwanContainer  string
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
		NodeID:         os.Getenv("NODE_ID"),
		APIBase:        strings.TrimRight(os.Getenv("API_BASE"), "/"),
		ScriptToken:    envOr("SCRIPT_TOKEN", token),
		Protocol:       envOr("NODE_PROTOCOL", "ikev2"),
		SwanContainer:  envOr("SWAN_CONTAINER", "strongswan"),
		XrayAPIAddr:    envOr("XRAY_API_ADDR", "127.0.0.1:10085"),
		XrayInboundTag: envOr("XRAY_INBOUND_TAG", "vless"),
		XrayConfigPath: envOr("XRAY_CONFIG_PATH", "/etc/xray/config.json"),
		Iface:          collect.DetectPrimaryIface(),
		ReportInterval: parseDuration(envOr("REPORT_INTERVAL", "2m")),
	}
}

func main() {
	cfg := LoadConfig()

	log.SetOutput(&lumberjack.Logger{
		Filename:   "/var/log/node-agent.log",
		MaxSize:    20,
		MaxBackups: 3,
		Compress:   true,
	})
	log.SetFlags(log.LstdFlags)
	log.Printf("node-agent version=%s host=%s protocol=%s", Version, cfg.Host, cfg.Protocol)

	if cfg.Host == "" {
		log.Fatal("NODE_HOST is required")
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

	hostKey := strings.ReplaceAll(cfg.Host, ".", "-")
	reportSubject := "node.report." + hostKey
	cmdSubject := "node.cmd." + hostKey

	dispatcher := command.NewDispatcher()
	dispatcher.Register(command.NewSwanUpdateHandler(cfg.SwanContainer))
	dispatcher.Register(command.BootstrapHandler{})
	dispatcher.Register(command.SelfUpdateHandler{CurrentVersion: Version, AssetName: assetName})

	setupXray(ctx, cfg, dispatcher)

	collector := collect.NewCollector(buildProviders(cfg)...)

	if _, err := nc.Subscribe(cmdSubject, dispatcher.Dispatch); err != nil {
		log.Fatalf("subscribe cmd failed: %v", err)
	}

	protoSubject := "node.cmd.proto." + cfg.Protocol
	if _, err := nc.Subscribe(protoSubject, dispatcher.Dispatch); err != nil {
		log.Fatalf("subscribe proto broadcast failed: %v", err)
	}

	go startDailyJobs(ctx, cfg)

	p := collector.Collect()
	p.AV = Version
	p.NodeID = cfg.NodeID
	publish(nc, reportSubject, p)

	ticker := time.NewTicker(cfg.ReportInterval)
	defer ticker.Stop()
	updateTicker := time.NewTicker(1 * time.Hour)
	defer updateTicker.Stop()
	update.CheckAndUpdate(Version, assetName)

	for {
		select {
		case <-ticker.C:
			p := collector.Collect()
			p.AV = Version
			p.NodeID = cfg.NodeID
			publish(nc, reportSubject, p)
		case <-updateTicker.C:
			update.CheckAndUpdate(Version, assetName)
		case <-ctx.Done():
			log.Println("shutting down")
			return
		}
	}
}

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

var cst = time.FixedZone("CST", 8*3600)

// dailyScheduler 每天在北京时间 hour 点（加 jitter 偏移）执行 fn。
// jitter 应基于节点 IP 计算，避免多节点同时触发。
func dailyScheduler(ctx context.Context, hour int, jitter time.Duration, fn func()) {
	for {
		now := time.Now().In(cst)
		next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, cst).Add(jitter)
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
			fn()
		}
	}
}

// hostJitter 取 host IP 末段 mod 60 作为秒级抖动，同节点固定，不同节点错开。
func hostJitter(host string) time.Duration {
	parts := strings.Split(host, ".")
	last := parts[len(parts)-1]
	n, err := strconv.Atoi(last)
	if err != nil {
		return 0
	}
	return time.Duration(n%60) * time.Second
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
