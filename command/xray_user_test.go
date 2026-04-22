//go:build xray

package command

import (
	"encoding/json"
	"errors"
	"testing"
)

// mockSyncer 实现 XrayUserManager 接口，用于测试。
type mockSyncer struct {
	addErr       error
	removeErr    error
	addCalled    string
	addTier      string
	removeCalled string
}

func (m *mockSyncer) AddUser(uuid, tier string) error {
	m.addCalled = uuid
	m.addTier = tier
	return m.addErr
}

func (m *mockSyncer) RemoveUser(uuid string) error {
	m.removeCalled = uuid
	return m.removeErr
}

// respBody 解析 okResp / errResp 的 JSON 结构。
func respBody(t *testing.T, b []byte) (ok bool, msg string) {
	t.Helper()
	var v struct {
		Ok  bool   `json:"ok"`
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal resp: %v", err)
	}
	return v.Ok, v.Msg
}

// ---- XrayUserAddHandler ----

func TestXrayUserAddHandler_Name(t *testing.T) {
	h := NewXrayUserAddHandler(&mockSyncer{})
	if h.Name() != "xray_user_add" {
		t.Errorf("Name() = %q, want %q", h.Name(), "xray_user_add")
	}
}

func TestXrayUserAddHandler_NilSyncer(t *testing.T) {
	h := NewXrayUserAddHandler(nil)
	got, err := h.Handle([]byte(`{"uuid":"abc"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ok, _ := respBody(t, got)
	if ok {
		t.Error("expected ok=false for nil syncer")
	}
}

func TestXrayUserAddHandler_InvalidJSON(t *testing.T) {
	h := NewXrayUserAddHandler(&mockSyncer{})
	got, err := h.Handle([]byte(`not-json`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ok, _ := respBody(t, got)
	if ok {
		t.Error("expected ok=false for invalid JSON")
	}
}

func TestXrayUserAddHandler_MissingUUID(t *testing.T) {
	h := NewXrayUserAddHandler(&mockSyncer{})
	got, err := h.Handle([]byte(`{"uuid":""}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ok, _ := respBody(t, got)
	if ok {
		t.Error("expected ok=false for missing uuid")
	}
}

func TestXrayUserAddHandler_Success(t *testing.T) {
	m := &mockSyncer{}
	h := NewXrayUserAddHandler(m)
	got, err := h.Handle([]byte(`{"uuid":"test-uuid-123"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ok, _ := respBody(t, got)
	if !ok {
		t.Error("expected ok=true")
	}
	if m.addCalled != "test-uuid-123" {
		t.Errorf("AddUser called with %q, want %q", m.addCalled, "test-uuid-123")
	}
}

func TestXrayUserAddHandler_SyncerError(t *testing.T) {
	m := &mockSyncer{addErr: errors.New("grpc failed")}
	h := NewXrayUserAddHandler(m)
	got, err := h.Handle([]byte(`{"uuid":"test-uuid-123"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ok, msg := respBody(t, got)
	if ok {
		t.Error("expected ok=false on syncer error")
	}
	if msg == "" {
		t.Error("expected non-empty error message")
	}
}

// ---- XrayUserRemoveHandler ----

func TestXrayUserRemoveHandler_Name(t *testing.T) {
	h := NewXrayUserRemoveHandler(&mockSyncer{})
	if h.Name() != "xray_user_remove" {
		t.Errorf("Name() = %q, want %q", h.Name(), "xray_user_remove")
	}
}

func TestXrayUserRemoveHandler_NilSyncer(t *testing.T) {
	h := NewXrayUserRemoveHandler(nil)
	got, err := h.Handle([]byte(`{"uuid":"abc"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ok, _ := respBody(t, got)
	if ok {
		t.Error("expected ok=false for nil syncer")
	}
}

func TestXrayUserRemoveHandler_InvalidJSON(t *testing.T) {
	h := NewXrayUserRemoveHandler(&mockSyncer{})
	got, err := h.Handle([]byte(`{bad json}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ok, _ := respBody(t, got)
	if ok {
		t.Error("expected ok=false for invalid JSON")
	}
}

func TestXrayUserRemoveHandler_MissingUUID(t *testing.T) {
	h := NewXrayUserRemoveHandler(&mockSyncer{})
	got, err := h.Handle([]byte(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ok, _ := respBody(t, got)
	if ok {
		t.Error("expected ok=false for missing uuid")
	}
}

func TestXrayUserRemoveHandler_Success(t *testing.T) {
	m := &mockSyncer{}
	h := NewXrayUserRemoveHandler(m)
	got, err := h.Handle([]byte(`{"uuid":"rm-uuid-456"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ok, _ := respBody(t, got)
	if !ok {
		t.Error("expected ok=true")
	}
	if m.removeCalled != "rm-uuid-456" {
		t.Errorf("RemoveUser called with %q, want %q", m.removeCalled, "rm-uuid-456")
	}
}

func TestXrayUserRemoveHandler_SyncerError(t *testing.T) {
	m := &mockSyncer{removeErr: errors.New("remove failed")}
	h := NewXrayUserRemoveHandler(m)
	got, err := h.Handle([]byte(`{"uuid":"rm-uuid-456"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ok, _ := respBody(t, got)
	if ok {
		t.Error("expected ok=false on syncer error")
	}
}
