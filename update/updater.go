package update

import (
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
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=10", repo)
	return fetchLatestVersionFor(url, assetName)
}

var downloadFn = func(tag, assetName string) error {
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, tag, assetName)
	return downloadAndReplaceFrom(url)
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
		for _, a := range r.Assets {
			if a.Name == assetName {
				return r.TagName, nil
			}
		}
	}
	return "", fmt.Errorf("no release found containing asset %q", assetName)
}

func downloadAndReplaceFrom(url string) error {
	resp, err := httpClient.Get(url)
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
	n, copyErr := io.Copy(f, resp.Body)
	f.Close()
	if copyErr != nil {
		os.Remove(tmp)
		return fmt.Errorf("download binary: %w", copyErr)
	}
	if resp.ContentLength > 0 && n != resp.ContentLength {
		os.Remove(tmp)
		return fmt.Errorf("download binary: incomplete (%d/%d bytes)", n, resp.ContentLength)
	}

	if err := os.Rename(tmp, self); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace binary: %w", err)
	}
	return nil
}
