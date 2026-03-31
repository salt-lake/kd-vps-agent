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

	"github.com/getsentry/sentry-go"
)

const repo = "salt-lake/kd-vps-agent"

var httpClient = &http.Client{Timeout: 60 * time.Second}

// fetchFn / downloadFn 可在测试中替换
var fetchFn = func() (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	return fetchLatestVersionFrom(url)
}

var downloadFn = func(tag, assetName string) error {
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, tag, assetName)
	return downloadAndReplaceFrom(url)
}

type ghRelease struct {
	TagName string `json:"tag_name"`
}

// CheckAndUpdate 检查 GitHub 最新 Release，若版本不同则下载并重启
func CheckAndUpdate(currentVersion, assetName string) {
	if err := TryUpdate(currentVersion, assetName); err != nil {
		log.Printf("check update failed: %v", err)
		sentry.CaptureException(err)
	}
}

// TryUpdate 执行检查并更新，返回 error；已是最新版时返回 nil。
func TryUpdate(currentVersion, assetName string) error {
	latest, err := fetchFn()
	if err != nil {
		return fmt.Errorf("fetch version: %w", err)
	}
	// 统一去掉 v 前缀再比较（tag 可能是 v1.0.3，version.txt 是 1.0.3）
	if strings.TrimPrefix(latest, "v") == strings.TrimPrefix(currentVersion, "v") {
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

func fetchLatestVersionFrom(url string) (string, error) {
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
	var r ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	return r.TagName, nil
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
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("download binary: %w", err)
	}
	f.Close()

	if err := os.Rename(tmp, self); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace binary: %w", err)
	}
	return nil
}
