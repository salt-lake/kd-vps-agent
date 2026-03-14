package update

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"time"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

type versionResp struct {
	Version string `json:"version"`
}

// CheckAndUpdate 检查服务端版本，若不同则下载新二进制并通过 systemctl 重启
func CheckAndUpdate(apiBase, token, currentVersion string) {
	if err := TryUpdate(apiBase, token, currentVersion); err != nil {
		log.Printf("check update failed: %v", err)
	}
}

// TryUpdate 执行检查并更新，返回 error；已是最新版时返回 nil。
func TryUpdate(apiBase, token, currentVersion string) error {
	latest, err := fetchLatestVersion(apiBase, token)
	if err != nil {
		return fmt.Errorf("fetch version: %w", err)
	}
	if latest == currentVersion {
		return nil
	}
	log.Printf("update available: %s -> %s, downloading...", currentVersion, latest)
	if err := downloadAndReplace(apiBase, token); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	log.Println("update success, restarting via systemctl...")
	_ = exec.Command("systemctl", "restart", "node-agent").Run()
	return nil
}

func fetchLatestVersion(apiBase, token string) (string, error) {
	req, err := http.NewRequest("GET", apiBase+"/api/agent/version", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("version endpoint returned %d", resp.StatusCode)
	}
	var v versionResp
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	return v.Version, nil
}

func downloadAndReplace(apiBase, token string) error {
	req, err := http.NewRequest("GET", apiBase+"/api/agent/binary", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("binary endpoint returned %d", resp.StatusCode)
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

	if err := os.Chmod(tmp, 0755); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("chmod binary: %w", err)
	}
	if err := os.Rename(tmp, self); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace binary: %w", err)
	}
	return nil
}
