//go:build xray

package xray

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ---- buildProtocolUser ----

func TestBuildProtocolUser_ValidUUID(t *testing.T) {
	u := &User{ID: "aaaa0000-0000-0000-0000-000000000001", UUID: "aaaa0000-0000-0000-0000-000000000001", Flow: "xtls-rprx-vision"}
	pu, err := buildProtocolUser(u)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pu.Email != "xray@aaaa0000-0000-0000-0000-000000000001" {
		t.Errorf("Email = %q, want %q", pu.Email, "xray@aaaa0000-0000-0000-0000-000000000001")
	}
	if pu.Account == nil {
		t.Error("Account should not be nil")
	}
}

func TestBuildProtocolUser_InvalidUUID(t *testing.T) {
	u := &User{ID: "not-a-uuid", UUID: "not-a-uuid"}
	_, err := buildProtocolUser(u)
	if err == nil {
		t.Fatal("expected error for invalid UUID")
	}
	if !strings.Contains(err.Error(), "invalid uuid") {
		t.Errorf("error %q should mention 'invalid uuid'", err.Error())
	}
}

func TestBuildProtocolUser_EmailUsesID(t *testing.T) {
	// Email 用 ID 字段构建，不用 UUID（两者可能不同）
	u := &User{ID: "id-field", UUID: "bbbb0000-0000-0000-0000-000000000002"}
	pu, err := buildProtocolUser(u)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pu.Email != "xray@id-field" {
		t.Errorf("Email = %q, should be derived from ID", pu.Email)
	}
}

// ---- isXrayAlreadyExists ----

func TestIsXrayAlreadyExists_AlreadyExistsCode(t *testing.T) {
	err := status.Error(codes.AlreadyExists, "user already exists")
	if !isXrayAlreadyExists(err) {
		t.Error("expected true for AlreadyExists code")
	}
}

func TestIsXrayAlreadyExists_MessageContainsAlreadyExists(t *testing.T) {
	err := status.Error(codes.Internal, "operation failed: already exists in list")
	if !isXrayAlreadyExists(err) {
		t.Error("expected true when message contains 'already exists'")
	}
}

func TestIsXrayAlreadyExists_MessageContainsDuplicate(t *testing.T) {
	err := status.Error(codes.Internal, "duplicate entry")
	if !isXrayAlreadyExists(err) {
		t.Error("expected true when message contains 'duplicate'")
	}
}

func TestIsXrayAlreadyExists_UnrelatedError(t *testing.T) {
	err := status.Error(codes.Internal, "something went wrong")
	if isXrayAlreadyExists(err) {
		t.Error("expected false for unrelated error")
	}
}

func TestIsXrayAlreadyExists_NonGRPCError(t *testing.T) {
	err := &customErr{"some plain error"}
	if isXrayAlreadyExists(err) {
		t.Error("expected false for non-gRPC error")
	}
}

// ---- isXrayNotFound ----

func TestIsXrayNotFound_NotFoundCode(t *testing.T) {
	err := status.Error(codes.NotFound, "user not found")
	if !isXrayNotFound(err) {
		t.Error("expected true for NotFound code")
	}
}

func TestIsXrayNotFound_MessageContainsNotFound(t *testing.T) {
	err := status.Error(codes.Internal, "operation failed: not found in list")
	if !isXrayNotFound(err) {
		t.Error("expected true when message contains 'not found'")
	}
}

func TestIsXrayNotFound_UnrelatedError(t *testing.T) {
	err := status.Error(codes.Internal, "permission denied")
	if isXrayNotFound(err) {
		t.Error("expected false for unrelated error")
	}
}

func TestIsXrayNotFound_NonGRPCError(t *testing.T) {
	err := &customErr{"some plain error"}
	if isXrayNotFound(err) {
		t.Error("expected false for non-gRPC error")
	}
}

// ---- xrayEmail ----

func TestXrayEmail(t *testing.T) {
	if got := xrayEmail("my-id"); got != "xray@my-id" {
		t.Errorf("xrayEmail(%q) = %q, want %q", "my-id", got, "xray@my-id")
	}
}

// ---- helpers ----

type customErr struct{ msg string }

func (e *customErr) Error() string { return e.msg }
