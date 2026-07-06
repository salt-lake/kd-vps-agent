//go:build xray

package command

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// xraySetDestSelfTestURL 自测时经代理访问的目标，返回 204 视为出网正常。
const xraySetDestSelfTestURL = "https://www.google.com/generate_204"

// xraySetDestSocksAddr 自测客户端本地 socks 监听地址。
const xraySetDestSocksAddr = "127.0.0.1:38123"

// defaultTestUUID 固定测试用户，与 node-agent xray 包及节点部署脚本保持一致。
const defaultTestUUID = "a1b2c3d4-0000-0000-0000-000000000001"

// xraySetDestReq xray_set_dest 指令载荷。
type xraySetDestReq struct {
	Dest string `json:"dest"` // 形如 "www.cloudflare.com:443"
}

// XraySetDestHandler 处理 xray_set_dest 指令：仅替换 REALITY 伪装目标
// （realitySettings.dest + serverNames），保留本机密钥/shortId/端口不变，
// 重启 xray 并做一次出网自测；任一步失败自动回滚到改动前配置。
type XraySetDestHandler struct {
	configPath string
}

func NewXraySetDestHandler(configPath string) XraySetDestHandler {
	return XraySetDestHandler{configPath: configPath}
}

func (XraySetDestHandler) Name() string { return "xray_set_dest" }

func (h XraySetDestHandler) Handle(data []byte) ([]byte, error) {
	var req xraySetDestReq
	if err := json.Unmarshal(data, &req); err != nil {
		return errResp("invalid payload: " + err.Error()), nil
	}
	dest := strings.TrimSpace(req.Dest)
	if dest == "" {
		return errResp("dest is required"), nil
	}
	serverName := dest
	if i := strings.LastIndex(dest, ":"); i >= 0 {
		serverName = dest[:i]
	}
	if serverName == "" {
		return errResp("invalid dest: empty serverName"), nil
	}

	// 1. 备份当前配置，便于失败回滚。
	backup, err := os.ReadFile(h.configPath)
	if err != nil {
		return errResp("read config: " + err.Error()), nil
	}

	// 2. 写入新 dest / serverNames。
	if err := setXrayDest(h.configPath, dest, serverName); err != nil {
		return errResp("set dest failed: " + err.Error()), nil
	}

	// 3. 重启 xray；失败回滚。
	if err := restartXray(); err != nil {
		rollbackXrayConfig(h.configPath, backup)
		return errResp("restart xray failed: " + err.Error()), nil
	}

	// 4. 出网自测；失败回滚。
	if err := selfTestXrayEgress(h.configPath); err != nil {
		log.Printf("xray_set_dest: self-test failed for dest=%s: %v, rolling back", dest, err)
		rollbackXrayConfig(h.configPath, backup)
		return errResp("egress self-test failed, rolled back: " + err.Error()), nil
	}

	log.Printf("xray_set_dest: dest set to %s, self-test passed", dest)
	return okResp("dest set to " + dest), nil
}

