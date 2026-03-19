//go:build xray

package xray

import (
	"context"
	"log"
	"sync"
	"time"
)

// userManager 是 TempUserSync 依赖的最小接口，由 XrayUserSync 满足。
type userManager interface {
	AddUser(uuid string) error
	RemoveUser(uuid string) error
}

// TempUserSync 管理临时用户的轮询同步（仅 gRPC 注入，不写配置文件）。
type TempUserSync struct {
	apiBase string
	token   string
	manager userManager
	mu      sync.RWMutex
	version string
	uuids   []string
}

func NewTempUserSync(apiBase, token string, manager userManager) *TempUserSync {
	return &TempUserSync{
		apiBase: apiBase,
		token:   token,
		manager: manager,
	}
}

// startup 初次拉取并注入全部临时用户。
func (t *TempUserSync) startup() error {
	version, uuids, err := fetchTempUsers(t.apiBase, t.token)
	if err != nil {
		return err
	}
	for _, uuid := range uuids {
		if err := t.manager.AddUser(uuid); err != nil {
			log.Printf("temp_sync: startup add user=%s err=%v", uuid, err)
		}
	}
	t.mu.Lock()
	t.version = version
	t.uuids = uuids
	t.mu.Unlock()
	log.Printf("temp_sync: startup injected %d users", len(uuids))
	return nil
}

// poll 拉取最新列表，version 相同则跳过，否则 diff 后先 add 再 remove。
func (t *TempUserSync) poll() error {
	version, uuids, err := fetchTempUsers(t.apiBase, t.token)
	if err != nil {
		return err
	}

	t.mu.RLock()
	sameVersion := t.version == version
	cached := t.uuids
	t.mu.RUnlock()

	if sameVersion {
		return nil
	}

	newSet := make(map[string]struct{}, len(uuids))
	for _, u := range uuids {
		newSet[u] = struct{}{}
	}
	cachedSet := make(map[string]struct{}, len(cached))
	for _, u := range cached {
		cachedSet[u] = struct{}{}
	}

	var toAdd, toRemove []string
	for u := range newSet {
		if _, ok := cachedSet[u]; !ok {
			toAdd = append(toAdd, u)
		}
	}
	for u := range cachedSet {
		if _, ok := newSet[u]; !ok {
			toRemove = append(toRemove, u)
		}
	}

	for _, uuid := range toAdd {
		if err := t.manager.AddUser(uuid); err != nil {
			log.Printf("temp_sync: poll add user=%s err=%v", uuid, err)
		}
	}
	for _, uuid := range toRemove {
		if err := t.manager.RemoveUser(uuid); err != nil {
			log.Printf("temp_sync: poll remove user=%s err=%v", uuid, err)
		}
	}

	t.mu.Lock()
	t.version = version
	t.uuids = uuids
	t.mu.Unlock()
	log.Printf("temp_sync: poll done add=%d remove=%d", len(toAdd), len(toRemove))
	return nil
}

// ReInjectAll 将缓存的临时用户重新注入 xray（用于 xray 重启后恢复）。
func (t *TempUserSync) ReInjectAll() {
	t.mu.RLock()
	uuids := t.uuids
	t.mu.RUnlock()
	for _, uuid := range uuids {
		if err := t.manager.AddUser(uuid); err != nil {
			log.Printf("temp_sync: re-inject user=%s err=%v", uuid, err)
		}
	}
	log.Printf("temp_sync: re-injected %d temp users", len(uuids))
}

// Start 启动后台 goroutine：先 startup（30s 重试），再每 5 分钟 poll。
func (t *TempUserSync) Start(ctx context.Context) {
	go func() {
		for {
			if err := t.startup(); err != nil {
				log.Printf("temp_sync: startup err=%v, retrying in 30s", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(30 * time.Second):
				}
				continue
			}
			break
		}

		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := t.poll(); err != nil {
					log.Printf("temp_sync: poll err=%v", err)
				}
			}
		}
	}()
}
