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
	"sync"
	"testing"
	"time"
)

// ---- mockXrayAPI ----

type mockXrayAPI struct {
	mu          sync.Mutex
	ready       bool
	readyAfter  int // 若 >0，前 readyAfter 次 IsXrayReady 返回 false
	probes      int // IsXrayReady 调用次数
	added       []string // UUIDs passed to AddOrReplace
	removed     []string // IDs passed to RemoveUserById
	addErr      error
	removeErr   error
	closeCalled bool
}

func (m *mockXrayAPI) IsXrayReady(_ context.Context) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.probes++
	if m.readyAfter > 0 {
		return m.probes > m.readyAfter
	}
	return m.ready
}

func (m *mockXrayAPI) AddOrReplace(_ context.Context, u *User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.added = append(m.added, u.UUID)
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

func (m *mockXrayAPI) RemoveUserById(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, id)
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

func newSync(api XrayAPI, apiBase string) *XrayUserSync {
	s := NewXrayUserSync(apiBase, "token", "127.0.0.1:10085", "vless", "")
	s.xrayAPI = api
	return s
}

// usersServer 返回一个 httptest.Server，响应 /api/agent/xray/users。
func usersServer(uuids []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data []userDTO
		for _, u := range uuids {
			data = append(data, userDTO{UUID: u})
		}
		_ = json.NewEncoder(w).Encode(apiResp{Code: 200, Data: data})
	}))
}

