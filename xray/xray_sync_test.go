//go:build xray

package xray

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
)

// ---- mockXrayAPI ----

type mockXrayAPI struct {
	mu           sync.Mutex
	ready        bool
	added        []string // UUIDs passed to AddOrReplace
	addedTags    []string // inboundTag for each Add call（与 added 一一对应）
	removed      []string // IDs passed to RemoveUserById
	removedTags  []string // inboundTag for each Remove call（与 removed 一一对应）
	addErr       error
	removeErr    error
	closeCalled  bool
}

func (m *mockXrayAPI) IsXrayReady(_ context.Context) bool { return m.ready }

func (m *mockXrayAPI) AddOrReplace(ctx context.Context, u *User) error {
	return m.AddOrReplaceToTag(ctx, "", u)
}

func (m *mockXrayAPI) AddOrReplaceToTag(_ context.Context, tag string, u *User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.added = append(m.added, u.UUID)
	m.addedTags = append(m.addedTags, tag)
	return m.addErr
}

func (m *mockXrayAPI) AddBatch(ctx context.Context, users []*User) error {
	for _, u := range users {
		if err := m.AddOrReplace(ctx, u); err != nil {
			return err
		}
	}
	return nil
}

func (m *mockXrayAPI) RemoveUserById(ctx context.Context, id string) error {
	return m.RemoveUserFromTag(ctx, "", id)
}

func (m *mockXrayAPI) RemoveUserFromTag(_ context.Context, tag, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, id)
	m.removedTags = append(m.removedTags, tag)
	return m.removeErr
}

func (m *mockXrayAPI) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeCalled = true
	return nil
}

func (m *mockXrayAPI) allAdded() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.added))
	copy(out, m.added)
	return out
}

func (m *mockXrayAPI) allRemoved() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.removed))
	copy(out, m.removed)
	return out
}

// ---- helpers ----

// newSync 构造一个预注入 mock API 的 XrayUserSync，current 由调用方指定（tier="" 兼容模式）。
func newSync(api XrayAPI, apiBase string, current ...string) *XrayUserSync {
	s := NewXrayUserSync(apiBase, "token", "127.0.0.1:10085", "vless", "")
	s.xrayAPI = api
	for _, u := range current {
		s.current[u] = ""
	}
	return s
}

// usersServer 返回一个 httptest.Server，响应 /api/agent/xray/users（老格式）。
func usersServer(uuids []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data []userDTO
		for _, u := range uuids {
			data = append(data, userDTO{UUID: u})
		}
		_ = json.NewEncoder(w).Encode(apiResp{Code: 200, Data: data})
	}))
}

// deltaServer 返回一个 httptest.Server，响应 /api/agent/xray/users/delta（老格式，added 是 []string）。
func deltaServer(added, removed []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 200,
			"data": map[string]any{
				"added":   added,
				"removed": removed,
			},
		})
	}))
}

// writeTempStateFile 写临时 sync_state 文件，返回路径和清理函数。
func writeTempStateFile(t *testing.T, ts int64) (path string, cleanup func()) {
	t.Helper()
	f, err := os.CreateTemp("", "sync_state*.json")
	if err != nil {
		t.Fatalf("create temp state file: %v", err)
	}
	data, _ := json.Marshal(syncState{LastSyncTime: ts})
	_, _ = f.Write(data)
	f.Close()
	return f.Name(), func() { os.Remove(f.Name()) }
}

func sorted(ss []string) []string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}

// ---- diffUsers ----

func TestDiffUsers_EmptyCurrentAndRemote(t *testing.T) {
	s := newSync(nil, "")
	toAdd, toRemove, toChange := s.diffUsers(map[string]string{}, nil)
	if len(toAdd) != 0 || len(toRemove) != 0 || len(toChange) != 0 {
		t.Errorf("expected empty diff, got add=%v remove=%v change=%v", toAdd, toRemove, toChange)
	}
}

func TestDiffUsers_NewUsersInRemote(t *testing.T) {
	s := newSync(nil, "")
	remote := map[string]string{"u1": "", "u2": ""}
	toAdd, toRemove, toChange := s.diffUsers(remote, nil)
	if len(toRemove) != 0 || len(toChange) != 0 {
		t.Errorf("expected no removes/changes, got remove=%v change=%v", toRemove, toChange)
	}
	if len(toAdd) != 2 {
		t.Errorf("expected 2 adds, got %v", toAdd)
	}
}

func TestDiffUsers_GoneUsersInCurrent(t *testing.T) {
	s := newSync(nil, "", "old1", "old2")
	remote := map[string]string{}
	toAdd, toRemove, toChange := s.diffUsers(remote, nil)
	if len(toAdd) != 0 || len(toChange) != 0 {
		t.Errorf("expected no adds/changes, got add=%v change=%v", toAdd, toChange)
	}
	if len(toRemove) != 2 {
		t.Errorf("expected 2 removes, got %v", toRemove)
	}
}

