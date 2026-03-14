//go:build xray

package sync

import (
	"sync"

	xrayapi "github.com/salt-lake/kd-vps-agent/xray"
)

// defaultUUID 固定测试用户，永不被同步逻辑删除。
const defaultUUID = "a1b2c3d4-0000-0000-0000-000000000001"

// XrayUserSync 管理 xray 用户的全量同步和实时增量操作。
type XrayUserSync struct {
	apiBase    string
	token      string
	container  string
	apiAddr    string
	inboundTag string
	configPath string
	mu         sync.Mutex
	current    map[string]struct{}
	xrayAPI    *xrayapi.GRPCXrayAPI
}

func NewXrayUserSync(apiBase, token, container, apiAddr, inboundTag, configPath string) *XrayUserSync {
	return &XrayUserSync{
		apiBase:    apiBase,
		token:      token,
		container:  container,
		apiAddr:    apiAddr,
		inboundTag: inboundTag,
		configPath: configPath,
		current:    make(map[string]struct{}),
	}
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
