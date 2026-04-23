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

// StartupSync 平滑初始化：
// 1. 从后端拉用户，更新 tiers 缓存
// 2. 全量写配置文件（持久化保证）
// 3. 尝试探测 gRPC，如果可用则动态注入用户跳过重启
// 4. 如果 gRPC 不可用，执行 systemctl restart xray
func (s *XrayUserSync) StartupSync() error {
	users, err := s.fetchUsers()
	if err != nil {
		return fmt.Errorf("fetch users: %w", err)
	}

	if err := s.writeConfig(users); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	if err := s.injectUsers(users); err == nil {
		log.Printf("xray_sync: startup smooth injected %d users, skipped restart", len(users))
		if err := saveSyncState(syncState{LastSyncTime: time.Now().Unix() - 1}); err != nil {
			log.Printf("xray_sync: save sync state err=%v (non-fatal)", err)
		}
		// 启动 / 节点重启后恢复 iptables + tc 规则（从 config 读 portRange）
		if err := s.applyTCFromState(); err != nil {
			log.Printf("xray_sync: apply tc from state err=%v (non-fatal)", err)
		}
		return nil
	}

	log.Printf("xray_sync: gRPC unavailable or inject failed, falling back to systemctl restart xray")
	restartCtx, restartCancel := context.WithTimeout(context.Background(), 30*time.Second)
	out, restartErr := exec.CommandContext(restartCtx, "systemctl", "restart", "xray").CombinedOutput()
	restartCancel()
	if restartErr != nil {
		return fmt.Errorf("systemctl restart xray: %v, output: %s", restartErr, out)
	}

	s.mu.Lock()
	s.current = make(map[string]string, len(users))
	for _, u := range users {
		s.current[u.UUID] = u.Tier
	}
	s.mu.Unlock()

	log.Printf("xray_sync: startup done via restart, loaded %d users", len(users))
	if err := saveSyncState(syncState{LastSyncTime: time.Now().Unix() - 1}); err != nil {
		log.Printf("xray_sync: save sync state err=%v (non-fatal)", err)
	}
	// 重启路径同样需要恢复 tc/iptables
	if err := s.applyTCFromState(); err != nil {
		log.Printf("xray_sync: apply tc from state err=%v (non-fatal)", err)
	}
	return nil
}

// userChange 升降级场景：tier 变化。
type userChange struct {
	UUID     string
	FromTier string
	ToTier   string
}

// diffUsers 返回 add / remove / changeTier 三态。
// remote: uuid → tier name。
// exclude: 临时用户 UUID（永不 remove）。
func (s *XrayUserSync) diffUsers(remote map[string]string, exclude map[string]struct{}) (toAdd []userDTO, toRemove []string, toChange []userChange) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for uuid, rtier := range remote {
		ctier, ok := s.current[uuid]
		if !ok {
			toAdd = append(toAdd, userDTO{UUID: uuid, Tier: rtier})
			continue
		}
		if ctier != rtier {
			toChange = append(toChange, userChange{UUID: uuid, FromTier: ctier, ToTier: rtier})
		}
	}
	for uuid := range s.current {
		if uuid == defaultUUID {
			continue
		}
		if _, ok := exclude[uuid]; ok {
			continue
		}
		if _, ok := remote[uuid]; !ok {
			toRemove = append(toRemove, uuid)
		}
	}
	return
}

// tempUserSet 返回临时用户 UUID 集合快照（无临时同步器时返回空集合）。
func (s *XrayUserSync) tempUserSet() map[string]struct{} {
	if s.tempSync == nil {
		return nil
	}
	return s.tempSync.UUIDSet()
}

// applyTierChanges 执行三态同步：先 remove，再 change（remove+add），最后 add。
// onErr 控制错误处理：fatal=true 时遇到错误立即返回；false 时只 log 继续。
func (s *XrayUserSync) applyTierChanges(toAdd []userDTO, toRemove []string, toChange []userChange, fatal bool) error {
	for _, uuid := range toRemove {
		if err := s.RemoveUser(uuid); err != nil {
			if fatal {
				return fmt.Errorf("remove user=%s: %w", uuid, err)
			}
			log.Printf("xray_sync: remove user=%s err=%v (continuing)", uuid, err)
		}
	}
	for _, c := range toChange {
		if err := s.RemoveUser(c.UUID); err != nil {
			if fatal {
				return fmt.Errorf("change remove user=%s: %w", c.UUID, err)
			}
			log.Printf("xray_sync: change remove user=%s err=%v (continuing)", c.UUID, err)
			continue
		}
		if err := s.AddUser(c.UUID, c.ToTier); err != nil {
			if fatal {
				return fmt.Errorf("change add user=%s tier=%s: %w", c.UUID, c.ToTier, err)
			}
			log.Printf("xray_sync: change add user=%s tier=%s err=%v (continuing)", c.UUID, c.ToTier, err)
		}
	}
	for _, u := range toAdd {
		if err := s.AddUser(u.UUID, u.Tier); err != nil {
			if fatal {
				return fmt.Errorf("add user=%s tier=%s: %w", u.UUID, u.Tier, err)
			}
			log.Printf("xray_sync: add user=%s tier=%s err=%v (continuing)", u.UUID, u.Tier, err)
		}
	}
	return nil
}

