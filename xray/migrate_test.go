//go:build xray

package xray

import "testing"

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
