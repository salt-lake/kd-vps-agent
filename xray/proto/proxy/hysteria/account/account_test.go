package account

import (
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestAccountDescriptorFullName(t *testing.T) {
	got := string((&Account{}).ProtoReflect().Descriptor().FullName())
	want := "xray.proxy.hysteria.account.Account"
	if got != want {
		t.Fatalf("FullName = %q, want %q", got, want)
	}
}

func TestAccountMarshalAuthField(t *testing.T) {
	b, err := proto.Marshal(&Account{Auth: "u-123"})
	if err != nil {
		t.Fatal(err)
	}
	// field 1, wire type 2 (len-delim): 0x0a, len=5, "u-123"
	want := []byte{0x0a, 0x05, 'u', '-', '1', '2', '3'}
	if string(b) != string(want) {
		t.Fatalf("wire = % x, want % x", b, want)
	}
}
