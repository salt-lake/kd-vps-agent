package update

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func withHTTPClient(srv *httptest.Server, f func()) {
	orig := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = orig }()
	f()
}

func withFetchFn(fn func() (string, error), f func()) {
	orig := fetchFn
	fetchFn = fn
	defer func() { fetchFn = orig }()
	f()
}

func withDownloadFn(fn func(string, string) error, f func()) {
	orig := downloadFn
	downloadFn = fn
	defer func() { downloadFn = orig }()
	f()
}

// --- fetchLatestVersionFrom ---

func TestFetchLatestVersionFrom_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(ghRelease{TagName: "v1.2.3"})
	}))
	defer srv.Close()

	withHTTPClient(srv, func() {
		tag, err := fetchLatestVersionFrom(srv.URL + "/releases/latest")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tag != "v1.2.3" {
			t.Errorf("got %q, want %q", tag, "v1.2.3")
		}
	})
}

func TestFetchLatestVersionFrom_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()

	withHTTPClient(srv, func() {
		_, err := fetchLatestVersionFrom(srv.URL)
		if err == nil {
			t.Fatal("expected error for 404")
		}
	})
}

func TestFetchLatestVersionFrom_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("not-json"))
	}))
	defer srv.Close()

	withHTTPClient(srv, func() {
		_, err := fetchLatestVersionFrom(srv.URL)
		if err == nil {
			t.Fatal("expected JSON parse error")
		}
	})
}

func TestFetchLatestVersionFrom_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // 关闭后再请求，模拟网络错误

	withHTTPClient(srv, func() {
		_, err := fetchLatestVersionFrom(url)
		if err == nil {
			t.Fatal("expected network error")
		}
	})
}

// --- downloadAndReplaceFrom ---

func TestDownloadAndReplaceFrom_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	}))
	defer srv.Close()

	withHTTPClient(srv, func() {
		err := downloadAndReplaceFrom(srv.URL + "/binary")
		if err == nil {
			t.Fatal("expected error for 403")
		}
	})
}

func TestDownloadAndReplaceFrom_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL + "/binary"
	srv.Close()

	withHTTPClient(srv, func() {
		err := downloadAndReplaceFrom(url)
		if err == nil {
			t.Fatal("expected network error")
		}
	})
}

// --- TryUpdate ---

func TestTryUpdate_AlreadyUpToDate(t *testing.T) {
	withFetchFn(func() (string, error) { return "v1.0.3", nil }, func() {
		if err := TryUpdate("1.0.3", "node-agent-ikev2"); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})
}

func TestTryUpdate_AlreadyUpToDate_BothVPrefix(t *testing.T) {
	withFetchFn(func() (string, error) { return "v2.0.0", nil }, func() {
		if err := TryUpdate("v2.0.0", "node-agent-ikev2"); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})
}

func TestTryUpdate_FetchError(t *testing.T) {
	withFetchFn(func() (string, error) { return "", fmt.Errorf("network down") }, func() {
		err := TryUpdate("1.0.0", "node-agent-ikev2")
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestTryUpdate_DownloadError(t *testing.T) {
	withFetchFn(func() (string, error) { return "v1.0.9", nil }, func() {
		withDownloadFn(func(tag, asset string) error {
			return fmt.Errorf("download failed")
		}, func() {
			err := TryUpdate("1.0.0", "node-agent-ikev2")
			if err == nil {
				t.Fatal("expected download error")
			}
		})
	})
}

func TestTryUpdate_DownloadCalledWithCorrectArgs(t *testing.T) {
	withFetchFn(func() (string, error) { return "v1.5.0", nil }, func() {
		var gotTag, gotAsset string
		withDownloadFn(func(tag, asset string) error {
			gotTag, gotAsset = tag, asset
			return fmt.Errorf("stop here")
		}, func() {
			_ = TryUpdate("1.0.0", "node-agent-xray")
			if gotTag != "v1.5.0" {
				t.Errorf("tag = %q, want %q", gotTag, "v1.5.0")
			}
			if gotAsset != "node-agent-xray" {
				t.Errorf("asset = %q, want %q", gotAsset, "node-agent-xray")
			}
		})
	})
}
