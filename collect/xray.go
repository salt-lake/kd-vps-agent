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

// xrayProvider 采集 Xray 连接数、版本和端口可达性。
// 迁移后支持多 inbound（proxy-vip / proxy-svip），Collect 会聚合所有 proxy* tag 的数据。
type xrayProvider struct {
	apiAddr    string
	configPath string
}

func NewXrayProvider(apiAddr, configPath string) MetricProvider {
	return &xrayProvider{apiAddr: apiAddr, configPath: configPath}
}

func (x *xrayProvider) Collect(p *Payload) {
	p.SV = xrayVersion()
	inbounds, err := readProxyInbounds(x.configPath)
	if err != nil || len(inbounds) == 0 {
		p.Conn = "0"
		p.Health = "err"
		return
	}
	p.Conn, p.ConnByTag = aggregateConnCounts(inbounds)
	if probeAllInbounds(inbounds) {
		p.Health = "ok"
	} else {
		p.Health = "err"
	}
}

// proxyInbound 是 xray config 里一个 proxy* tag 的 inbound 信息。
type proxyInbound struct {
	Tag       string
	PortRange portRange
}

// readProxyInbounds 返回 xray config 里所有 tag 以 "proxy" 开头的 inbound。
// 兼容迁移前（单 "proxy"）和迁移后（"proxy-vip" + "proxy-svip"）两种结构。
// tag 排序按 inbound 在文件里的先后，测试可稳定断言。
func readProxyInbounds(configPath string) ([]proxyInbound, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Inbounds []struct {
			Tag  string          `json:"tag"`
			Port json.RawMessage `json:"port"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	var out []proxyInbound
	for _, ib := range raw.Inbounds {
		if !strings.HasPrefix(ib.Tag, "proxy") {
			continue
		}
		pr, err := parsePort(ib.Port)
		if err != nil || pr.Start <= 0 {
			continue
		}
		out = append(out, proxyInbound{Tag: ib.Tag, PortRange: pr})
	}
	return out, nil
}

// portRange 表示一个端口或端口范围。
type portRange struct {
	Start int
	End   int // 单端口时 End == Start
}

// parsePort 解析 JSON 端口值：数字 443 或字符串 "56771-56774"。
func parsePort(raw json.RawMessage) (portRange, error) {
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return portRange{n, n}, nil
	}
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

// xrayVersion 从 `xray version` 输出解析版本号。
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

// aggregateConnCounts 遍历所有 proxy inbound，统计：
//  1. 去重后的全局唯一源 IP 数（跨 tier 同 IP 只算一次）→ Payload.Conn
//  2. 每个 tag 独立的唯一源 IP 数 → Payload.ConnByTag
// 单 "proxy" inbound 节点 byTag 退化为 1 个 entry，与 Conn 相等，不造成信息丢失。
func aggregateConnCounts(inbounds []proxyInbound) (total string, byTag map[string]string) {
	return aggregateConnCountsWith(inbounds, collectSourceIPs)
}

// aggregateConnCountsWith 和 aggregateConnCounts 逻辑相同，仅在单元测试里通过注入 collector 函数跳过 ss 调用。
func aggregateConnCountsWith(inbounds []proxyInbound, collect func(portRange) map[string]struct{}) (total string, byTag map[string]string) {
	global := make(map[string]struct{})
	byTag = make(map[string]string, len(inbounds))
	for _, ib := range inbounds {
		ips := collect(ib.PortRange)
		byTag[ib.Tag] = strconv.Itoa(len(ips))
		for ip := range ips {
			global[ip] = struct{}{}
		}
	}
	return strconv.Itoa(len(global)), byTag
}

// collectSourceIPs 用 ss 查指定端口区间上 ESTABLISHED 连接的唯一源 IP 集合。
// 排除 127.0.0.1 / ::1 回环（xray 自身 / api 管理连接用）。
func collectSourceIPs(pr portRange) map[string]struct{} {
	var filter string
	if pr.Start == pr.End {
		filter = fmt.Sprintf("sport = :%d", pr.Start)
	} else {
		filter = fmt.Sprintf("sport >= :%d and sport <= :%d", pr.Start, pr.End)
	}
	out, err := exec.Command("ss", "-tn", "state", "established", filter).Output()
	ips := make(map[string]struct{})
	if err != nil {
		return ips
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		peer := fields[3] // "1.2.3.4:12345" 或 "[::ffff:1.2.3.4]:12345"
		idx := strings.LastIndex(peer, ":")
		if idx <= 0 {
			continue
		}
		ip := strings.Trim(peer[:idx], "[]")
		ip = strings.TrimPrefix(ip, "::ffff:")
		if ip == "" || ip == "127.0.0.1" || ip == "::1" {
			continue
		}
		ips[ip] = struct{}{}
	}
	return ips
}

// probeAllInbounds 对每个 inbound 的起始端口做 TCP dial 探测。
// 任一失败即返回 false（全部通才算健康，避免迁移后 svip 掉线被漏报）。
func probeAllInbounds(inbounds []proxyInbound) bool {
	if len(inbounds) == 0 {
		return false
	}
	for _, ib := range inbounds {
		addr := fmt.Sprintf("127.0.0.1:%d", ib.PortRange.Start)
		conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			return false
		}
		conn.Close()
	}
	return true
}
