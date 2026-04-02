package update

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const repo = "salt-lake/kd-vps-agent"

var httpClient = &http.Client{Timeout: 60 * time.Second}

// fetchFn / downloadFn 可在测试中替换
var fetchFn = func(assetName string) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=20", repo)
	return fetchLatestVersionFor(url, assetName)
}

var downloadFn = func(tag, assetName string) error {
	binaryURL := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, tag, assetName)
	checksumURL := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s.sha256", repo, tag, assetName)
	return downloadAndReplaceFrom(binaryURL, checksumURL)
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name string `json:"name"`
}

// CheckAndUpdate 检查 GitHub 最新 Release，若版本不同则下载并重启。
// 下载失败只打本地日志，不上报 Sentry，保持原 binary 不变。
func CheckAndUpdate(currentVersion, assetName string) {
	if err := TryUpdate(currentVersion, assetName); err != nil {
		log.Printf("check update failed (keeping current version): %v", err)
	}
}

// baseVersion 去掉 v 前缀和 build suffix（如 -ikev2、-xray），用于与 GitHub tag 比较。
func baseVersion(v string) string {
	v = strings.TrimPrefix(v, "v")
	if i := strings.Index(v, "-"); i >= 0 {
		v = v[:i]
	}
	return v
}

// TryUpdate 执行检查并更新，返回 error；已是最新版时返回 nil。
func TryUpdate(currentVersion, assetName string) error {
	latest, err := fetchFn(assetName)
	if err != nil {
		return fmt.Errorf("fetch version: %w", err)
	}
	// 统一去掉 v 前缀和 build suffix 再比较
	if baseVersion(latest) == baseVersion(currentVersion) {
		return nil
	}
	log.Printf("update available: %s -> %s, downloading...", currentVersion, latest)
	if err := downloadFn(latest, assetName); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	log.Println("update success, restarting via systemctl...")
	err = exec.Command("systemctl", "restart", "node-agent").Run()
	// systemctl restart 会向当前进程发送 SIGTERM，Run() 因此返回 signal error，属于正常重启流程。
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				return nil
			}
		}
		return fmt.Errorf("systemctl restart node-agent: %w", err)
	}
	return nil
}

// fetchLatestVersionFor 扫描最近 releases，返回第一个包含指定 asset 的 release tag。
// 这样 xray-only 发版时，ikev2 节点找到的仍是上一个含 ikev2 asset 的版本，不会误触发更新。
// tag 命名约定（-xray / -ikev2 后缀）用于快速跳过明确不含目标 asset 的 release。
func fetchLatestVersionFor(url, assetName string) (string, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("github releases returned %d", resp.StatusCode)
	}
	var releases []ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return "", err
	}
	for _, r := range releases {
		if strings.HasSuffix(r.TagName, "-xray") && assetName == "node-agent-ikev2" {
			continue
		}
		if strings.HasSuffix(r.TagName, "-ikev2") && assetName == "node-agent-xray" {
			continue
		}
		for _, a := range r.Assets {
			if a.Name == assetName {
				return r.TagName, nil
			}
		}
	}
	return "", fmt.Errorf("no release found containing asset %q", assetName)
}

func downloadAndReplaceFrom(binaryURL, checksumURL string) error {
	resp, err := httpClient.Get(binaryURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download binary returned %d", resp.StatusCode)
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("get executable path: %w", err)
	}

	tmp := self + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return fmt.Errorf("create tmp file: %w", err)
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), resp.Body)
	f.Close()
	if copyErr != nil {
		os.Remove(tmp)
		return fmt.Errorf("download binary: %w", copyErr)
	}
	if resp.ContentLength > 0 && n != resp.ContentLength {
		os.Remove(tmp)
		return fmt.Errorf("download binary: incomplete (%d/%d bytes)", n, resp.ContentLength)
	}

	expected, err := fetchChecksum(checksumURL)
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("fetch checksum: %w", err)
	}
	got := fmt.Sprintf("%x", h.Sum(nil))
	if got != expected {
		os.Remove(tmp)
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, expected)
	}

	if err := os.Rename(tmp, self); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace binary: %w", err)
	}
	return nil
}

// fetchChecksum 下载 .sha256 文件，返回十六进制 hash 字符串。
// sha256sum 输出格式：<64-hex>  <filename>
func fetchChecksum(url string) (string, error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("fetch checksum returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(body))
	if len(fields) == 0 {
		return "", fmt.Errorf("empty checksum file")
	}
	hash := strings.ToLower(fields[0])
	if len(hash) != 64 {
		return "", fmt.Errorf("invalid checksum format")
	}
	return hash, nil
}
