//go:build xray

package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	statscmd "github.com/salt-lake/kd-vps-agent/xray/proto/app/stats/command"
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
	p.Conn = xrayOnlineUsers(x.apiAddr, x.configPath, x.inboundTag)
	p.U = xrayUserTraffic(x.apiAddr)
	p.SV = xrayVersion()
	if xrayPortProbe(x.configPath, x.inboundTag) {
		p.Health = "ok"
	} else {
		p.Health = "err"
	}
}

// xrayPortProbe 从配置文件读取监听端口，TCP dial 探测是否可达。
func xrayPortProbe(configPath, inboundTag string) bool {
	prs, err := readInboundPort(configPath, inboundTag)
	if err != nil || len(prs) == 0 || prs[0].Start <= 0 {
		return false
	}
	addr := fmt.Sprintf("127.0.0.1:%d", prs[0].Start)
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

// readInboundPort 解析 xray 配置文件，返回指定 inbound tag 的监听端口段列表
// （支持单端口 443、范围 "56771-56774"、以及逗号列表 "56771-56774,443"）。
func readInboundPort(configPath, inboundTag string) ([]portRange, error) {
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
	for _, ib := range raw.Inbounds {
		if ib.Tag != inboundTag {
			continue
		}
		return parsePort(ib.Port)
	}
	return nil, fmt.Errorf("inbound tag %q not found in %s", inboundTag, configPath)
}

// parsePort 解析 JSON 端口值，返回一个或多个端口段。兼容旧格式：
//   - 数字 443           → [{443,443}]
//   - 字符串 "56771-56774" → [{56771,56774}]
//   - 字符串 "443"        → [{443,443}]
//
// 新增支持逗号列表（各段可为单端口或范围）：
//   - "56771-56774,443"  → [{56771,56774},{443,443}]
func parsePort(raw json.RawMessage) ([]portRange, error) {
	// 尝试数字
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return []portRange{{n, n}}, nil
	}
	// 尝试字符串（单端口 / 范围 / 逗号列表）
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("invalid port value: %s", string(raw))
	}
	var out []portRange
	for _, seg := range strings.Split(s, ",") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		if parts := strings.SplitN(seg, "-", 2); len(parts) == 2 {
			start, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
			end, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("invalid port range: %s", seg)
			}
			out = append(out, portRange{start, end})
			continue
		}
		p, err := strconv.Atoi(seg)
		if err != nil {
			return nil, fmt.Errorf("invalid port: %s", seg)
		}
		out = append(out, portRange{p, p})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no valid port in: %s", s)
	}
	return out, nil
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

// xrayOnlineUsers 统计在线用户数。优先走 xray gRPC StatsService.GetAllOnlineUsers
// （工作在用户会话层，同时覆盖 VLESS(TCP) 与 Hysteria2(UDP/QUIC)，前提是配置了
// policy.levels.*.statsUserOnline = true）；当 RPC 报错（极老 xray 无此接口）时，
// 回退到旧的 ss 方案，保证对存量节点向下兼容。
//
// 注意：statsUserOnline 未开启时 GetAllOnlineUsers 返回空列表（非报错），此时会得到
// "0"——rollout 需先给存量节点开启 statsUserOnline 再发新 agent，避免空窗期误报 0。
func xrayOnlineUsers(apiAddr, configPath, inboundTag string) string {
	conn, err := grpc.NewClient(apiAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return xrayConnCountSS(configPath, inboundTag)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	client := statscmd.NewStatsServiceClient(conn)
	resp, err := client.GetAllOnlineUsers(ctx, &statscmd.GetAllOnlineUsersRequest{})
	if err != nil {
		// RPC 不可用（老版本 xray 无此接口 / 连接失败）→ 回退 ss
		return xrayConnCountSS(configPath, inboundTag)
	}
	return strconv.Itoa(len(resp.GetUsers()))
}

// buildSsPortFilter 为一个或多个端口段构造 ss 过滤表达式。
// 单段时无括号；多段时各段用括号包裹再 OR（ss 要求括号内外留空格）。
func buildSsPortFilter(prs []portRange) string {
	seg := func(pr portRange) string {
		if pr.Start == pr.End {
			return fmt.Sprintf("sport = :%d", pr.Start)
		}
		return fmt.Sprintf("sport >= :%d and sport <= :%d", pr.Start, pr.End)
	}
	if len(prs) == 1 {
		return seg(prs[0])
	}
	parts := make([]string, len(prs))
	for i, pr := range prs {
		parts[i] = "( " + seg(pr) + " )"
	}
	return strings.Join(parts, " or ")
}

// xrayConnCountSS 用 ss 统计 xray 监听端口上的唯一源 IP 数（仅 TCP，不含 hy2/UDP）。
// 作为 GetAllOnlineUsers 不可用时的向下兼容兜底。
func xrayConnCountSS(configPath, inboundTag string) string {
	prs, err := readInboundPort(configPath, inboundTag)
	if err != nil || len(prs) == 0 || prs[0].Start <= 0 {
		return "0"
	}
	filter := buildSsPortFilter(prs)
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
		peer := fields[3] // "1.2.3.4:12345" 或 "[::ffff:1.2.3.4]:12345"
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