func TestDiffUsers_UserInBothNoChange(t *testing.T) {
	s := newSync(nil, "", "common")
	remote := map[string]string{"common": ""}
	toAdd, toRemove, toChange := s.diffUsers(remote, nil)
	if len(toAdd) != 0 || len(toRemove) != 0 || len(toChange) != 0 {
		t.Errorf("expected empty diff, got add=%v remove=%v change=%v", toAdd, toRemove, toChange)
	}
}

func TestDiffUsers_DefaultUUIDNeverRemoved(t *testing.T) {
	s := newSync(nil, "", defaultUUID, "regular")
	remote := map[string]string{} // 两者都不在 remote
	_, toRemove, _ := s.diffUsers(remote, nil)
	for _, id := range toRemove {
		if id == defaultUUID {
			t.Errorf("defaultUUID should never appear in toRemove")
		}
	}
}

func TestDiffUsers_TempUsersExcludedFromRemove(t *testing.T) {
	s := newSync(nil, "", "temp-u1", "regular-u1")
	remote := map[string]string{} // 两者都消失
	exclude := map[string]struct{}{"temp-u1": {}}
	_, toRemove, _ := s.diffUsers(remote, exclude)
	for _, id := range toRemove {
		if id == "temp-u1" {
			t.Errorf("temp user should be excluded from toRemove")
		}
	}
	if len(toRemove) != 1 || toRemove[0] != "regular-u1" {
		t.Errorf("expected [regular-u1] in toRemove, got %v", toRemove)
	}
}

// ---- tempUserSet ----

func TestTempUserSet_NilTempSync(t *testing.T) {
	s := newSync(nil, "")
	if s.tempUserSet() != nil {
		t.Error("expected nil when no tempSync")
	}
}

func TestTempUserSet_DelegatesUUIDSet(t *testing.T) {
	srv := tempServer("v1", []string{"t1", "t2"})
	defer srv.Close()

	m := newMockManager()
	ts := NewTempUserSync(srv.URL, "token", m)
	_ = ts.startup() // 填充缓存

	s := newSync(nil, "")
	s.SetTempSync(ts)

	set := s.tempUserSet()
	if len(set) != 2 {
		t.Errorf("expected 2 temp UUIDs, got %d", len(set))
	}
}

// ---- fetchUsers ----

func TestFetchUsers_Success(t *testing.T) {
	srv := usersServer([]string{"u1", "u2", "u3"})
	defer srv.Close()

	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	s := newSync(nil, srv.URL)
	users, err := s.fetchUsers()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(users) != 3 {
		t.Errorf("expected 3 users, got %d", len(users))
	}
}

func TestFetchUsers_NonOKCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(apiResp{Code: 500})
	}))
	defer srv.Close()

	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	s := newSync(nil, srv.URL)
	_, err := s.fetchUsers()
	if err == nil {
		t.Fatal("expected error for code=500")
	}
}

func TestFetchUsers_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not-json"))
	}))
	defer srv.Close()

	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	s := newSync(nil, srv.URL)
	_, err := s.fetchUsers()
	if err == nil {
		t.Fatal("expected JSON parse error")
	}
}

func TestFetchUsers_NetworkError(t *testing.T) {
	s := newSync(nil, "http://127.0.0.1:1")
	_, err := s.fetchUsers()
	if err == nil {
		t.Fatal("expected network error")
	}
}

func TestFetchUsers_SendsAuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(apiResp{Code: 200})
	}))
	defer srv.Close()

	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	s := NewXrayUserSync(srv.URL, "my-token", "", "vless", "")
	s.xrayAPI = &mockXrayAPI{}
	_, _ = s.fetchUsers()

	if gotAuth != "Bearer my-token" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer my-token")
	}
}

// ---- fetchDelta ----

func TestFetchDelta_Success(t *testing.T) {
	srv := deltaServer([]string{"add1"}, []string{"rem1"})
	defer srv.Close()

	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	s := newSync(nil, srv.URL)
	delta, err := s.fetchDelta(0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(delta.Added) != 1 || delta.Added[0].UUID != "add1" {
		t.Errorf("added = %v, want [add1]", delta.Added)
	}
	if len(delta.Removed) != 1 || delta.Removed[0] != "rem1" {
		t.Errorf("removed = %v, want [rem1]", delta.Removed)
	}
}

func TestFetchDelta_NonOKCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(deltaResp{Code: 403})
	}))
	defer srv.Close()

	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	s := newSync(nil, srv.URL)
	_, err := s.fetchDelta(0)
	if err == nil {
		t.Fatal("expected error for code=403")
	}
}

