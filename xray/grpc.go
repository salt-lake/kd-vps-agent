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

// injectUsers 批量将用户注入 Xray 内存并初始化 current 状态。
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
		if err := api.AddOrReplace(ctx, &User{ID: u.UUID, UUID: u.UUID, Flow: "xtls-rprx-vision"}); err != nil {
			return fmt.Errorf("inject user %s failed: %w", u.UUID, err)
		}
	}

	s.mu.Lock()
	s.current = make(map[string]struct{}, len(users))
	for _, u := range users {
		s.current[u.UUID] = struct{}{}
	}
	s.mu.Unlock()
	return nil
}

// AddUser 通过 xray gRPC API 动态添加用户。
func (s *XrayUserSync) AddUser(uuid string) error {
	api, err := s.getAPI()
	if err != nil {
		return fmt.Errorf("get api: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := api.AddOrReplace(ctx, &User{ID: uuid, UUID: uuid, Flow: "xtls-rprx-vision"}); err != nil {
		if strings.Contains(err.Error(), "connection") || strings.Contains(err.Error(), "unavailable") {
			s.mu.Lock()
			s.xrayAPI = nil
			s.mu.Unlock()
		}
		return fmt.Errorf("AddUser uuid=%s: %w", uuid, err)
	}
	s.mu.Lock()
	s.current[uuid] = struct{}{}
	s.mu.Unlock()
	return nil
}

// RemoveUser 通过 xray gRPC API 动态移除用户。
func (s *XrayUserSync) RemoveUser(uuid string) error {
	api, err := s.getAPI()
	if err != nil {
		return fmt.Errorf("get api: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := api.RemoveUserById(ctx, uuid); err != nil {
		if strings.Contains(err.Error(), "connection") || strings.Contains(err.Error(), "unavailable") {
			s.mu.Lock()
			s.xrayAPI = nil
			s.mu.Unlock()
		}
		return fmt.Errorf("RemoveUser uuid=%s: %w", uuid, err)
	}
	s.mu.Lock()
	delete(s.current, uuid)
	s.mu.Unlock()
	return nil
}
