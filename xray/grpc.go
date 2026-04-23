//go:build xray

package xray

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// getAPI 获取或创建 xray gRPC API 客户端（长连接复用）。
func (s *XrayUserSync) getAPI() (XrayAPI, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.xrayAPI != nil {
		return s.xrayAPI, nil
	}

	api, err := NewGRPCXrayAPI(s.apiAddr, s.inboundTag)
	if err != nil {
		return nil, err
	}
	s.xrayAPI = api
	return s.xrayAPI, nil
}

// injectUsers 按 tier 分组注入用户到对应 inbound。
// 兼容模式（s.tiers 为空）下全部注入到 s.inboundTag。
// 初始化完成后覆盖 current（uuid → tier）。
func (s *XrayUserSync) injectUsers(users []userDTO) error {
	api, err := s.getAPI()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if !api.IsXrayReady(ctx) {
		return fmt.Errorf("xray gRPC not ready")
	}

	for _, u := range users {
		if u.UUID == defaultUUID {
			continue
		}
		inbound := s.inboundTagForTier(u.Tier)
		if err := api.AddOrReplaceToTag(ctx, inbound, &User{ID: u.UUID, UUID: u.UUID, Flow: flowVision}); err != nil {
			return fmt.Errorf("inject user %s tier=%s to %s: %w", u.UUID, u.Tier, inbound, err)
		}
	}

	s.mu.Lock()
	s.current = make(map[string]string, len(users))
	for _, u := range users {
		s.current[u.UUID] = u.Tier
	}
	s.mu.Unlock()
	return nil
}

// AddUser 按 tier 注入用户到对应 inbound。兼容模式下 tier 传 ""。
func (s *XrayUserSync) AddUser(uuid, tier string) error {
	api, err := s.getAPI()
	if err != nil {
		return fmt.Errorf("get api: %w", err)
	}

	inbound := s.inboundTagForTier(tier)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := api.AddOrReplaceToTag(ctx, inbound, &User{ID: uuid, UUID: uuid, Flow: flowVision}); err != nil {
		if strings.Contains(err.Error(), "connection") || strings.Contains(err.Error(), "unavailable") {
			s.mu.Lock()
			if s.xrayAPI == api {
				_ = s.xrayAPI.Close()
				s.xrayAPI = nil
			}
			s.mu.Unlock()
		}
		return fmt.Errorf("AddUser uuid=%s tier=%s: %w", uuid, tier, err)
	}
	s.mu.Lock()
	s.current[uuid] = tier
	s.mu.Unlock()
	return nil
}

// RemoveUser 按 current 记录的 tier 定位 inbound 移除用户。
// 找不到记录时走兼容路径（s.inboundTag）。
func (s *XrayUserSync) RemoveUser(uuid string) error {
	api, err := s.getAPI()
	if err != nil {
		return fmt.Errorf("get api: %w", err)
	}

	s.mu.Lock()
	tier := s.current[uuid]
	inbound := s.inboundTagForTierLocked(tier)
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := api.RemoveUserFromTag(ctx, inbound, uuid); err != nil {
		if strings.Contains(err.Error(), "connection") || strings.Contains(err.Error(), "unavailable") {
			s.mu.Lock()
			if s.xrayAPI == api {
				_ = s.xrayAPI.Close()
				s.xrayAPI = nil
			}
			s.mu.Unlock()
		}
		return fmt.Errorf("RemoveUser uuid=%s tier=%s: %w", uuid, tier, err)
	}
	s.mu.Lock()
	delete(s.current, uuid)
	s.mu.Unlock()
	return nil
}
