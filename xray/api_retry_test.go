//go:build xray

package xray

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func init() {
	apiRetryDelay = time.Millisecond
}

// ---- doWithRetry ----

func TestDoWithRetry_SuccessOnFirstAttempt(t *testing.T) {
	calls := 0
	err := doWithRetry(2, time.Millisecond, func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

func TestDoWithRetry_RetriesOnNetworkError(t *testing.T) {
	calls := 0
	err := doWithRetry(2, time.Millisecond, func() error {
		calls++
		if calls < 3 {
			return io.EOF // 网络错误，应重试
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls (1 initial + 2 retries), got %d", calls)
	}
}

func TestDoWithRetry_BusinessErrorNotRetried(t *testing.T) {
	calls := 0
	bizErr := fmt.Errorf("api returned code=500")
	err := doWithRetry(2, time.Millisecond, func() error {
		calls++
		return bizErr
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("business error should not be retried, expected 1 call, got %d", calls)
	}
}

func TestDoWithRetry_AllRetriesExhausted(t *testing.T) {
	calls := 0
	err := doWithRetry(2, time.Millisecond, func() error {
		calls++
		return io.EOF
	})
	if err == nil {
		t.Fatal("expected error after all retries exhausted")
	}
	if calls != 3 { // 1 initial + 2 retries
		t.Errorf("expected 3 calls, got %d", calls)
	}
}

func TestDoWithRetry_ZeroMaxRetries(t *testing.T) {
	calls := 0
	err := doWithRetry(0, time.Millisecond, func() error {
		calls++
		return io.EOF
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("expected 1 call with maxRetries=0, got %d", calls)
	}
}

// ---- fetchUsers 重试集成 ----

// flakyUsersServer 返回一个服务器，前 failN 次强制断开连接，之后返回 users 列表。
func flakyUsersServer(t *testing.T, failN int, uuids []string) *httptest.Server {
	t.Helper()
	var attempts atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(attempts.Add(1))
		if n <= failN {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("httptest.Server does not support hijacking")
				return
			}
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		var data []userDTO
		for _, u := range uuids {
			data = append(data, userDTO{UUID: u})
		}
		_ = json.NewEncoder(w).Encode(apiResp{Code: 200, Data: data})
	}))
}

func TestFetchUsers_RetryOnNetworkError_SucceedsOnSecondAttempt(t *testing.T) {
	srv := flakyUsersServer(t, 1, []string{"u1", "u2"})
	defer srv.Close()
	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	s := newSync(nil, srv.URL)
	users, err := s.fetchUsers()
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if len(users) != 2 {
		t.Errorf("expected 2 users, got %d", len(users))
	}
}

func TestFetchUsers_AllRetriesExhausted_ReturnsError(t *testing.T) {
	srv := flakyUsersServer(t, 3, nil)
	defer srv.Close()
	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	s := newSync(nil, srv.URL)
	_, err := s.fetchUsers()
	if err == nil {
		t.Fatal("expected error when all retries exhausted")
	}
}

func TestFetchUsers_BusinessErrorNotRetried(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
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
	if n := int(attempts.Load()); n != 1 {
		t.Errorf("business error should not be retried, expected 1 attempt, got %d", n)
	}
}

// ---- fetchDelta 重试集成 ----

// flakyDeltaServer 返回一个服务器，前 failN 次强制断开连接，之后正常响应。
func flakyDeltaServer(t *testing.T, failN int, added, removed []string) *httptest.Server {
	t.Helper()
	var attempts atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(attempts.Add(1))
		if n <= failN {
			// 强制断开连接，让客户端收到网络错误
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("httptest.Server does not support hijacking")
				return
			}
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		// 用原始 map 发送老格式（added: []string），让 fetchDelta 的 fallback 路径处理
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 200,
			"data": map[string]any{
				"added":   added,
				"removed": removed,
			},
		})
	}))
}

func TestFetchDelta_RetryOnNetworkError_SucceedsOnSecondAttempt(t *testing.T) {
	srv := flakyDeltaServer(t, 1, []string{"u1"}, nil)
	defer srv.Close()
	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	s := newSync(nil, srv.URL)
	delta, err := s.fetchDelta(0)
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if len(delta.Added) != 1 || delta.Added[0].UUID != "u1" {
		t.Errorf("unexpected delta: %v", delta)
	}
}

func TestFetchDelta_AllRetriesExhausted_ReturnsError(t *testing.T) {
	// 失败次数超过 maxRetries(2)+1，每次都断开
	srv := flakyDeltaServer(t, 3, nil, nil)
	defer srv.Close()
	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	s := newSync(nil, srv.URL)
	_, err := s.fetchDelta(0)
	if err == nil {
		t.Fatal("expected error when all retries exhausted")
	}
}

func TestFetchDelta_BusinessErrorNotRetried(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		_ = json.NewEncoder(w).Encode(deltaResp{Code: 403}) // 业务错误
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
	if n := int(attempts.Load()); n != 1 {
		t.Errorf("business error should not be retried, expected 1 attempt, got %d", n)
	}
}
