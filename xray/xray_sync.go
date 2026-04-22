//go:build xray

package xray

import (
	"context"
	"sync"
	"time"

	"github.com/salt-lake/kd-vps-agent/ratelimit"
)

const (
	defaultUUID = "a1b2c3d4-0000-0000-0000-000000000001" // 固定测试用户，永不被同步逻辑删除

	// 同步策略
	deltaSyncInterval   = 5 * time.Minute
	tempSyncInterval    = 5 * time.Minute
	healthCheckInterval = 30 * time.Second
)

// TierConfig 由后端下发的 tier 配置（稳态 API 返回）。
// 不含 Port：端口在迁移时烘焙进 xray config，稳态运行不需要。
type TierConfig struct {
	MarkID     int
	InboundTag string
	PoolMbps   int
}

// TCApplier 由 ratelimit.Manager 实现，注入到 XrayUserSync 供迁移和 tier 调整时应用 tc 规则。
// 接受 ratelimit.TierConfig（只含 MarkID/PoolMbps，不关心 inboundTag）。
type TCApplier interface {
	Apply(tiers map[string]ratelimit.TierConfig) error
}

// XrayUserSync 管理 xray 用户的全量同步和实时增量操作。
type XrayUserSync struct {
	apiBase    string
	token      string
	apiAddr    string
	inboundTag string // 兼容模式：tiers 为空时退化为单 inbound
	configPath string

	mu                  sync.Mutex
	tiers               map[string]TierConfig // 由后端下发缓存；空则兼容模式
	defaultTier         string                // 用户 tier 缺失时回退目标；仅在非兼容模式使用
	current             map[string]string     // uuid → tier name；兼容模式下 tier=""
	xrayAPI             XrayAPI
	tempSync            *TempUserSync
	ratelimit           TCApplier // 由外部注入，nil 时不应用 tc
	restartSyncInFlight int32     // atomic: 1 if syncAfterRestart goroutine is running
}

// SetTempSync 注入临时用户同步器，供 xray 重启后重注入临时用户。
func (s *XrayUserSync) SetTempSync(ts *TempUserSync) {
	s.tempSync = ts
}

// SetRatelimit 注入 ratelimit manager，供迁移流程和后续稳态应用限速规则。
func (s *XrayUserSync) SetRatelimit(m TCApplier) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ratelimit = m
}

func NewXrayUserSync(apiBase, token, apiAddr, inboundTag, configPath string) *XrayUserSync {
	return &XrayUserSync{
		apiBase:    apiBase,
		token:      token,
		apiAddr:    apiAddr,
		inboundTag: inboundTag,
		configPath: configPath,
		current:    make(map[string]string),
		tiers:      make(map[string]TierConfig),
	}
}

// Tiers 返回当前缓存的 tier 字典的快照副本（供外部 ratelimit manager 调用）。
func (s *XrayUserSync) Tiers() map[string]TierConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]TierConfig, len(s.tiers))
	for k, v := range s.tiers {
		out[k] = v
	}
	return out
}

// inboundTagForTier 根据 tier 名选 inbound tag。
// - 兼容模式（tiers 为空）或 tier=""：返回 s.inboundTag
// - tier 在 tiers 里：返回对应的 inboundTag
// - tier 未知：fallback 到 defaultTier 的 inboundTag，再 fallback 到 s.inboundTag
// 调用方必须持有 s.mu，或接受快照在调用期间被改动的风险。
// 为简化：内部先取快照再用。
func (s *XrayUserSync) inboundTagForTier(tier string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inboundTagForTierLocked(tier)
}

// inboundTagForTierLocked 同 inboundTagForTier，调用方已持 s.mu。
func (s *XrayUserSync) inboundTagForTierLocked(tier string) string {
	if tier == "" {
		return s.inboundTag
	}
	if t, ok := s.tiers[tier]; ok {
		return t.InboundTag
	}
	if t, ok := s.tiers[s.defaultTier]; ok {
		return t.InboundTag
	}
	return s.inboundTag
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
