//go:build xray

package xray

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"
)

// mockManager 实现 userManager 接口，记录调用顺序。
type mockManager struct {
	mu       sync.Mutex
	added    []string
	removed  []string
	addErr   map[string]error // uuid -> error，nil 表示成功
	removeErr map[string]error
}

func newMockManager() *mockManager {
	return &mockManager{
		addErr:    make(map[string]error),
		removeErr: make(map[string]error),
	}
}

func (m *mockManager) AddUser(uuid, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.added = append(m.added, uuid)
	return m.addErr[uuid]
}

func (m *mockManager) RemoveUser(uuid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, uuid)
	return m.removeErr[uuid]
}

func (m *mockManager) allAdded() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.added))
	copy(out, m.added)
	return out
}

func (m *mockManager) allRemoved() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.removed))
	copy(out, m.removed)
	return out
}

// tempServer 启动一个假的 HTTP 服务器，返回固定的 temp-users 响应。
func tempServer(version string, uuids []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/temp-users" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(tempUsersResp{
			Code: 200,
			Data: tempUsersData{Version: version, UUIDs: uuids},
		})
	}))
}

// ---- fetchTempUsers ----

func TestFetchTempUsers_OK(t *testing.T) {
	srv := tempServer("v1", []string{"uuid-a", "uuid-b"})
	defer srv.Close()

	ver, uuids, err := fetchTempUsers(srv.URL, "token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ver != "v1" {
		t.Errorf("version = %q, want %q", ver, "v1")
	}
	if len(uuids) != 2 {
		t.Errorf("len(uuids) = %d, want 2", len(uuids))
	}
}

func TestFetchTempUsers_NonOKCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(tempUsersResp{Code: 500})
	}))
	defer srv.Close()

	_, _, err := fetchTempUsers(srv.URL, "token")
	if err == nil {
		t.Fatal("expected error for code=500")
	}
}

func TestFetchTempUsers_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not-json"))
	}))
	defer srv.Close()

	_, _, err := fetchTempUsers(srv.URL, "token")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestFetchTempUsers_ServerError(t *testing.T) {
	_, _, err := fetchTempUsers("http://127.0.0.1:1", "token")
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
}

// ---- startup ----

func TestTempUserSync_Startup_InjectsAll(t *testing.T) {
	srv := tempServer("v1", []string{"u1", "u2", "u3"})
	defer srv.Close()

	m := newMockManager()
	ts := NewTempUserSync(srv.URL, "token", m)

	if err := ts.startup(); err != nil {
		t.Fatalf("startup err: %v", err)
	}

	added := m.allAdded()
	sort.Strings(added)
	if len(added) != 3 {
		t.Errorf("expected 3 adds, got %d: %v", len(added), added)
	}

	ts.mu.RLock()
	defer ts.mu.RUnlock()
	if ts.version != "v1" {
		t.Errorf("cached version = %q, want %q", ts.version, "v1")
	}
	if len(ts.uuids) != 3 {
		t.Errorf("cached uuids len = %d, want 3", len(ts.uuids))
	}
}

func TestTempUserSync_Startup_APIError(t *testing.T) {
	ts := NewTempUserSync("http://127.0.0.1:1", "token", newMockManager())
	if err := ts.startup(); err == nil {
		t.Fatal("expected error when API unreachable")
	}
}

func TestTempUserSync_Startup_PartialAddError(t *testing.T) {
	srv := tempServer("v1", []string{"u1", "u2"})
	defer srv.Close()

	m := newMockManager()
	m.addErr["u1"] = errors.New("grpc fail")

	ts := NewTempUserSync(srv.URL, "token", m)
	// 单个错误不中断，startup 应成功
	if err := ts.startup(); err != nil {
		t.Fatalf("startup should not fail on partial add error: %v", err)
	}
	// u2 仍被注入
	added := m.allAdded()
	if len(added) != 2 {
		t.Errorf("expected 2 add attempts, got %d", len(added))
	}
	// 缓存仍被更新
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	if len(ts.uuids) != 2 {
		t.Errorf("cached uuids len = %d, want 2", len(ts.uuids))
	}
}

// ---- poll ----

func TestTempUserSync_Poll_SameVersionSkips(t *testing.T) {
	srv := tempServer("v1", []string{"u1"})
	defer srv.Close()

	m := newMockManager()
	ts := NewTempUserSync(srv.URL, "token", m)
	ts.version = "v1"
	ts.uuids = []string{"u1"}

	if err := ts.poll(); err != nil {
		t.Fatalf("poll err: %v", err)
	}
	if len(m.allAdded()) != 0 || len(m.allRemoved()) != 0 {
		t.Error("expected no add/remove when version unchanged")
	}
}

func TestTempUserSync_Poll_AddsNew(t *testing.T) {
	srv := tempServer("v2", []string{"u1", "u2"})
	defer srv.Close()

	m := newMockManager()
	ts := NewTempUserSync(srv.URL, "token", m)
	ts.version = "v1"
	ts.uuids = []string{"u1"} // u2 是新增

	if err := ts.poll(); err != nil {
		t.Fatalf("poll err: %v", err)
	}
	added := m.allAdded()
	if len(added) != 1 || added[0] != "u2" {
		t.Errorf("expected [u2] added, got %v", added)
	}
	if len(m.allRemoved()) != 0 {
		t.Errorf("expected no removes, got %v", m.allRemoved())
	}
}

