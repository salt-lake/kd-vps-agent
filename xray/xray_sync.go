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

	// xray vless reality 固定 flow，写 config 和 gRPC 注入都用这一个值
	flowVision = "xtls-rprx-vision"

	// 固定测试用户的 email，config 里与 defaultUUID 成对出现
	defaultUserEmail = "default@test"

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

// MigrateReporter 供 MigrateToTiers 回报迁移结果。
// success=true 时 errMsg 应为空；后端据此翻转 tb_node.xray_tier_migrated。
type MigrateReporter func(success bool, errMsg string)

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
	ratelimit           TCApplier       // 由外部注入，nil 时不应用 tc
	reporter            MigrateReporter // 由外部注入，nil 时迁移不上报（单元测试场景）
	restartSyncInFlight int32           // atomic: 1 if syncAfterRestart goroutine is running

	// startupReady 在首次 StartupSync 成功（tiers 已从后端拉到、用户已注入）后被关闭。
	// 其它 goroutine（如 tempSync.Start）必须先 <-s.StartupReady() 才能安全 AddUser，
	// 否则在迁移后的节点上空 tier 会路由到不存在的老 inbound。
	startupReady chan struct{}
	startupOnce  sync.Once
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

// SetMigrateReporter 注入迁移回报函数（publish NATS）。nil 时迁移完只记日志。
func (s *XrayUserSync) SetMigrateReporter(r MigrateReporter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reporter = r
}

func NewXrayUserSync(apiBase, token, apiAddr, inboundTag, configPath string) *XrayUserSync {
	return &XrayUserSync{
		apiBase:      apiBase,
		token:        token,
		apiAddr:      apiAddr,
		inboundTag:   inboundTag,
		configPath:   configPath,
		current:      make(map[string]string),
		tiers:        make(map[string]TierConfig),
		startupReady: make(chan struct{}),
	}
}

// StartupReady 返回一个 channel：首次 StartupSync 成功后被关闭。
// TempUserSync 等依赖方可以 select 它来确保 tiers 缓存已填充后再 AddUser。
func (s *XrayUserSync) StartupReady() <-chan struct{} {
	return s.startupReady
}

// signalStartupReady 在首次 StartupSync 成功时调用，幂等。
func (s *XrayUserSync) signalStartupReady() {
	s.startupOnce.Do(func() { close(s.startupReady) })
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
// 决策顺序：
//  1. 兼容模式（tiers 字典为空）：返回 s.inboundTag（老的单 inbound）
//  2. 多 tier 模式下 tier 能匹配：返回该 tier 的 inboundTag
//  3. 多 tier 模式下 tier 匹配不到（空或未知）：fallback 到 defaultTier 的 inboundTag
//  4. 最后兜底 s.inboundTag
// 关键修正：一旦 tiers 非空（迁移后），就不再用 s.inboundTag，避免往已被移除的老
// "proxy" inbound 发包（temp_sync 用 tier="" 调用会踩这个坑）。
func (s *XrayUserSync) inboundTagForTier(tier string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inboundTagForTierLocked(tier)
}

// inboundTagForTierLocked 同 inboundTagForTier，调用方已持 s.mu。
func (s *XrayUserSync) inboundTagForTierLocked(tier string) string {
	// 兼容模式：未配置任何 tier，走老单 inbound
	if len(s.tiers) == 0 {
		return s.inboundTag
	}
	// 多 tier 模式：精确匹配优先
	if tier != "" {
		if t, ok := s.tiers[tier]; ok {
			return t.InboundTag
		}
	}
	// tier 为空或未知：回退到 defaultTier 的 inboundTag
	if s.defaultTier != "" {
		if t, ok := s.tiers[s.defaultTier]; ok {
			return t.InboundTag
		}
	}
	// 多 tier 模式下 defaultTier 也没命中（异常情况）：任挑一个存在的 tier，
	// 不返回 s.inboundTag（老的 "proxy" 迁移后已经不存在了）。
	for _, t := range s.tiers {
		return t.InboundTag
	}
	// 真的没 tier 就只能兜底
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
