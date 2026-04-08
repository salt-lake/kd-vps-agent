//go:build xray

package collect

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// xrayProvider 采集 Xray 连接数、版本和端口可达性
type xrayProvider struct {
	apiAddr    string
	configPath string
	inboundTag string
}

func NewXrayProvider(apiAddr, configPath, inboundTag string) MetricProvider {
	return &xrayProvider{apiAddr: apiAddr, configPath: configPath, inboundTag: inboundTag}
}

func (x *xrayProvider) Collect(p *Payload) {
	p.Conn = xrayConnCount(x.configPath, x.inboundTag)
	p.SV = xrayVersion()
	if xrayPortProbe(x.configPath, x.inboundTag) {
		p.Health = "ok"
	} else {
		p.Health = "err"
	}
}

// xrayPortProbe 从配置文件读取监听端口，TCP dial 探测是否可达。
func xrayPortProbe(configPath, inboundTag string) bool {
	pr, err := readInboundPort(configPath, inboundTag)
	if err != nil || pr.Start <= 0 {
		return false
	}
	addr := fmt.Sprintf("127.0.0.1:%d", pr.Start)
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// portRange 表示一个端口或端口范围。
type portRange struct {
	Start int
	End   int // 单端口时 End == Start
}

// readInboundPort 解析 xray 配置文件，返回指定 inbound tag 的监听端口（支持范围格式如 "56771-56774"）。
func readInboundPort(configPath, inboundTag string) (portRange, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return portRange{}, err
	}
	var raw struct {
		Inbounds []struct {
			Tag  string          `json:"tag"`
			Port json.RawMessage `json:"port"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return portRange{}, err
	}
	for _, ib := range raw.Inbounds {
		if ib.Tag != inboundTag {
			continue
		}
		return parsePort(ib.Port)
	}
	return portRange{}, fmt.Errorf("inbound tag %q not found in %s", inboundTag, configPath)
}

// parsePort 解析 JSON 端口值：数字 443 或字符串 "56771-56774"。
func parsePort(raw json.RawMessage) (portRange, error) {
	// 尝试数字
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return portRange{n, n}, nil
	}
	// 尝试字符串（可能是范围）
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return portRange{}, fmt.Errorf("invalid port value: %s", string(raw))
	}
	if parts := strings.SplitN(s, "-", 2); len(parts) == 2 {
		start, err1 := strconv.Atoi(parts[0])
		end, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			return portRange{}, fmt.Errorf("invalid port range: %s", s)
		}
		return portRange{start, end}, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return portRange{}, fmt.Errorf("invalid port: %s", s)
	}
	return portRange{n, n}, nil
}

var xrayVersionRe = regexp.MustCompile(`Xray\s+(\S+)`)

// xrayVersion 从 `xray version` 输出解析版本号
// 输出示例："Xray 1.8.4 (Xray, Penetrates Everything) ..."
func xrayVersion() string {
	out, err := exec.Command("xray", "version").Output()
	if err != nil {
		return "error"
	}
	m := xrayVersionRe.FindStringSubmatch(string(out))
	if m == nil {
		return ""
	}
	return m[1]
}

// xrayConnCount 统计 xray 监听端口上的唯一源 IP 数作为在线连接数。
// xray v26 的 stats API（QueryStats）有服务端 bug，CLI 和 gRPC 直调均失败，
// 因此改用 ss 统计 TCP 连接。
func xrayConnCount(configPath, inboundTag string) string {
	pr, err := readInboundPort(configPath, inboundTag)
	if err != nil || pr.Start <= 0 {
		return "0"
	}

	// ss 过滤指定端口（范围）的 ESTABLISHED 连接，统计唯一源 IP
	var filter string
	if pr.Start == pr.End {
		filter = fmt.Sprintf("sport = :%d", pr.Start)
	} else {
		filter = fmt.Sprintf("sport >= :%d and sport <= :%d", pr.Start, pr.End)
	}
	out, err := exec.Command("ss", "-tn", "state", "established", filter).Output()
	if err != nil {
		return "0"
	}

	ips := make(map[string]struct{})
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		peer := fields[3] // 格式 "1.2.3.4:12345" 或 "[::ffff:1.2.3.4]:12345"
		if idx := strings.LastIndex(peer, ":"); idx > 0 {
			ip := peer[:idx]
			ip = strings.Trim(ip, "[]")
			ip = strings.TrimPrefix(ip, "::ffff:")
			if ip != "" && ip != "127.0.0.1" && ip != "::1" {
				ips[ip] = struct{}{}
			}
		}
	}
	return strconv.Itoa(len(ips))
}