// HourlySync 拉全量用户，diff current，只对变更用户调 xray API。个别错误不阻塞。
func (s *XrayUserSync) HourlySync() error {
	users, err := s.fetchUsers()
	if err != nil {
		return fmt.Errorf("fetch users: %w", err)
	}

	remote := make(map[string]string, len(users))
	for _, u := range users {
		remote[u.UUID] = u.Tier
	}

	toAdd, toRemove, toChange := s.diffUsers(remote, s.tempUserSet())
	_ = s.applyTierChanges(toAdd, toRemove, toChange, false)
	// tier 字典可能变化（如 pool_mbps 被后端调整），顺带 re-apply ratelimit
	if err := s.applyTCFromState(); err != nil {
		log.Printf("xray_sync: apply tc from state err=%v (non-fatal)", err)
	}
	return nil
}

// DeltaSync 拉增量变更并应用。无状态文件时降级全量 HourlySync。
// delta 错误是 fatal（会传播）。
func (s *XrayUserSync) DeltaSync() error {
	state, err := loadSyncState()
	if err != nil {
		log.Printf("xray_sync: no sync state, falling back to full sync: %v", err)
		return s.HourlySync()
	}

	delta, err := s.fetchDelta(state.LastSyncTime)
	if err != nil {
		return fmt.Errorf("fetch delta: %w", err)
	}

	// Delta 语义：先 remove 再 add，保证 tier 变化时干净
	for _, uuid := range delta.Removed {
		if err := s.RemoveUser(uuid); err != nil {
			return fmt.Errorf("delta remove user=%s: %w", uuid, err)
		}
	}
	for _, u := range delta.Added {
		if err := s.AddUser(u.UUID, u.Tier); err != nil {
			return fmt.Errorf("delta add user=%s tier=%s: %w", u.UUID, u.Tier, err)
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
		case <-time.After(5 * time.Second):
		}
		// 每 10 次重新拉取，避免用户列表过期
		if attempt%10 == 0 {
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

// Start 启动 xray 用户同步的所有后台 goroutine。
func (s *XrayUserSync) Start(ctx context.Context) {
	go func() {
		for {
			if err := s.StartupSync(); err != nil {
				log.Printf("xray startup sync failed: %v, retrying in 30s", err)
				sentry.CaptureException(err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(30 * time.Second):
				}
				continue
			}
			break
		}
	}()

	go func() {
		t := time.NewTicker(deltaSyncInterval)
		defer t.Stop()
		consecutiveFails := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := s.DeltaSync(); err != nil {
					consecutiveFails++
					log.Printf("xray delta sync failed (%d): %v", consecutiveFails, err)
					sentry.CaptureException(err)
					if consecutiveFails >= 3 {
						consecutiveFails = 0
						log.Printf("xray_sync: 3 consecutive delta failures, falling back to full sync")
						if err := s.FullSync(); err != nil {
							log.Printf("xray full sync failed: %v", err)
							sentry.CaptureException(err)
						}
					}
				} else {
					consecutiveFails = 0
				}
			}
		}
	}()

	go s.watchXrayHealth(ctx)
}

// FullSync 全量拉取并与当前状态 diff，容错处理单用户失败，不更新 last_sync_time。
func (s *XrayUserSync) FullSync() error {
	users, err := s.fetchUsers()
	if err != nil {
		return fmt.Errorf("fetch users: %w", err)
	}

	remote := make(map[string]string, len(users))
	for _, u := range users {
		remote[u.UUID] = u.Tier
	}

	toAdd, toRemove, toChange := s.diffUsers(remote, s.tempUserSet())
	_ = s.applyTierChanges(toAdd, toRemove, toChange, false)
	if err := s.applyTCFromState(); err != nil {
		log.Printf("xray_sync: apply tc from state err=%v (non-fatal)", err)
	}

	log.Printf("xray_sync: full sync add=%d remove=%d change=%d", len(toAdd), len(toRemove), len(toChange))
	return nil
}