func TestTempUserSync_Poll_RemovesGone(t *testing.T) {
	srv := tempServer("v2", []string{"u1"})
	defer srv.Close()

	m := newMockManager()
	ts := NewTempUserSync(srv.URL, "token", m)
	ts.version = "v1"
	ts.uuids = []string{"u1", "u2"} // u2 消失

	if err := ts.poll(); err != nil {
		t.Fatalf("poll err: %v", err)
	}
	if len(m.allAdded()) != 0 {
		t.Errorf("expected no adds, got %v", m.allAdded())
	}
	removed := m.allRemoved()
	if len(removed) != 1 || removed[0] != "u2" {
		t.Errorf("expected [u2] removed, got %v", removed)
	}
}

func TestTempUserSync_Poll_AddBeforeRemove(t *testing.T) {
	srv := tempServer("v2", []string{"u2", "u3"})
	defer srv.Close()

	type op struct{ action, uuid string }
	var ops []op
	var mu sync.Mutex

	m := &mockManager{addErr: map[string]error{}, removeErr: map[string]error{}}
	// 包装以记录顺序
	mgr := &orderManager{
		addFn: func(uuid string) error {
			mu.Lock()
			ops = append(ops, op{"add", uuid})
			mu.Unlock()
			return nil
		},
		removeFn: func(uuid string) error {
			mu.Lock()
			ops = append(ops, op{"remove", uuid})
			mu.Unlock()
			return nil
		},
	}
	_ = m

	ts := NewTempUserSync(srv.URL, "token", mgr)
	ts.version = "v1"
	ts.uuids = []string{"u1", "u2"} // u3 新增，u1 删除

	if err := ts.poll(); err != nil {
		t.Fatalf("poll err: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// 所有 add 操作应在所有 remove 操作之前
	lastAdd, firstRemove := -1, len(ops)
	for i, op := range ops {
		if op.action == "add" && i > lastAdd {
			lastAdd = i
		}
		if op.action == "remove" && i < firstRemove {
			firstRemove = i
		}
	}
	if lastAdd >= firstRemove && firstRemove < len(ops) {
		t.Errorf("remove happened before add: ops=%v", ops)
	}
}

func TestTempUserSync_Poll_UpdatesCache(t *testing.T) {
	srv := tempServer("v2", []string{"u2", "u3"})
	defer srv.Close()

	ts := NewTempUserSync(srv.URL, "token", newMockManager())
	ts.version = "v1"
	ts.uuids = []string{"u1"}

	if err := ts.poll(); err != nil {
		t.Fatalf("poll err: %v", err)
	}

	ts.mu.RLock()
	defer ts.mu.RUnlock()
	if ts.version != "v2" {
		t.Errorf("cached version = %q, want %q", ts.version, "v2")
	}
	got := append([]string{}, ts.uuids...)
	sort.Strings(got)
	if len(got) != 2 || got[0] != "u2" || got[1] != "u3" {
		t.Errorf("cached uuids = %v, want [u2 u3]", got)
	}
}

func TestTempUserSync_Poll_APIError(t *testing.T) {
	ts := NewTempUserSync("http://127.0.0.1:1", "token", newMockManager())
	ts.version = "v1"
	ts.uuids = []string{"u1"}

	if err := ts.poll(); err == nil {
		t.Fatal("expected error when API unreachable")
	}
	// 缓存不变
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	if ts.version != "v1" {
		t.Error("cache should not change on poll error")
	}
}

// ---- ReInjectAll ----

func TestTempUserSync_ReInjectAll_Empty(t *testing.T) {
	m := newMockManager()
	ts := NewTempUserSync("", "", m)
	ts.ReInjectAll() // 空缓存，不应 panic
	if len(m.allAdded()) != 0 {
		t.Error("expected no adds for empty cache")
	}
}

func TestTempUserSync_ReInjectAll_InjectsAll(t *testing.T) {
	m := newMockManager()
	ts := NewTempUserSync("", "", m)
	ts.uuids = []string{"u1", "u2", "u3"}

	ts.ReInjectAll()

	added := m.allAdded()
	sort.Strings(added)
	if len(added) != 3 {
		t.Errorf("expected 3 adds, got %d: %v", len(added), added)
	}
}

func TestTempUserSync_ReInjectAll_PartialError(t *testing.T) {
	m := newMockManager()
	m.addErr["u2"] = errors.New("fail")
	ts := NewTempUserSync("", "", m)
	ts.uuids = []string{"u1", "u2", "u3"}

	ts.ReInjectAll() // 单个失败不 panic，其余继续
	if len(m.allAdded()) != 3 {
		t.Errorf("expected 3 add attempts, got %d", len(m.allAdded()))
	}
}

// ---- Start（集成验证）----

func TestTempUserSync_Start_StartsAndPolls(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		// 第一次返回 v1，后续返回 v2 触发一次 poll diff
		ver := "v1"
		if callCount > 1 {
			ver = "v2"
		}
		_ = json.NewEncoder(w).Encode(tempUsersResp{
			Code: 200,
			Data: tempUsersData{Version: ver, UUIDs: []string{"u1"}},
		})
	}))
	defer srv.Close()

	m := newMockManager()
	ts := &TempUserSync{
		apiBase: srv.URL,
		token:   "tok",
		manager: m,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ts.Start(ctx)

	// 等待 startup 完成
	time.Sleep(200 * time.Millisecond)
	if len(m.allAdded()) == 0 {
		t.Error("expected at least one AddUser call after startup")
	}
}

// ---- 辅助 ----

// orderManager 用于记录 add/remove 的调用顺序。
type orderManager struct {
	addFn    func(string) error
	removeFn func(string) error
}

func (o *orderManager) AddUser(uuid, _ string) error { return o.addFn(uuid) }
func (o *orderManager) RemoveUser(uuid string) error { return o.removeFn(uuid) }
