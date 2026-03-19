//go:build xray

package xray

import (
	"context"
	"sync"
)

// defaultUUID 固定测试用户，永不被同步逻辑删除。
const defaultUUID = "a1b2c3d4-0000-0000-0000-000000000001"

// XrayUserSync 管理 xray 用户的全量同步和实时增量操作。
type XrayUserSync struct {
	apiBase    string
	token      string
	apiAddr    string
	inboundTag string
	configPath string
	mu         sync.Mutex
	current    map[string]struct{}
	xrayAPI    *GRPCXrayAPI
	tempSync   *TempUserSync
}

// SetTempSync 注入临时用户同步器，供 xray 重启后重注入临时用户。
func (s *XrayUserSync) SetTempSync(ts *TempUserSync) {
	s.tempSync = ts
}

func NewXrayUserSync(apiBase, token, apiAddr, inboundTag, configPath string) *XrayUserSync {
	return &XrayUserSync{
		apiBase:    apiBase,
		token:      token,
		apiAddr:    apiAddr,
		inboundTag: inboundTag,
		configPath: configPath,
		current:    make(map[string]struct{}),
	}
}

// TriggerResync 等待 xray gRPC 可用后重新全量注入用户（供外部调用）。
func (s *XrayUserSync) TriggerResync(ctx context.Context) {
	s.syncAfterRestart(ctx)
}

// Close 关闭持有的 gRPC 连接。
func (s *XrayUserSync) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.xrayAPI != nil {
		err := s.xrayAPI.Close()
		s.xrayAPI = nil
		return err
	}
	return nil
}
