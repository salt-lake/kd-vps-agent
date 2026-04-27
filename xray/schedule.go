//go:build xray

package xray

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"
)

// DeltaSync 拉增量变更并应用。无状态文件时（首次安装）记录当前时间为起点，不拉历史用户。
func (s *XrayUserSync) DeltaSync() error {
	state, err := loadSyncState()
	if err != nil {
		log.Printf("xray_sync: no sync state, initializing cursor at now")
		return saveSyncState(syncState{LastSyncTime: time.Now().Unix() - 1})
	}

	delta, err := s.fetchDelta(state.LastSyncTime)
	if err != nil {
		return fmt.Errorf("fetch delta: %w", err)
	}

	for _, uuid := range delta.Added {
		if err := s.AddUser(uuid); err != nil {
			return fmt.Errorf("delta add user=%s: %w", uuid, err)
		}
	}
	for _, uuid := range delta.Removed {
		if err := s.RemoveUser(uuid); err != nil {
			return fmt.Errorf("delta remove user=%s: %w", uuid, err)
		}
	}

	if err := saveSyncState(syncState{LastSyncTime: time.Now().Unix() - 1}); err != nil {
		log.Printf("xray_sync: save sync state err=%v (non-fatal)", err)
	}
	return nil
}

// isXrayHealthy 探测 xray gRPC 是否可用。
func (s *XrayUserSync) isXrayHealthy() bool {
	api, err := s.getAPI()
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return api.IsXrayReady(ctx)
}

// syncAfterRestart 等待 xray gRPC 可用后重新全量注入用户，持续重试直到成功或 ctx 取消。
// 每 5s 重试一次，每 10 次（50s）重新从 API 拉取最新用户列表。
func (s *XrayUserSync) syncAfterRestart(ctx context.Context) {
	users, err := s.fetchUsers()
	if err != nil {
		log.Printf("xray_sync: post-restart fetch users err=%v, will retry", err)
		sentry.CaptureException(err)
	}
	for attempt := 1; ; attempt++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(syncRestartRetryInterval):
		}
		// 每 N 次重新拉取，避免用户列表过期
		if attempt%syncRestartRefreshEvery == 0 {
			if fresh, err := s.fetchUsers(); err == nil {
				users = fresh
			}
		}
		if users == nil {
			continue
		}
		if err := s.injectUsers(users); err == nil {
			log.Printf("xray_sync: post-restart injected %d users OK (attempt=%d)", len(users), attempt)
			if s.tempSync != nil {
				s.tempSync.ReInjectAll()
			}
			return
		}
	}
}

// watchXrayHealth 每 30s 探测 xray gRPC 健康状态，连续 2 次失败则 systemctl restart xray 并重注入用户。
func (s *XrayUserSync) watchXrayHealth(ctx context.Context) {
	consecutiveFails := 0
	t := time.NewTicker(healthCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.isXrayHealthy() {
				consecutiveFails = 0
				continue
			}
			consecutiveFails++
			log.Printf("xray_sync: health check failed (%d/2)", consecutiveFails)
			if consecutiveFails < 2 {
				continue
			}
			consecutiveFails = 0
			log.Printf("xray_sync: xray unhealthy, restarting via systemctl")
			s.mu.Lock()
			if s.xrayAPI != nil {
				if err := s.xrayAPI.Close(); err != nil {
					log.Printf("xray_sync: close gRPC conn err=%v", err)
				}
				s.xrayAPI = nil
			}
			s.mu.Unlock()
			restartCtx, restartCancel := context.WithTimeout(context.Background(), 30*time.Second)
			restartOut, restartErr := exec.CommandContext(restartCtx, "systemctl", "restart", "xray").CombinedOutput()
			restartCancel()
			if restartErr != nil {
				log.Printf("xray_sync: systemctl restart xray failed: %v, output: %s", restartErr, restartOut)
				sentry.CaptureException(fmt.Errorf("systemctl restart xray: %w, output: %s", restartErr, restartOut))
				continue
			}
			if atomic.CompareAndSwapInt32(&s.restartSyncInFlight, 0, 1) {
				go func() {
					defer atomic.StoreInt32(&s.restartSyncInFlight, 0)
					s.syncAfterRestart(ctx)
				}()
			} else {
				log.Printf("xray_sync: syncAfterRestart already in flight, skipping")
			}
		}
	}
}

// Start 启动 xray 用户同步的后台 goroutine：30 分钟一次 DeltaSync + xray 健康监测。
// 启动时不做任何全量拉取——业务方通过幂等的 HTTP API 调用主动维护节点用户。
// goroutine 在 ctx 取消时退出。
func (s *XrayUserSync) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(deltaSyncInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := s.DeltaSync(); err != nil {
					log.Printf("xray delta sync failed: %v", err)
					sentry.CaptureException(err)
				}
			}
		}
	}()

	go s.watchXrayHealth(ctx)
}

