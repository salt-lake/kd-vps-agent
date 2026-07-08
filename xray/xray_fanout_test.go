//go:build xray

package xray

import (
	"testing"

	hyacct "github.com/salt-lake/kd-vps-agent/xray/proto/proxy/hysteria/account"
	"github.com/salt-lake/kd-vps-agent/xray/proto/proxy/vless"
	"google.golang.org/protobuf/proto"
)

func TestBuildProtocolUser_PerProtocol(t *testing.T) {
	u := &User{ID: "n1", UUID: "a1b2c3d4-0000-0000-0000-000000000009", Flow: "xtls-rprx-vision"}

	pv, err := buildProtocolUser(u, "vless")
	if err != nil {
		t.Fatal(err)
	}
	if pv.Account.Type != "xray.proxy.vless.Account" {
		t.Fatalf("vless type = %q", pv.Account.Type)
	}
	var va vless.Account
	_ = proto.Unmarshal(pv.Account.Value, &va)
	if va.Id != u.UUID || va.Flow != u.Flow {
		t.Fatalf("vless acct = %+v", &va)
	}

	ph, err := buildProtocolUser(u, "hysteria")
	if err != nil {
		t.Fatal(err)
	}
	if ph.Account.Type != "xray.proxy.hysteria.account.Account" {
		t.Fatalf("hy2 type = %q", ph.Account.Type)
	}
	var ha hyacct.Account
	_ = proto.Unmarshal(ph.Account.Value, &ha)
	if ha.Auth != u.UUID {
		t.Fatalf("hy2 auth = %q, want %q", ha.Auth, u.UUID)
	}
	if pv.Email != xrayEmail(u.ID) || pv.Email != ph.Email {
		t.Fatalf("email mismatch v=%q h=%q", pv.Email, ph.Email)
	}
}
