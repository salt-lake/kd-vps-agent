//go:build xray

package xray

import (
	"os"
	"testing"
)

func TestMigrateToTiers_InvalidJSON(t *testing.T) {
	s := &XrayUserSync{}
	err := s.MigrateToTiers([]byte("not json"))
	if err == nil {
		t.Error("expected error on invalid json")
	}
}

func TestMigrateToTiers_EmptyTiers(t *testing.T) {
	s := &XrayUserSync{}
	err := s.MigrateToTiers([]byte(`{"defaultTier":"vip","tiers":{}}`))
	if err == nil {
		t.Error("expected error on empty tiers")
	}
}

func TestMigrateToTiers_DefaultTierNotInTiers(t *testing.T) {
	s := &XrayUserSync{}
	payload := `{"defaultTier":"gold","tiers":{"vip":{"markId":1,"inboundTag":"proxy-vip","portRange":"443","poolMbps":100}}}`
	err := s.MigrateToTiers([]byte(payload))
	if err == nil {
		t.Error("expected error when defaultTier not in tiers")
	}
}

// reporter 在错误路径被调用，payload 包含 error message
func TestMigrateToTiers_ReporterCalledOnFailure(t *testing.T) {
	s := &XrayUserSync{}
	var gotSuccess bool
	var gotErr string
	called := false
	s.SetMigrateReporter(func(success bool, errMsg string) {
		called = true
		gotSuccess = success
		gotErr = errMsg
	})

	// invalid JSON → doMigrate 返回错误 → reporter 被触发
	_ = s.MigrateToTiers([]byte("not json"))

	if !called {
		t.Fatal("reporter should be called on failure")
	}
	if gotSuccess {
		t.Error("success should be false on failure")
	}
	if gotErr == "" {
		t.Error("errMsg should be non-empty")
	}
}

// reporter 未注入时不 panic
func TestMigrateToTiers_NoReporter(t *testing.T) {
	s := &XrayUserSync{}
	// 不调 SetMigrateReporter
	err := s.MigrateToTiers([]byte("not json"))
	if err == nil {
		t.Error("expected error")
	}
	// 只要不 panic 就 OK
}

// 幂等路径（config 已双 inbound）走成功 reporter
func TestMigrateToTiers_ReporterCalledOnIdempotentSuccess(t *testing.T) {
	// 用临时 config 文件，已经是"双 inbound"状态（含 proxy-vip tag）
	dir := t.TempDir()
	configPath := dir + "/config.json"
	prebuilt := []byte(`{
  "inbounds": [
    {"tag": "proxy-vip", "port": "443", "settings": {"clients": []}}
  ],
  "outbounds": [{"tag":"direct","protocol":"freedom"}],
  "routing": {"rules": []}
}`)
	if err := writeFile(configPath, prebuilt); err != nil {
		t.Fatal(err)
	}

	s := &XrayUserSync{configPath: configPath}
	var gotSuccess bool
	called := false
	s.SetMigrateReporter(func(success bool, errMsg string) {
		called = true
		gotSuccess = success
	})

	payload := `{"defaultTier":"vip","tiers":{"vip":{"markId":1,"inboundTag":"proxy-vip","portRange":"443","poolMbps":100}},"migrateExisting":true}`
	if err := s.MigrateToTiers([]byte(payload)); err != nil {
		t.Fatalf("idempotent path should succeed: %v", err)
	}

	if !called {
		t.Fatal("reporter should be called on idempotent success")
	}
	if !gotSuccess {
		t.Error("success should be true on idempotent success")
	}
}

// 用于上面测试的小工具：简单包一下 os.WriteFile，让测试代码意图清晰。
func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0644)
}