func TestFetchDelta_NetworkError(t *testing.T) {
	s := newSync(nil, "http://127.0.0.1:1")
	_, err := s.fetchDelta(0)
	if err == nil {
		t.Fatal("expected network error")
	}
}

// ---- writeConfig ----

// minimalConfig 生成含一个 vless inbound 的最小 xray 配置文件。
func minimalConfig(tag string, existingClients []map[string]string) []byte {
	clientsJSON, _ := json.Marshal(existingClients)
	config := map[string]interface{}{
		"inbounds": []map[string]interface{}{
			{
				"tag": tag,
				"settings": map[string]json.RawMessage{
					"clients": clientsJSON,
				},
			},
		},
	}
	data, _ := json.Marshal(config)
	return data
}

func TestWriteConfig_UpdatesClients(t *testing.T) {
	f, err := os.CreateTemp("", "xray_config*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	_, _ = f.Write(minimalConfig("vless", nil))
	f.Close()

	s := NewXrayUserSync("", "", "", "vless", f.Name())
	users := []userDTO{{UUID: "aaaa0000-0000-0000-0000-000000000001"}}
	if err := s.writeConfig(users); err != nil {
		t.Fatalf("writeConfig err: %v", err)
	}

	// 读回，验证 clients 被写入
	data, _ := os.ReadFile(f.Name())
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(data, &raw)
	var inbounds []map[string]json.RawMessage
	_ = json.Unmarshal(raw["inbounds"], &inbounds)

	var settings map[string]json.RawMessage
	_ = json.Unmarshal(inbounds[0]["settings"], &settings)
	var clients []map[string]string
	_ = json.Unmarshal(settings["clients"], &clients)

	// 应包含 defaultUUID + 传入的 user
	ids := make(map[string]bool)
	for _, c := range clients {
		ids[c["id"]] = true
	}
	if !ids[defaultUUID] {
		t.Error("defaultUUID should always be in clients")
	}
	if !ids["aaaa0000-0000-0000-0000-000000000001"] {
		t.Error("user UUID should be in clients")
	}
}

func TestWriteConfig_DefaultUUIDNotDuplicated(t *testing.T) {
	f, err := os.CreateTemp("", "xray_config*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	_, _ = f.Write(minimalConfig("vless", nil))
	f.Close()

	s := NewXrayUserSync("", "", "", "vless", f.Name())
	// 传入包含 defaultUUID 的用户列表
	users := []userDTO{{UUID: defaultUUID}}
	if err := s.writeConfig(users); err != nil {
		t.Fatalf("writeConfig err: %v", err)
	}

	data, _ := os.ReadFile(f.Name())
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(data, &raw)
	var inbounds []map[string]json.RawMessage
	_ = json.Unmarshal(raw["inbounds"], &inbounds)
	var settings map[string]json.RawMessage
	_ = json.Unmarshal(inbounds[0]["settings"], &settings)
	var clients []map[string]string
	_ = json.Unmarshal(settings["clients"], &clients)

	count := 0
	for _, c := range clients {
		if c["id"] == defaultUUID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("defaultUUID appears %d times, want exactly 1", count)
	}
}

func TestWriteConfig_PreservesOtherInbounds(t *testing.T) {
	config := map[string]interface{}{
		"inbounds": []map[string]interface{}{
			{"tag": "other-inbound"},
			{"tag": "vless", "settings": map[string]interface{}{"clients": []interface{}{}}},
		},
	}
	data, _ := json.Marshal(config)

	f, err := os.CreateTemp("", "xray_config*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	_, _ = f.Write(data)
	f.Close()

	s := NewXrayUserSync("", "", "", "vless", f.Name())
	if err := s.writeConfig(nil); err != nil {
		t.Fatalf("writeConfig err: %v", err)
	}

	out, _ := os.ReadFile(f.Name())
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(out, &raw)
	var inbounds []map[string]json.RawMessage
	_ = json.Unmarshal(raw["inbounds"], &inbounds)
	if len(inbounds) != 2 {
		t.Errorf("expected 2 inbounds preserved, got %d", len(inbounds))
	}
}

func TestWriteConfig_MissingFile(t *testing.T) {
	s := NewXrayUserSync("", "", "", "vless", "/nonexistent/config.json")
	if err := s.writeConfig(nil); err == nil {
		t.Fatal("expected error for missing config file")
	}
}

// ---- Close ----

func TestClose_NilAPI(t *testing.T) {
	s := newSync(nil, "")
	if err := s.Close(); err != nil {
		t.Errorf("expected nil error for nil xrayAPI, got %v", err)
	}
}

func TestClose_CallsAPIClose(t *testing.T) {
	mock := &mockXrayAPI{}
	s := newSync(mock, "")
	if err := s.Close(); err != nil {
		t.Fatalf("Close err: %v", err)
	}
	if !mock.closeCalled {
		t.Error("expected Close() to be called on xrayAPI")
	}
	if s.xrayAPI != nil {
		t.Error("expected xrayAPI to be nil after Close")
	}
}

// ---- AddUser ----

func TestAddUser_Success(t *testing.T) {
	mock := &mockXrayAPI{ready: true}
	s := newSync(mock, "")
	if err := s.AddUser("aaaa0000-0000-0000-0000-000000000002", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.allAdded()) != 1 {
		t.Errorf("expected 1 add, got %d", len(mock.allAdded()))
	}
	s.mu.Lock()
	_, inCurrent := s.current["aaaa0000-0000-0000-0000-000000000002"]
	s.mu.Unlock()
	if !inCurrent {
		t.Error("expected UUID to be in current after AddUser")
	}
}

func TestAddUser_ConnectionError_ResetsAPI(t *testing.T) {
	mock := &mockXrayAPI{addErr: fmt.Errorf("connection refused")}
	s := newSync(mock, "")
	err := s.AddUser("aaaa0000-0000-0000-0000-000000000002", "")
	if err == nil {
		t.Fatal("expected error")
	}
	s.mu.Lock()
	api := s.xrayAPI
	s.mu.Unlock()
	if api != nil {
		t.Error("expected xrayAPI to be reset to nil on connection error")
	}
}

func TestAddUser_NonConnectionError_KeepsAPI(t *testing.T) {
	mock := &mockXrayAPI{addErr: fmt.Errorf("some other error")}
	s := newSync(mock, "")
	_ = s.AddUser("aaaa0000-0000-0000-0000-000000000002", "")
	s.mu.Lock()
	api := s.xrayAPI
	s.mu.Unlock()
	if api == nil {
		t.Error("expected xrayAPI to remain after non-connection error")
	}
}

// ---- RemoveUser ----

func TestRemoveUser_Success(t *testing.T) {
	mock := &mockXrayAPI{}
	s := newSync(mock, "", "aaaa0000-0000-0000-0000-000000000003")
	if err := s.RemoveUser("aaaa0000-0000-0000-0000-000000000003"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.allRemoved()) != 1 {
		t.Errorf("expected 1 remove, got %d", len(mock.allRemoved()))
	}
	s.mu.Lock()
	_, inCurrent := s.current["aaaa0000-0000-0000-0000-000000000003"]
	s.mu.Unlock()
	if inCurrent {
		t.Error("expected UUID to be removed from current after RemoveUser")
	}
}

func TestRemoveUser_ConnectionError_ResetsAPI(t *testing.T) {
	mock := &mockXrayAPI{removeErr: fmt.Errorf("transport: connection is unavailable")}
	s := newSync(mock, "", "aaaa0000-0000-0000-0000-000000000003")
	_ = s.RemoveUser("aaaa0000-0000-0000-0000-000000000003")
	s.mu.Lock()
	api := s.xrayAPI
	s.mu.Unlock()
	if api != nil {
		t.Error("expected xrayAPI to be reset to nil on connection error")
	}
}

// ---- HourlySync ----

func TestHourlySync_AddsNewUsers(t *testing.T) {
	srv := usersServer([]string{"u-new"})
	defer srv.Close()
	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	mock := &mockXrayAPI{}
	s := newSync(mock, srv.URL) // current 为空
	if err := s.HourlySync(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.allAdded()) != 1 || mock.allAdded()[0] != "u-new" {
		t.Errorf("expected [u-new] added, got %v", mock.allAdded())
	}
}

func TestHourlySync_RemovesGoneUsers(t *testing.T) {
	srv := usersServer([]string{}) // 没有用户了
	defer srv.Close()
	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	mock := &mockXrayAPI{}
	s := newSync(mock, srv.URL, "u-gone")
	if err := s.HourlySync(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.allRemoved()) != 1 || mock.allRemoved()[0] != "u-gone" {
		t.Errorf("expected [u-gone] removed, got %v", mock.allRemoved())
	}
}

func TestHourlySync_TempUsersNotRemoved(t *testing.T) {
	srv := usersServer([]string{}) // remote 为空
	defer srv.Close()
	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	// 建一个 tempSync，缓存 temp-u1
	tsSrv := tempServer("v1", []string{"temp-u1"})
	defer tsSrv.Close()
	ts := NewTempUserSync(tsSrv.URL, "tok", newMockManager())
	_ = ts.startup()

	mock := &mockXrayAPI{}
	s := newSync(mock, srv.URL, "temp-u1", "regular-gone")
	s.SetTempSync(ts)

	if err := s.HourlySync(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, id := range mock.allRemoved() {
		if id == "temp-u1" {
			t.Error("temp user should not be removed by HourlySync")
		}
	}
	if len(mock.allRemoved()) != 1 || mock.allRemoved()[0] != "regular-gone" {
		t.Errorf("expected [regular-gone] removed, got %v", mock.allRemoved())
	}
}

func TestHourlySync_FetchError(t *testing.T) {
	s := newSync(&mockXrayAPI{}, "http://127.0.0.1:1")
	if err := s.HourlySync(); err == nil {
		t.Fatal("expected error when fetch fails")
	}
}

func TestHourlySync_ContinuesOnIndividualError(t *testing.T) {
	srv := usersServer([]string{"u1", "u2"})
	defer srv.Close()
	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	// AddUser 对任意 UUID 返回错误，HourlySync 应继续而不返回 error
	mock := &mockXrayAPI{addErr: fmt.Errorf("some error")}
	s := newSync(mock, srv.URL)
	if err := s.HourlySync(); err != nil {
		t.Errorf("HourlySync should swallow individual errors, got %v", err)
	}
}

// ---- FullSync ----

func TestFullSync_AddsAndRemoves(t *testing.T) {
	srv := usersServer([]string{"u-new"})
	defer srv.Close()
	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	mock := &mockXrayAPI{}
	s := newSync(mock, srv.URL, "u-old")
	if err := s.FullSync(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.allAdded()) != 1 || mock.allAdded()[0] != "u-new" {
		t.Errorf("added = %v, want [u-new]", mock.allAdded())
	}
	if len(mock.allRemoved()) != 1 || mock.allRemoved()[0] != "u-old" {
		t.Errorf("removed = %v, want [u-old]", mock.allRemoved())
	}
}

func TestFullSync_ContinuesOnIndividualError(t *testing.T) {
	srv := usersServer([]string{"u1", "u2"})
	defer srv.Close()
	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	mock := &mockXrayAPI{addErr: fmt.Errorf("inject failed")}
	s := newSync(mock, srv.URL)
	// FullSync 对单个错误只 log，不返回
	if err := s.FullSync(); err != nil {
		t.Errorf("FullSync should swallow individual errors, got %v", err)
	}
}

// ---- DeltaSync ----

func TestDeltaSync_NoStateFile_FallsBackToHourlySync(t *testing.T) {
	srv := usersServer([]string{"u-remote"})
	defer srv.Close()
	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	// 指向不存在的 state 文件
	origStateFile := syncStateFile
	syncStateFile = "/nonexistent/sync_state.json"
	defer func() { syncStateFile = origStateFile }()

	mock := &mockXrayAPI{}
	s := newSync(mock, srv.URL)
	if err := s.DeltaSync(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 因为 loadSyncState 失败，降级为 HourlySync，应调用 AddUser
	if len(mock.allAdded()) != 1 {
		t.Errorf("expected HourlySync fallback to add u-remote, got %v", mock.allAdded())
	}
}

func TestDeltaSync_WithStateFile_AppliesDelta(t *testing.T) {
	path, cleanup := writeTempStateFile(t, 12345)
	defer cleanup()

	origStateFile := syncStateFile
	syncStateFile = path
	defer func() { syncStateFile = origStateFile }()

	srv := deltaServer([]string{"add-u"}, []string{"rem-u"})
	defer srv.Close()
	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	mock := &mockXrayAPI{}
	s := newSync(mock, srv.URL, "rem-u")
	if err := s.DeltaSync(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.allAdded()) != 1 || mock.allAdded()[0] != "add-u" {
		t.Errorf("added = %v, want [add-u]", mock.allAdded())
	}
	if len(mock.allRemoved()) != 1 || mock.allRemoved()[0] != "rem-u" {
		t.Errorf("removed = %v, want [rem-u]", mock.allRemoved())
	}
}

func TestDeltaSync_FetchDeltaError(t *testing.T) {
	path, cleanup := writeTempStateFile(t, 12345)
	defer cleanup()

	origStateFile := syncStateFile
	syncStateFile = path
	defer func() { syncStateFile = origStateFile }()

	s := newSync(&mockXrayAPI{}, "http://127.0.0.1:1")
	if err := s.DeltaSync(); err == nil {
		t.Fatal("expected error when delta fetch fails")
	}
}

func TestDeltaSync_AddError_ReturnsError(t *testing.T) {
	path, cleanup := writeTempStateFile(t, 12345)
	defer cleanup()

	origStateFile := syncStateFile
	syncStateFile = path
	defer func() { syncStateFile = origStateFile }()

	srv := deltaServer([]string{"add-u"}, nil)
	defer srv.Close()
	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	mock := &mockXrayAPI{addErr: fmt.Errorf("inject failed")}
	s := newSync(mock, srv.URL)
	// DeltaSync 对 AddUser 错误会直接返回（与 HourlySync 不同）
	if err := s.DeltaSync(); err == nil {
		t.Fatal("DeltaSync should propagate AddUser errors")
	}
}

// ---- tier 集成路径测试 ----

// withTiers 给 XrayUserSync 注入 tiers 字典和 defaultTier（测试辅助）。
func withTiers(s *XrayUserSync, defaultTier string, tiers map[string]TierConfig) *XrayUserSync {
	s.tiers = tiers
	s.defaultTier = defaultTier
	return s
}

// Gap 1: diffUsers 检测用户 tier 变化 → toChange
func TestDiffUsers_TierChange(t *testing.T) {
	s := newSync(nil, "")
	s.current = map[string]string{
		"u-upgrade": "vip",  // 要升级
		"u-stable":  "svip", // 不变
	}
	remote := map[string]string{
		"u-upgrade": "svip", // tier 变了
		"u-stable":  "svip",
		"u-new":     "vip", // 新增
	}
	toAdd, toRemove, toChange := s.diffUsers(remote, nil)

	if len(toAdd) != 1 || toAdd[0].UUID != "u-new" || toAdd[0].Tier != "vip" {
		t.Errorf("toAdd = %+v, want [{u-new vip}]", toAdd)
	}
	if len(toRemove) != 0 {
		t.Errorf("toRemove should be empty, got %v", toRemove)
	}
	if len(toChange) != 1 {
		t.Fatalf("toChange len = %d, want 1", len(toChange))
	}
	c := toChange[0]
	if c.UUID != "u-upgrade" || c.FromTier != "vip" || c.ToTier != "svip" {
		t.Errorf("toChange[0] = %+v, want {u-upgrade vip svip}", c)
	}
}

// Gap 2: fetchUsers V2 格式解析（含 tiers 字典缓存）
func TestFetchUsers_V2Format_CachesTiers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 200,
			"data": map[string]any{
				"tiers": map[string]any{
					"vip":  map[string]any{"markId": 1, "inboundTag": "proxy-vip", "poolMbps": 100},
					"svip": map[string]any{"markId": 2, "inboundTag": "proxy-svip", "poolMbps": 500},
				},
				"users": []map[string]any{
					{"uuid": "u1", "tier": "vip"},
					{"uuid": "u2", "tier": "svip"},
				},
			},
		})
	}))
	defer srv.Close()

	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	s := newSync(nil, srv.URL)
	users, err := s.fetchUsers()
	if err != nil {
		t.Fatalf("fetchUsers err: %v", err)
	}

	// 用户 tier 字段带回来
	byUUID := map[string]string{}
	for _, u := range users {
		byUUID[u.UUID] = u.Tier
	}
	if byUUID["u1"] != "vip" || byUUID["u2"] != "svip" {
		t.Errorf("users tier not parsed: %+v", byUUID)
	}

	// tiers 字典被缓存到 XrayUserSync
	cached := s.Tiers()
	if len(cached) != 2 {
		t.Fatalf("tiers cache len = %d, want 2", len(cached))
	}
	if cached["vip"].MarkID != 1 || cached["vip"].InboundTag != "proxy-vip" || cached["vip"].PoolMbps != 100 {
		t.Errorf("vip tier cached wrong: %+v", cached["vip"])
	}
	if cached["svip"].MarkID != 2 || cached["svip"].InboundTag != "proxy-svip" || cached["svip"].PoolMbps != 500 {
		t.Errorf("svip tier cached wrong: %+v", cached["svip"])
	}
}

