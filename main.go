package main

import (
	"context"
	_ "embed"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/salt-lake/kd-vps-agent/collect"
	"github.com/salt-lake/kd-vps-agent/command"
	"gopkg.in/natefinch/lumberjack.v2"
)

//go:embed version.txt
var versionFile string

var Version = strings.TrimSpace(versionFile)

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
	dispatcher.Register(command.NewSwanUpdateHandler(cfg.SwanContainer, cfg.SwanImage))
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
