//go:build xray

package xray

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeSyncer struct {
	addCalls    []string
	removeCalls []string
	addErr      error
	removeErr   error
}

func (f *fakeSyncer) AddUser(uuid string) error {
	f.addCalls = append(f.addCalls, uuid)
	return f.addErr
}

func (f *fakeSyncer) RemoveUser(uuid string) error {
	f.removeCalls = append(f.removeCalls, uuid)
	return f.removeErr
}

func newRequest(method, path, body, token string) *http.Request {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func TestHTTPAPI_Add(t *testing.T) {
	f := &fakeSyncer{}
	api := NewHTTPAPI(f, "tok")
	w := httptest.NewRecorder()
	api.ServeHTTP(w, newRequest(http.MethodPost, "/xray/users", `{"uuid":"abc"}`, "tok"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(f.addCalls) != 1 || f.addCalls[0] != "abc" {
		t.Errorf("addCalls=%v, want [abc]", f.addCalls)
	}
}

func TestHTTPAPI_Remove(t *testing.T) {
	f := &fakeSyncer{}
	api := NewHTTPAPI(f, "tok")
	w := httptest.NewRecorder()
	api.ServeHTTP(w, newRequest(http.MethodDelete, "/xray/users/xyz", "", "tok"))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(f.removeCalls) != 1 || f.removeCalls[0] != "xyz" {
		t.Errorf("removeCalls=%v, want [xyz]", f.removeCalls)
	}
}

func TestHTTPAPI_Auth(t *testing.T) {
	f := &fakeSyncer{}
	api := NewHTTPAPI(f, "secret")

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong scheme", "Basic abc", http.StatusUnauthorized},
		{"wrong token", "Bearer wrong", http.StatusUnauthorized},
		{"correct token", "Bearer secret", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/xray/users", strings.NewReader(`{"uuid":"u"}`))
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			w := httptest.NewRecorder()
			api.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Errorf("status=%d, want=%d, body=%s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestHTTPAPI_BadRequests(t *testing.T) {
	f := &fakeSyncer{}
	api := NewHTTPAPI(f, "tok")

	cases := []struct {
		name string
		req  *http.Request
		want int
	}{
		{"add empty body", newRequest(http.MethodPost, "/xray/users", "", "tok"), http.StatusBadRequest},
		{"add bad json", newRequest(http.MethodPost, "/xray/users", `{`, "tok"), http.StatusBadRequest},
		{"add missing uuid", newRequest(http.MethodPost, "/xray/users", `{}`, "tok"), http.StatusBadRequest},
		{"remove no uuid", newRequest(http.MethodDelete, "/xray/users/", "", "tok"), http.StatusNotFound},
		{"remove multi-segment", newRequest(http.MethodDelete, "/xray/users/abc/extra", "", "tok"), http.StatusNotFound},
		{"unknown path", newRequest(http.MethodGet, "/foo", "", "tok"), http.StatusNotFound},
		{"wrong method", newRequest(http.MethodPut, "/xray/users", "", "tok"), http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			api.ServeHTTP(w, tc.req)
			if w.Code != tc.want {
				t.Errorf("status=%d, want=%d, body=%s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestHTTPAPI_DownstreamError(t *testing.T) {
	f := &fakeSyncer{addErr: errors.New("xray gRPC down")}
	api := NewHTTPAPI(f, "tok")
	w := httptest.NewRecorder()
	api.ServeHTTP(w, newRequest(http.MethodPost, "/xray/users", `{"uuid":"u"}`, "tok"))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status=%d, want 500, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "xray gRPC down") {
		t.Errorf("body=%q does not contain downstream error", w.Body.String())
	}
}
