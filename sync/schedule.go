//go:build xray

package sync

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"time"
)

// StartupSync 平滑初始化：
// 1. 全量写配置文件（持久化保证）。
// 2. 尝试探测 gRPC，如果可用则动态注入用户跳过重启。
// 3. 如果 gRPC 不可用，执行 docker restart。
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
		return nil
	}

	log.Printf("xray_sync: gRPC unavailable or inject failed, falling back to restart container=%s", s.container)
	if out, err := exec.Command("docker", "restart", s.container).CombinedOutput(); err != nil {
		return fmt.Errorf("docker restart: %v, output: %s", err, out)
	}

	s.mu.Lock()
	s.current = make(map[string]struct{}, len(users))
	for _, u := range users {
		s.current[u.UUID] = struct{}{}
	}
	s.mu.Unlock()

	log.Printf("xray_sync: startup done via restart, loaded %d users", len(users))
	if err := saveSyncState(syncState{LastSyncTime: time.Now().Unix() - 1}); err != nil {
		log.Printf("xray_sync: save sync state err=%v (non-fatal)", err)
	}
	return nil
}

// diffUsers 计算 remote 与 current 的差集，返回需要新增和删除的 UUID 列表。
func (s *XrayUserSync) diffUsers(remote map[string]struct{}) (toAdd, toRemove []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for uuid := range remote {
		if _, ok := s.current[uuid]; !ok {
			toAdd = append(toAdd, uuid)
		}
	}
	for uuid := range s.current {
		if uuid == defaultUUID {
			continue
		}
		if _, ok := remote[uuid]; !ok {
			toRemove = append(toRemove, uuid)
		}
	}
	return
}

// HourlySync 拉全量用户，diff current，只对变更用户调 xray API。
func (s *XrayUserSync) HourlySync() error {
	users, err := s.fetchUsers()
	if err != nil {
		return fmt.Errorf("fetch users: %w", err)
	}

	remote := make(map[string]struct{}, len(users))
	for _, u := range users {
		remote[u.UUID] = struct{}{}
	}

	toAdd, toRemove := s.diffUsers(remote)
	for _, uuid := range toAdd {
		if err := s.AddUser(uuid); err != nil {
			log.Printf("xray_sync: hourly add user=%s err=%v", uuid, err)
		}
	}
	for _, uuid := range toRemove {
		if err := s.RemoveUser(uuid); err != nil {
			log.Printf("xray_sync: hourly remove user=%s err=%v", uuid, err)
		}
	}
	return nil
}

// DeltaSync 拉增量变更并应用。无状态文件时降级全量 HourlySync。
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

// Start 启动 xray 用户同步的所有后台 goroutine（startup 重试、每小时 delta、每 5 分钟 check_dest）。
// goroutine 在 ctx 取消时退出。
func (s *XrayUserSync) Start(ctx context.Context) {
	go func() {
		for {
			if err := s.StartupSync(); err != nil {
				log.Printf("xray startup sync failed: %v, retrying in 30s", err)
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
		t := time.NewTicker(1 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := s.DeltaSync(); err != nil {
					log.Printf("xray delta sync failed: %v", err)
				}
			}
		}
	}()

	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := s.CheckDest(); err != nil {
					log.Printf("xray check_dest failed: %v", err)
				}
			}
		}
	}()
}

// FullSync 全量拉取并与当前状态 diff，容错处理单用户失败，不更新 last_sync_time。
func (s *XrayUserSync) FullSync() error {
	users, err := s.fetchUsers()
	if err != nil {
		return fmt.Errorf("fetch users: %w", err)
	}

	remote := make(map[string]struct{}, len(users))
	for _, u := range users {
		remote[u.UUID] = struct{}{}
	}

	toAdd, toRemove := s.diffUsers(remote)
	for _, uuid := range toAdd {
		if err := s.AddUser(uuid); err != nil {
			log.Printf("xray_sync: full sync add user=%s err=%v (continuing)", uuid, err)
		}
	}
	for _, uuid := range toRemove {
		if err := s.RemoveUser(uuid); err != nil {
			log.Printf("xray_sync: full sync remove user=%s err=%v (continuing)", uuid, err)
		}
	}

	log.Printf("xray_sync: full sync done add=%d remove=%d", len(toAdd), len(toRemove))
	return nil
}
