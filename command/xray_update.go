//go:build xray

package command

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	xrayBinaryPath = "/usr/local/bin/xray"
	xrayReleaseURL = "https://github.com/XTLS/Xray-core/releases/download/v%s/Xray-linux-64.zip"
)

var xrayHTTPClient = &http.Client{Timeout: 120 * time.Second}

// XrayResyncTrigger 依赖接口，避免直接 import xray 包。
type XrayResyncTrigger interface {
	TriggerResync(ctx context.Context)
}

type xrayUpdateReq struct {
	Version string `json:"version"`
	Config  string `json:"config"`
}

// XrayUpdateHandler 处理 xray_update 指令：更新二进制和/或配置，重启 xray，重注入用户。
type XrayUpdateHandler struct {
	syncer     XrayResyncTrigger
	configPath string
	ctx        context.Context
}

func NewXrayUpdateHandler(ctx context.Context, syncer XrayResyncTrigger, configPath string) XrayUpdateHandler {
	return XrayUpdateHandler{syncer: syncer, configPath: configPath, ctx: ctx}
}

func (XrayUpdateHandler) Name() string { return "xray_update" }

func (h XrayUpdateHandler) Handle(data []byte) ([]byte, error) {
	var req xrayUpdateReq
	if err := json.Unmarshal(data, &req); err != nil {
		return errResp("invalid payload: " + err.Error()), nil
	}
	if req.Version == "" && req.Config == "" {
		return errResp("version or config is required"), nil
	}

	if req.Version != "" {
		if err := downloadXrayBinary(req.Version); err != nil {
			log.Printf("xray_update: download version=%s err=%v", req.Version, err)
			return errResp(fmt.Sprintf("download xray v%s failed: %v", req.Version, err)), nil
		}
		log.Printf("xray_update: binary updated to v%s", req.Version)
	}

	if req.Config != "" {
		if err := mergeXrayConfig(h.configPath, req.Config); err != nil {
			log.Printf("xray_update: merge config err=%v", err)
			return errResp("merge config failed: " + err.Error()), nil
		}
		log.Printf("xray_update: config merged")
	}

	restartCtx, restartCancel := context.WithTimeout(context.Background(), 30*time.Second)
	restartOut, restartErr := exec.CommandContext(restartCtx, "systemctl", "restart", "xray").CombinedOutput()
	restartCancel()
	if restartErr != nil {
		log.Printf("xray_update: systemctl restart xray failed: %v, output: %s", restartErr, restartOut)
		return errResp(fmt.Sprintf("restart xray failed: %v", restartErr)), nil
	}
	log.Printf("xray_update: xray restarted, triggering resync")
	go h.syncer.TriggerResync(h.ctx)
	return okResp("ok"), nil
}

func downloadXrayBinary(version string) error {
	version = strings.TrimPrefix(version, "v")
	url := fmt.Sprintf(xrayReleaseURL, version)
	resp, err := xrayHTTPClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download returned %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp("", "xray-*.zip")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("write zip: %w", err)
	}
	tmp.Close()

	zr, err := zip.OpenReader(tmp.Name())
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer zr.Close()

	var xrayEntry *zip.File
	for _, f := range zr.File {
		if f.Name == "xray" {
			xrayEntry = f
			break
		}
	}
	if xrayEntry == nil {
		return fmt.Errorf("xray binary not found in zip")
	}

	rc, err := xrayEntry.Open()
	if err != nil {
		return fmt.Errorf("open xray in zip: %w", err)
	}
	defer rc.Close()

	tmpBin := xrayBinaryPath + ".new"
	f, err := os.OpenFile(tmpBin, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return fmt.Errorf("create temp binary: %w", err)
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		os.Remove(tmpBin)
		return fmt.Errorf("write binary: %w", err)
	}
	f.Close()

	if err := os.Rename(tmpBin, xrayBinaryPath); err != nil {
		os.Remove(tmpBin)
		return fmt.Errorf("replace binary: %w", err)
	}
	return nil
}

// mergeXrayConfig 将 partial（顶层字段 JSON）合并到现有配置文件，只覆盖 partial 中出现的字段。
func mergeXrayConfig(configPath, partial string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var existing map[string]json.RawMessage
	if err := json.Unmarshal(data, &existing); err != nil {
		return fmt.Errorf("parse existing config: %w", err)
	}
	var patch map[string]json.RawMessage
	if err := json.Unmarshal([]byte(partial), &patch); err != nil {
		return fmt.Errorf("parse patch: %w", err)
	}
	for k, v := range patch {
		existing[k] = v
	}
	out, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return os.WriteFile(configPath, out, 0644)
}
