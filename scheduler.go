package main

import (
	"context"
	"strconv"
	"strings"
	"time"
)

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
