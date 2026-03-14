package main

import (
	"context"
	"log"
	"os/exec"
	"time"

	"github.com/salt-lake/kd-vps-agent/sync"
)

var cst = time.FixedZone("CST", 8*3600)

// startDailyJobs 启动两个独立的每日定时任务。
func startDailyJobs(ctx context.Context, swanContainer string, syncer *sync.XrayUserSync) {
	go startFullSyncJob(ctx, syncer)
	go startClearLogJob(ctx, swanContainer)
}

// dailyScheduler 每天在指定北京时间小时触发 fn，ctx 取消时退出。
func dailyScheduler(ctx context.Context, hour int, fn func()) {
	for {
		now := time.Now().In(cst)
		next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, cst)
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

// startFullSyncJob 每天北京时间 03:00 执行全量 xray 用户同步。
func startFullSyncJob(ctx context.Context, syncer *sync.XrayUserSync) {
	if syncer == nil {
		return
	}
	dailyScheduler(ctx, 3, func() {
		if err := syncer.FullSync(); err != nil {
			log.Printf("startFullSyncJob: err=%v", err)
		}
	})
}

// startClearLogJob 每天北京时间 04:00 清空 charon.log。
func startClearLogJob(ctx context.Context, swanContainer string) {
	dailyScheduler(ctx, 4, func() { clearCharonLog(swanContainer) })
}

func clearCharonLog(container string) {
	out, err := exec.Command("docker", "exec", container,
		"sh", "-c", "test -f /var/log/charon.log && truncate -s 0 /var/log/charon.log || true",
	).CombinedOutput()
	if err != nil {
		log.Printf("clearCharonLog: container=%s err=%v output=%s", container, err, out)
		return
	}
}
