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
	p.Conn = xrayConnCount(x.apiAddr)
	p.SV = xrayVersion()
	if xrayPortProbe(x.configPath, x.inboundTag) {
		p.Health = "ok"
	} else {
		p.Health = "err"
	}
}

// xrayPortProbe 从配置文件读取监听端口，TCP dial 探测是否可达。
func xrayPortProbe(configPath, inboundTag string) bool {
	port, err := readInboundPort(configPath, inboundTag)
	if err != nil || port <= 0 {
		return false
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// readInboundPort 解析 xray 配置文件，返回指定 inbound tag 的监听端口。
func readInboundPort(configPath, inboundTag string) (int, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return 0, err
	}
	var raw struct {
		Inbounds []struct {
			Tag  string      `json:"tag"`
			Port json.Number `json:"port"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return 0, err
	}
	for _, ib := range raw.Inbounds {
		if ib.Tag == inboundTag {
			n, err := ib.Port.Int64()
			return int(n), err
		}
	}
	return 0, fmt.Errorf("inbound tag %q not found in %s", inboundTag, configPath)
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

// xrayConnCount 通过 Xray stats API 查询在线用户数
// 返回格式与 swan 保持一致："N,0"
func xrayConnCount(apiAddr string) string {
	out, err := exec.Command("xray", "api", "statsquery",
		"--server="+apiAddr, "--pattern", "user>>>", "--reset=true").Output()
	if err != nil {
		return "0"
	}
	count := countXrayOnline(string(out))
	return strconv.Itoa(count)
}

// countXrayOnline 统计 downlink 不为 0 的用户条目数。
// 兼容新版 JSON 输出和旧版文本输出两种格式。
func countXrayOnline(statsOutput string) int {
	// 尝试 JSON 格式（新版 xray）
	var resp struct {
		Stat []struct {
			Name  string `json:"name"`
			Value int64  `json:"value"`
		} `json:"stat"`
	}
	if err := json.Unmarshal([]byte(statsOutput), &resp); err == nil {
		count := 0
		for _, s := range resp.Stat {
			if strings.Contains(s.Name, ">>>traffic>>>downlink") && s.Value > 0 {
				count++
			}
		}
		return count
	}

	// 回退：旧版文本格式（name value: 123 在同一行）
	count := 0
	for _, line := range strings.Split(statsOutput, "\n") {
		if strings.Contains(line, ">>>traffic>>>downlink") && strings.Contains(line, "value:") {
			parts := strings.SplitN(line, "value:", 2)
			if len(parts) < 2 {
				continue
			}
			val := strings.TrimSpace(strings.Trim(parts[1], " >"))
			n, err := strconv.ParseInt(val, 10, 64)
			if err == nil && n > 0 {
				count++
			}
		}
	}
	return count
}
