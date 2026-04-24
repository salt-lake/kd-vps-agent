package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/salt-lake/kd-vps-agent/collect"
	"github.com/salt-lake/kd-vps-agent/command"
	"gopkg.in/natefinch/lumberjack.v2"
)

// versionFile 由各 build 文件（ikev2.go / xray.go）通过 //go:embed 注入
var Version = strings.TrimSpace(versionFile) + buildSuffix

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(Version)
		return
	}

	cfg := LoadConfig()

	log.SetOutput(&lumberjack.Logger{
		Filename:   "/var/log/node-agent.log",
		MaxSize:    20,
		MaxBackups: 3,
		Compress:   true,
	})
	log.SetFlags(log.LstdFlags)
	log.Printf("node-agent version=%s host=%s protocol=%s", Version, cfg.Host, cfg.Protocol)

	if err := sentry.Init(sentry.ClientOptions{
		Dsn:     os.Getenv("SENTRY_DSN"),
		Release: Version,
	}); err != nil {
		log.Printf("sentry init failed: %v", err)
	}
	sentry.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetTag("host", cfg.Host)
		scope.SetTag("protocol", cfg.Protocol)
	})
	defer sentry.Flush(2 * time.Second)

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
	dispatcher.Register(command.NewSwanUpdateHandler(cfg.SwanContainer, cfg.SwanImage))
	dispatcher.Register(command.BootstrapHandler{})
	dispatcher.Register(command.SelfUpdateHandler{CurrentVersion: Version, AssetName: assetName})
	dispatcher.Register(command.UpdateConfigHandler{})

	setupXray(ctx, cfg, dispatcher, nc)

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

	for {
		select {
		case <-ticker.C:
			p := collector.Collect()
			p.AV = Version
			p.NodeID = cfg.NodeID
			publish(nc, reportSubject, p)
		case <-ctx.Done():
			log.Println("shutting down")
			return
		}
	}
}
