package main

import (
	"context"
	"log"
	"os/exec"
	"time"
)

var cst = time.FixedZone("CST", 8*3600)

// startDailyJobs 启动每日定时任务。
// fullSync 为 xray 全量同步函数，ikev2 传 nil。
func startDailyJobs(ctx context.Context, swanContainer string, fullSync func()) {
	go startFullSyncJob(ctx, fullSync)
	go startClearLogJob(ctx, swanContainer)
}

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
func startFullSyncJob(ctx context.Context, fullSync func()) {
	if fullSync == nil {
		return
	}
	dailyScheduler(ctx, 3, func() {
		log.Println("daily full sync: start")
		fullSync()
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
	}
}