// Gap 2b: fetchUsers 老格式 fallback 清空 tiers 进入兼容模式
func TestFetchUsers_OldFormat_ClearsTiers(t *testing.T) {
	srv := usersServer([]string{"u1"}) // 老格式
	defer srv.Close()

	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	s := newSync(nil, srv.URL)
	// 预先放一些 tiers（模拟上次拉到新格式，这次后端回到老格式）
	s.tiers = map[string]TierConfig{"vip": {MarkID: 1, InboundTag: "proxy-vip", PoolMbps: 100}}

	_, err := s.fetchUsers()
	if err != nil {
		t.Fatalf("fetchUsers err: %v", err)
	}

	if len(s.Tiers()) != 0 {
		t.Errorf("老格式响应后应清空 tiers 进入兼容模式，但 tiers = %v", s.Tiers())
	}
}

// Gap 3: fetchDelta V2 格式（added 是对象数组，带 tier）
func TestFetchDelta_V2Format(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 200,
			"data": map[string]any{
				"added": []map[string]any{
					{"uuid": "new-vip", "tier": "vip"},
					{"uuid": "new-svip", "tier": "svip"},
				},
				"removed": []string{"gone-1"},
			},
		})
	}))
	defer srv.Close()

	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	s := newSync(nil, srv.URL)
	delta, err := s.fetchDelta(0)
	if err != nil {
		t.Fatalf("fetchDelta err: %v", err)
	}

	if len(delta.Added) != 2 {
		t.Fatalf("added len = %d, want 2", len(delta.Added))
	}
	byUUID := map[string]string{}
	for _, u := range delta.Added {
		byUUID[u.UUID] = u.Tier
	}
	if byUUID["new-vip"] != "vip" || byUUID["new-svip"] != "svip" {
		t.Errorf("delta added tier wrong: %+v", byUUID)
	}
	if len(delta.Removed) != 1 || delta.Removed[0] != "gone-1" {
		t.Errorf("delta removed = %v, want [gone-1]", delta.Removed)
	}
}

