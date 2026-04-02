package update

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func withHTTPClient(srv *httptest.Server, f func()) {
	orig := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = orig }()
	f()
}

func withFetchFn(fn func(assetName string) (string, error), f func()) {
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

// --- fetchLatestVersionFor ---

func TestFetchLatestVersionFor_Success(t *testing.T) {
	releases := []ghRelease{
		{TagName: "v1.2.3-xray", Assets: []ghAsset{{Name: "node-agent-xray"}}},
		{TagName: "v1.2.2", Assets: []ghAsset{{Name: "node-agent-ikev2"}, {Name: "node-agent-xray"}}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(releases)
	}))
	defer srv.Close()

	withHTTPClient(srv, func() {
		// xray 节点应找到最新的含 xray asset 的 release
		tag, err := fetchLatestVersionFor(srv.URL, "node-agent-xray")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tag != "v1.2.3-xray" {
			t.Errorf("got %q, want %q", tag, "v1.2.3-xray")
		}

		// ikev2 节点跳过 xray-only release，找到含 ikev2 asset 的版本
		tag, err = fetchLatestVersionFor(srv.URL, "node-agent-ikev2")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tag != "v1.2.2" {
			t.Errorf("got %q, want %q", tag, "v1.2.2")
		}
	})
}

func TestFetchLatestVersionFor_TagFilter(t *testing.T) {
	// 前两个都是 protocol-only tag，应被快速跳过，找到第三个
	releases := []ghRelease{
		{TagName: "v1.3.0-xray", Assets: []ghAsset{{Name: "node-agent-xray"}}},
		{TagName: "v1.2.9-xray", Assets: []ghAsset{{Name: "node-agent-xray"}}},
		{TagName: "v1.2.8-ikev2", Assets: []ghAsset{{Name: "node-agent-ikev2"}}},
		{TagName: "v1.2.7", Assets: []ghAsset{{Name: "node-agent-ikev2"}, {Name: "node-agent-xray"}}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(releases)
	}))
	defer srv.Close()

	withHTTPClient(srv, func() {
		// xray 节点：跳过 -ikev2 tag，找到第一个 -xray tag
		tag, err := fetchLatestVersionFor(srv.URL, "node-agent-xray")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tag != "v1.3.0-xray" {
			t.Errorf("got %q, want %q", tag, "v1.3.0-xray")
		}

		// ikev2 节点：跳过所有 -xray tag，找到 -ikev2 tag
		tag, err = fetchLatestVersionFor(srv.URL, "node-agent-ikev2")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tag != "v1.2.8-ikev2" {
			t.Errorf("got %q, want %q", tag, "v1.2.8-ikev2")
		}
	})
}

func TestFetchLatestVersionFor_NoMatchingAsset(t *testing.T) {
	releases := []ghRelease{
		{TagName: "v1.2.3", Assets: []ghAsset{{Name: "node-agent-xray"}}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(releases)
	}))
	defer srv.Close()

	withHTTPClient(srv, func() {
		_, err := fetchLatestVersionFor(srv.URL, "node-agent-ikev2")
		if err == nil {
			t.Fatal("expected error when no matching asset found")
		}
	})
}

func TestFetchLatestVersionFor_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()

	withHTTPClient(srv, func() {
		_, err := fetchLatestVersionFor(srv.URL, "node-agent-ikev2")
		if err == nil {
			t.Fatal("expected error for 404")
		}
	})
}

func TestFetchLatestVersionFor_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("not-json"))
	}))
	defer srv.Close()

	withHTTPClient(srv, func() {
		_, err := fetchLatestVersionFor(srv.URL, "node-agent-ikev2")
		if err == nil {
			t.Fatal("expected JSON parse error")
		}
	})
}

func TestFetchLatestVersionFor_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	withHTTPClient(srv, func() {
		_, err := fetchLatestVersionFor(url, "node-agent-ikev2")
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
		err := downloadAndReplaceFrom(srv.URL+"/binary", srv.URL+"/binary.sha256")
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
		err := downloadAndReplaceFrom(url, url+".sha256")
		if err == nil {
			t.Fatal("expected network error")
		}
	})
}

func TestDownloadAndReplaceFrom_ChecksumMismatch(t *testing.T) {
	content := []byte("fake binary content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			// 返回错误的 checksum
			fmt.Fprintf(w, "%s  node-agent-ikev2\n", strings.Repeat("0", 64))
			return
		}
		w.WriteHeader(200)
		w.Write(content)
	}))
	defer srv.Close()

	withHTTPClient(srv, func() {
		err := downloadAndReplaceFrom(srv.URL+"/binary", srv.URL+"/binary.sha256")
		if err == nil {
			t.Fatal("expected checksum mismatch error")
		}
		if !strings.Contains(err.Error(), "checksum mismatch") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

// --- TryUpdate ---

func TestTryUpdate_AlreadyUpToDate(t *testing.T) {
	withFetchFn(func(_ string) (string, error) { return "v1.0.3", nil }, func() {
		if err := TryUpdate("1.0.3", "node-agent-ikev2"); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})
}

func TestTryUpdate_AlreadyUpToDate_BothVPrefix(t *testing.T) {
	withFetchFn(func(_ string) (string, error) { return "v2.0.0", nil }, func() {
		if err := TryUpdate("v2.0.0", "node-agent-ikev2"); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})
}

func TestTryUpdate_FetchError(t *testing.T) {
	withFetchFn(func(_ string) (string, error) { return "", fmt.Errorf("network down") }, func() {
		err := TryUpdate("1.0.0", "node-agent-ikev2")
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestTryUpdate_DownloadError(t *testing.T) {
	withFetchFn(func(_ string) (string, error) { return "v1.0.9", nil }, func() {
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
	withFetchFn(func(_ string) (string, error) { return "v1.5.0", nil }, func() {
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

func TestTryUpdate_FetchPassesAssetName(t *testing.T) {
	var gotAsset string
	withFetchFn(func(assetName string) (string, error) {
		gotAsset = assetName
		return "v9.9.9", nil
	}, func() {
		withDownloadFn(func(_, _ string) error { return fmt.Errorf("stop") }, func() {
			_ = TryUpdate("1.0.0", "node-agent-xray")
		})
	})
	if gotAsset != "node-agent-xray" {
		t.Errorf("fetchFn received asset %q, want %q", gotAsset, "node-agent-xray")
	}
}
