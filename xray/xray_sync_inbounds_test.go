//go:build xray

package xray

import "testing"

func TestBuildInboundSpecs(t *testing.T) {
	got := buildInboundSpecs("proxy", "hysteria")
	if len(got) != 2 || got[0] != (InboundSpec{"proxy", "vless"}) || got[1] != (InboundSpec{"hysteria", "hysteria"}) {
		t.Fatalf("with hy2 = %+v", got)
	}
	only := buildInboundSpecs("proxy", "")
	if len(only) != 1 || only[0] != (InboundSpec{"proxy", "vless"}) {
		t.Fatalf("without hy2 = %+v", only)
	}
}