// deltaServer 返回一个 httptest.Server，响应 /api/agent/xray/users/delta。
func deltaServer(added, removed []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(deltaResp{
			Code: 200,
			Data: deltaData{Added: added, Removed: removed},
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
	if len(delta.Added) != 1 || delta.Added[0] != "add1" {
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
	if err := s.AddUser("aaaa0000-0000-0000-0000-000000000002"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := mock.allAdded(); len(got) != 1 || got[0] != "aaaa0000-0000-0000-0000-000000000002" {
		t.Errorf("added = %v, want [aaaa...0002]", got)
	}
}

func TestAddUser_ConnectionError_ResetsAPI(t *testing.T) {
	mock := &mockXrayAPI{addErr: fmt.Errorf("connection refused")}
	s := newSync(mock, "")
	err := s.AddUser("aaaa0000-0000-0000-0000-000000000002")
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
	_ = s.AddUser("aaaa0000-0000-0000-0000-000000000002")
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
	s := newSync(mock, "")
	if err := s.RemoveUser("aaaa0000-0000-0000-0000-000000000003"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := mock.allRemoved(); len(got) != 1 || got[0] != "aaaa0000-0000-0000-0000-000000000003" {
		t.Errorf("removed = %v, want [aaaa...0003]", got)
	}
}

func TestRemoveUser_ConnectionError_ResetsAPI(t *testing.T) {
	mock := &mockXrayAPI{removeErr: fmt.Errorf("transport: connection is unavailable")}
	s := newSync(mock, "")
	_ = s.RemoveUser("aaaa0000-0000-0000-0000-000000000003")
	s.mu.Lock()
	api := s.xrayAPI
	s.mu.Unlock()
	if api != nil {
		t.Error("expected xrayAPI to be reset to nil on connection error")
	}
}

// ---- DeltaSync ----

func TestDeltaSync_NoStateFile_InitializesCursor(t *testing.T) {
	statePath := t.TempDir() + "/sync_state.json"

	origStateFile := syncStateFile
	syncStateFile = statePath
	defer func() { syncStateFile = origStateFile }()

	mock := &mockXrayAPI{}
	s := newSync(mock, "http://127.0.0.1:1")

	before := time.Now().Unix()
	if err := s.DeltaSync(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	after := time.Now().Unix()

	if len(mock.allAdded()) != 0 || len(mock.allRemoved()) != 0 {
		t.Errorf("expected no user ops on first run, got add=%v remove=%v", mock.allAdded(), mock.allRemoved())
	}

	state, err := loadSyncState()
	if err != nil {
		t.Fatalf("expected state file to be created, got %v", err)
	}
	if state.LastSyncTime < before-1 || state.LastSyncTime > after {
		t.Errorf("LastSyncTime = %d, want in [%d, %d]", state.LastSyncTime, before-1, after)
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
	s := newSync(mock, srv.URL)
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
	if err := s.DeltaSync(); err == nil {
		t.Fatal("DeltaSync should propagate AddUser errors")
	}
}

// ---- injectUsers ----

func TestInjectUsers_AllSucceed(t *testing.T) {
	mock := &mockXrayAPI{ready: true}
	s := newSync(mock, "")
	users := []userDTO{{UUID: "u1"}, {UUID: "u2"}}
	if err := s.injectUsers(users); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := sorted(mock.allAdded()); len(got) != 2 || got[0] != "u1" || got[1] != "u2" {
		t.Errorf("added = %v, want [u1 u2]", got)
	}
}

func TestInjectUsers_SkipsDefaultUUID(t *testing.T) {
	mock := &mockXrayAPI{ready: true}
	s := newSync(mock, "")
	users := []userDTO{{UUID: defaultUUID}, {UUID: "u1"}}
	if err := s.injectUsers(users); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, id := range mock.allAdded() {
		if id == defaultUUID {
			t.Error("defaultUUID should not be sent to xray")
		}
	}
	if len(mock.allAdded()) != 1 || mock.allAdded()[0] != "u1" {
		t.Errorf("added = %v, want [u1]", mock.allAdded())
	}
}

func TestInjectUsers_NotReady(t *testing.T) {
	mock := &mockXrayAPI{ready: false}
	s := newSync(mock, "")
	if err := s.injectUsers([]userDTO{{UUID: "u1"}}); err == nil {
		t.Fatal("expected error when xray gRPC not ready")
	}
	if len(mock.allAdded()) != 0 {
		t.Errorf("expected no calls when not ready, got %v", mock.allAdded())
	}
}

func TestInjectUsers_FailFastOnAddError(t *testing.T) {
	mock := &mockXrayAPI{ready: true, addErr: fmt.Errorf("inject failed")}
	s := newSync(mock, "")
	users := []userDTO{{UUID: "u1"}, {UUID: "u2"}, {UUID: "u3"}}
	if err := s.injectUsers(users); err == nil {
		t.Fatal("expected error when add fails")
	}
	// 第一次失败立即返回，不应继续注入剩余用户
	if len(mock.allAdded()) != 1 {
		t.Errorf("expected fail-fast after 1st error, got %d adds", len(mock.allAdded()))
	}
}

// ---- syncAfterRestart ----

// 测试时大幅缩短 retry 间隔，使 4-5 次重试在 ~50ms 内完成。
func withFastSyncRestart() func() {
	origInterval := syncRestartRetryInterval
	origRefresh := syncRestartRefreshEvery
	syncRestartRetryInterval = 5 * time.Millisecond
	syncRestartRefreshEvery = 3
	return func() {
		syncRestartRetryInterval = origInterval
		syncRestartRefreshEvery = origRefresh
	}
}

func TestSyncAfterRestart_FetchOK_InjectImmediately(t *testing.T) {
	defer withFastSyncRestart()()

	srv := usersServer([]string{"u1", "u2"})
	defer srv.Close()
	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	mock := &mockXrayAPI{ready: true}
	s := newSync(mock, srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	s.syncAfterRestart(ctx)

	if got := sorted(mock.allAdded()); len(got) != 2 || got[0] != "u1" || got[1] != "u2" {
		t.Errorf("added = %v, want [u1 u2]", got)
	}
}

func TestSyncAfterRestart_TempUsersReInjected(t *testing.T) {
	defer withFastSyncRestart()()

	srv := usersServer([]string{"u-regular"})
	defer srv.Close()
	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	mock := &mockXrayAPI{ready: true}
	s := newSync(mock, srv.URL)

	tempMgr := newMockManager()
	ts := NewTempUserSync("", "", tempMgr)
	ts.uuids = []string{"temp-1", "temp-2"}
	s.SetTempSync(ts)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	s.syncAfterRestart(ctx)

	// 正式用户走 mockXrayAPI，临时用户走 tempMgr：分别验证。
	if got := mock.allAdded(); len(got) != 1 || got[0] != "u-regular" {
		t.Errorf("regular user injected = %v, want [u-regular]", got)
	}
	if got := sorted(tempMgr.allAdded()); len(got) != 2 || got[0] != "temp-1" || got[1] != "temp-2" {
		t.Errorf("temp users re-injected = %v, want [temp-1 temp-2]", got)
	}
}

func TestSyncAfterRestart_RetriesUntilInjectSucceeds(t *testing.T) {
	defer withFastSyncRestart()()

	srv := usersServer([]string{"u1"})
	defer srv.Close()
	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	mock := &mockXrayAPI{readyAfter: 3}
	s := newSync(mock, srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	s.syncAfterRestart(ctx)

	if mock.probes < 3 {
		t.Errorf("expected at least 3 probes before success, got %d", mock.probes)
	}
	if len(mock.allAdded()) == 0 {
		t.Error("expected eventual successful inject")
	}
}

func TestSyncAfterRestart_CtxCancelExits(t *testing.T) {
	defer withFastSyncRestart()()

	// 后端返回错误，让 fetchUsers 失败 → users 为 nil → 永远走 continue 分支
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	mock := &mockXrayAPI{ready: false}
	s := newSync(mock, srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.syncAfterRestart(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("syncAfterRestart did not exit after ctx cancel")
	}
}