// Gap 4: 请求必须带 X-Agent-Version: 2 header（后端靠此分流新老格式）
func TestFetchUsers_SendsAgentVersionHeader(t *testing.T) {
	var gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.Header.Get("X-Agent-Version")
		_ = json.NewEncoder(w).Encode(apiResp{Code: 200})
	}))
	defer srv.Close()

	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	s := newSync(&mockXrayAPI{}, srv.URL)
	_, _ = s.fetchUsers()

	if gotVersion != "2" {
		t.Errorf("X-Agent-Version = %q, want %q", gotVersion, "2")
	}
}

func TestFetchDelta_SendsAgentVersionHeader(t *testing.T) {
	var gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.Header.Get("X-Agent-Version")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": map[string]any{"added": []string{}, "removed": []string{}}})
	}))
	defer srv.Close()

	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	s := newSync(&mockXrayAPI{}, srv.URL)
	_, _ = s.fetchDelta(0)

	if gotVersion != "2" {
		t.Errorf("X-Agent-Version = %q, want %q", gotVersion, "2")
	}
}

// Gap 5: AddUser 按 tier 路由到对应 inbound tag
func TestAddUser_RoutesToTierInbound(t *testing.T) {
	mock := &mockXrayAPI{ready: true}
	s := withTiers(newSync(mock, ""), "vip", map[string]TierConfig{
		"vip":  {MarkID: 1, InboundTag: "proxy-vip", PoolMbps: 100},
		"svip": {MarkID: 2, InboundTag: "proxy-svip", PoolMbps: 500},
	})

	if err := s.AddUser("uuid-vip", "vip"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUser("uuid-svip", "svip"); err != nil {
		t.Fatal(err)
	}

	// 断言：两次 AddOrReplaceToTag 用的 inboundTag 分别是 proxy-vip / proxy-svip
	mock.mu.Lock()
	addedTags := append([]string{}, mock.addedTags...)
	mock.mu.Unlock()

	if len(addedTags) != 2 {
		t.Fatalf("expected 2 add calls, got %d", len(addedTags))
	}
	if addedTags[0] != "proxy-vip" {
		t.Errorf("first add inboundTag = %q, want proxy-vip", addedTags[0])
	}
	if addedTags[1] != "proxy-svip" {
		t.Errorf("second add inboundTag = %q, want proxy-svip", addedTags[1])
	}
}

// Gap 5b: RemoveUser 按 current 记录的 tier 查 inbound
func TestRemoveUser_UsesTierFromCurrent(t *testing.T) {
	mock := &mockXrayAPI{}
	s := withTiers(newSync(mock, ""), "vip", map[string]TierConfig{
		"vip":  {MarkID: 1, InboundTag: "proxy-vip", PoolMbps: 100},
		"svip": {MarkID: 2, InboundTag: "proxy-svip", PoolMbps: 500},
	})
	// 模拟 current 里 u-svip 记为 svip tier
	s.current["u-svip"] = "svip"

	if err := s.RemoveUser("u-svip"); err != nil {
		t.Fatal(err)
	}

	mock.mu.Lock()
	tags := append([]string{}, mock.removedTags...)
	mock.mu.Unlock()
	if len(tags) != 1 || tags[0] != "proxy-svip" {
		t.Errorf("remove inboundTag = %v, want [proxy-svip]", tags)
	}
}

// Gap 6: writeConfig 多 inbound 模式按 tier 分组 clients
func TestWriteConfig_MultiInboundByTier(t *testing.T) {
	// config 含两个 inbound：proxy-vip 和 proxy-svip
	config := map[string]interface{}{
		"inbounds": []map[string]interface{}{
			{"tag": "proxy-vip", "settings": map[string]interface{}{"clients": []interface{}{}}},
			{"tag": "proxy-svip", "settings": map[string]interface{}{"clients": []interface{}{}}},
		},
	}
	data, _ := json.Marshal(config)

	f, err := os.CreateTemp("", "xray_multi_*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	_, _ = f.Write(data)
	f.Close()

	s := NewXrayUserSync("", "", "", "ignored-in-multi-mode", f.Name())
	s.tiers = map[string]TierConfig{
		"vip":  {MarkID: 1, InboundTag: "proxy-vip", PoolMbps: 100},
		"svip": {MarkID: 2, InboundTag: "proxy-svip", PoolMbps: 500},
	}
	s.defaultTier = "vip"

	users := []userDTO{
		{UUID: "aaaa0000-0000-0000-0000-000000000100", Tier: "vip"},
		{UUID: "bbbb0000-0000-0000-0000-000000000200", Tier: "svip"},
	}
	if err := s.writeConfig(users); err != nil {
		t.Fatalf("writeConfig err: %v", err)
	}

	// 解析回来，断言每个 inbound 的 clients 里只有对应 tier 的用户 + defaultUUID
	out, _ := os.ReadFile(f.Name())
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(out, &raw)
	var inbounds []map[string]json.RawMessage
	_ = json.Unmarshal(raw["inbounds"], &inbounds)

	clientsByTag := map[string]map[string]bool{}
	for _, ib := range inbounds {
		var tag string
		_ = json.Unmarshal(ib["tag"], &tag)
		var settings map[string]json.RawMessage
		_ = json.Unmarshal(ib["settings"], &settings)
		var clients []map[string]string
		_ = json.Unmarshal(settings["clients"], &clients)
		ids := map[string]bool{}
		for _, c := range clients {
			ids[c["id"]] = true
		}
		clientsByTag[tag] = ids
	}

	vipIDs := clientsByTag["proxy-vip"]
	svipIDs := clientsByTag["proxy-svip"]

	if !vipIDs["aaaa0000-0000-0000-0000-000000000100"] {
		t.Error("vip user should be in proxy-vip inbound")
	}
	if vipIDs["bbbb0000-0000-0000-0000-000000000200"] {
		t.Error("svip user should NOT be in proxy-vip inbound")
	}
	if !svipIDs["bbbb0000-0000-0000-0000-000000000200"] {
		t.Error("svip user should be in proxy-svip inbound")
	}
	if svipIDs["aaaa0000-0000-0000-0000-000000000100"] {
		t.Error("vip user should NOT be in proxy-svip inbound")
	}
	// defaultUUID 应进每个 tier inbound
	if !vipIDs[defaultUUID] || !svipIDs[defaultUUID] {
		t.Error("defaultUUID should be in both tier inbounds")
	}
}

// Gap 6b: writeConfig 多 inbound 模式下，用户 tier 缺失时归入 defaultTier
func TestWriteConfig_MultiInbound_MissingTierUsesDefault(t *testing.T) {
	config := map[string]interface{}{
		"inbounds": []map[string]interface{}{
			{"tag": "proxy-vip", "settings": map[string]interface{}{"clients": []interface{}{}}},
			{"tag": "proxy-svip", "settings": map[string]interface{}{"clients": []interface{}{}}},
		},
	}
	data, _ := json.Marshal(config)
	f, _ := os.CreateTemp("", "xray_default_*.json")
	defer os.Remove(f.Name())
	_, _ = f.Write(data)
	f.Close()

	s := NewXrayUserSync("", "", "", "", f.Name())
	s.tiers = map[string]TierConfig{
		"vip":  {MarkID: 1, InboundTag: "proxy-vip", PoolMbps: 100},
		"svip": {MarkID: 2, InboundTag: "proxy-svip", PoolMbps: 500},
	}
	s.defaultTier = "vip"

	users := []userDTO{
		{UUID: "cccc0000-0000-0000-0000-000000000300", Tier: ""}, // 老数据没 tier
	}
	_ = s.writeConfig(users)

	out, _ := os.ReadFile(f.Name())
	if !strings.Contains(string(out), "cccc0000-0000-0000-0000-000000000300") {
		t.Error("legacy user should have been placed in some inbound")
	}
	// 归入 default tier = vip，应出现在 proxy-vip 的 clients 里
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(out, &raw)
	var inbounds []map[string]json.RawMessage
	_ = json.Unmarshal(raw["inbounds"], &inbounds)
	for _, ib := range inbounds {
		var tag string
		_ = json.Unmarshal(ib["tag"], &tag)
		if tag != "proxy-vip" {
			continue
		}
		var settings map[string]json.RawMessage
		_ = json.Unmarshal(ib["settings"], &settings)
		var clients []map[string]string
		_ = json.Unmarshal(settings["clients"], &clients)
		found := false
		for _, c := range clients {
			if c["id"] == "cccc0000-0000-0000-0000-000000000300" {
				found = true
			}
		}
		if !found {
			t.Error("legacy user should be in defaultTier=vip inbound (proxy-vip)")
		}
	}
}
