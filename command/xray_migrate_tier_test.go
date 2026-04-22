//go:build xray

package command

import (
	"encoding/json"
	"errors"
	"testing"
)

type mockTierMigrator struct {
	migrateCalled bool
	migrateErr    error
	gotPayload    []byte
}

func (m *mockTierMigrator) MigrateToTiers(payload []byte) error {
	m.migrateCalled = true
	m.gotPayload = payload
	return m.migrateErr
}

func TestXrayMigrateTier_Success(t *testing.T) {
	m := &mockTierMigrator{}
	h := NewXrayMigrateTierHandler(m)

	payload, _ := json.Marshal(map[string]any{
		"tiers": map[string]any{
			"vip":  map[string]any{"markId": 1, "inboundTag": "proxy-vip", "portRange": "34521-34524", "poolMbps": 100},
			"svip": map[string]any{"markId": 2, "inboundTag": "proxy-svip", "portRange": "45000-45003", "poolMbps": 500},
		},
		"defaultTier":     "vip",
		"migrateExisting": true,
	})
	out, err := h.Handle(payload)
	if err != nil {
		t.Fatal(err)
	}
	ok, _ := respBody(t, out)
	if !ok {
		t.Errorf("expected ok, got err resp: %s", out)
	}
	if !m.migrateCalled {
		t.Error("MigrateToTiers not called")
	}
}

func TestXrayMigrateTier_InvalidPayload(t *testing.T) {
	m := &mockTierMigrator{}
	h := NewXrayMigrateTierHandler(m)
	out, err := h.Handle([]byte("not json"))
	if err != nil {
		t.Fatal(err)
	}
	ok, _ := respBody(t, out)
	if ok {
		t.Error("expected err resp for invalid payload")
	}
	if m.migrateCalled {
		t.Error("should not call migrator on bad payload")
	}
}

func TestXrayMigrateTier_MigratorError(t *testing.T) {
	m := &mockTierMigrator{migrateErr: errors.New("boom")}
	h := NewXrayMigrateTierHandler(m)

	payload, _ := json.Marshal(map[string]any{
		"tiers":       map[string]any{"vip": map[string]any{"markId": 1, "inboundTag": "proxy-vip", "portRange": "34521-34524", "poolMbps": 100}},
		"defaultTier": "vip",
	})
	out, _ := h.Handle(payload)
	ok, msg := respBody(t, out)
	if ok || msg != "boom" {
		t.Errorf("expected err resp with 'boom', got ok=%v msg=%s", ok, msg)
	}
}

func TestXrayMigrateTier_NilMigrator(t *testing.T) {
	h := NewXrayMigrateTierHandler(nil)
	out, err := h.Handle([]byte(`{"tiers":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	ok, _ := respBody(t, out)
	if ok {
		t.Error("expected err resp when migrator is nil")
	}
}

func TestXrayMigrateTier_Name(t *testing.T) {
	h := NewXrayMigrateTierHandler(nil)
	if h.Name() != "xray_migrate_tier" {
		t.Errorf("Name() = %s, want xray_migrate_tier", h.Name())
	}
}