// setXrayDest 读取 config.json，仅替换 inbounds[0].streamSettings.realitySettings
// 的 dest 与 serverNames，其余字段（含 privateKey/shortIds/端口）原样保留。
func setXrayDest(configPath, dest, serverName string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	rs, err := realitySettings(cfg)
	if err != nil {
		return err
	}
	rs["dest"] = dest
	rs["serverNames"] = []string{serverName}

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(configPath, out, 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// realitySettings 从已解析的 config 中定位 inbounds[0].streamSettings.realitySettings。
func realitySettings(cfg map[string]any) (map[string]any, error) {
	inbounds, ok := cfg["inbounds"].([]any)
	if !ok || len(inbounds) == 0 {
		return nil, fmt.Errorf("inbounds missing or empty")
	}
	ib, ok := inbounds[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("inbounds[0] not an object")
	}
	ss, ok := ib["streamSettings"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("streamSettings missing")
	}
	rs, ok := ss["realitySettings"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("realitySettings missing")
	}
	return rs, nil
}

func restartXray() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "systemctl", "restart", "xray").CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	// 确认服务确实起来了。
	out, _ := exec.Command("systemctl", "is-active", "xray").CombinedOutput()
	if strings.TrimSpace(string(out)) != "active" {
		return fmt.Errorf("xray not active after restart: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func rollbackXrayConfig(configPath string, backup []byte) {
	if err := os.WriteFile(configPath, backup, 0644); err != nil {
		log.Printf("xray_set_dest: rollback write failed: %v", err)
		return
	}
	if err := restartXray(); err != nil {
		log.Printf("xray_set_dest: rollback restart failed: %v", err)
	}
}

// selfTestXrayEgress 用固定测试用户 + 本机密钥 + 新 SNI，起一个临时 xray 客户端
// 连本机 inbound，经 socks 访问外网，确认 REALITY 握手 + 出网链路真正可用。
func selfTestXrayEgress(configPath string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	rs, err := realitySettings(cfg)
	if err != nil {
		return err
	}
	priv, _ := rs["privateKey"].(string)
	sni := firstArrayString(rs, "serverNames")
	sid := firstArrayString(rs, "shortIds")
	port := firstInboundPort(cfg)
	if priv == "" || sni == "" || port == "" {
		return fmt.Errorf("incomplete reality config for self-test")
	}
	pub, err := derivePublicKey(priv)
	if err != nil {
		return fmt.Errorf("derive pubkey: %w", err)
	}

	clientCfg := buildSelfTestClientConfig(port, sni, pub, sid)
	tmp, err := os.CreateTemp("", "xray-selftest-*.json")
	if err != nil {
		return fmt.Errorf("temp config: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(clientCfg); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	tmp.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, xrayBinaryPath, "run", "-config", tmpPath)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start test client: %w", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	time.Sleep(1500 * time.Millisecond)
	return probeViaSocks(xraySetDestSocksAddr, xraySetDestSelfTestURL)
}

func buildSelfTestClientConfig(port, sni, pub, sid string) []byte {
	cfg := map[string]any{
		"log": map[string]any{"loglevel": "error"},
		"inbounds": []any{map[string]any{
			"port":     38123,
			"listen":   "127.0.0.1",
			"protocol": "socks",
			"settings": map[string]any{"udp": true},
		}},
		"outbounds": []any{map[string]any{
			"protocol": "vless",
			"settings": map[string]any{"vnext": []any{map[string]any{
				"address": "127.0.0.1",
				"port":    atoiOrZero(port),
				"users": []any{map[string]any{
					"id":         defaultTestUUID,
					"encryption": "none",
					"flow":       "xtls-rprx-vision",
				}},
			}}},
			"streamSettings": map[string]any{
				"network":  "tcp",
				"security": "reality",
				"realitySettings": map[string]any{
					"serverName":  sni,
					"fingerprint": "chrome",
					"publicKey":   pub,
					"shortId":     sid,
				},
			},
		}},
	}
	b, _ := json.Marshal(cfg)
	return b
}

func probeViaSocks(socksAddr, url string) error {
	dialer, err := proxy.SOCKS5("tcp", socksAddr, nil, proxy.Direct)
	if err != nil {
		return fmt.Errorf("socks dialer: %w", err)
	}
	cd, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return fmt.Errorf("socks dialer has no context support")
	}
	tr := &http.Transport{
		DialContext:         cd.DialContext,
		TLSHandshakeTimeout: 8 * time.Second,
	}
	client := &http.Client{Transport: tr, Timeout: 12 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("request via proxy: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("unexpected status %d via proxy", resp.StatusCode)
	}
	return nil
}

// derivePublicKey 用 `xray x25519 -i <priv>` 推导客户端 publicKey。
// xray v26.3.27+ 输出改为 "Password (PublicKey):"（旧版 "Password:"），故用 HasPrefix("Password") 兼容两者。
func derivePublicKey(priv string) (string, error) {
	out, err := exec.Command(xrayBinaryPath, "x25519", "-i", priv).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Password") || strings.HasPrefix(line, "Public key:") {
			parts := strings.Fields(line)
			if len(parts) > 0 {
				return parts[len(parts)-1], nil
			}
		}
	}
	return "", fmt.Errorf("public key not found in x25519 output")
}

// firstArrayString 返回 m[key]（[]any）首个元素的字符串值，缺失/类型不符时返回 ""。
func firstArrayString(m map[string]any, key string) string {
	arr, ok := m[key].([]any)
	if !ok || len(arr) == 0 {
		return ""
	}
	s, _ := arr[0].(string)
	return s
}

// firstInboundPort 返回 inbounds[0].port 的起始端口（支持 "a-b" 段或单端口；string 或 number）。
func firstInboundPort(cfg map[string]any) string {
	inbounds, ok := cfg["inbounds"].([]any)
	if !ok || len(inbounds) == 0 {
		return ""
	}
	ib, ok := inbounds[0].(map[string]any)
	if !ok {
		return ""
	}
	switch v := ib["port"].(type) {
	case string:
		return strings.SplitN(v, "-", 2)[0]
	case float64:
		return fmt.Sprintf("%d", int(v))
	default:
		return ""
	}
}

func atoiOrZero(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
