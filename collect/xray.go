//go:build xray

package collect

import (
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// xrayProvider 采集 Xray 连接数和版本
// 环境变量：
//
//	XRAY_API_ADDR   Xray API 地址（默认 127.0.0.1:10085）
type xrayProvider struct {
	apiAddr string
}

func NewXrayProvider(apiAddr string) MetricProvider {
	return &xrayProvider{apiAddr: apiAddr}
}

func (x *xrayProvider) Collect(p *Payload) {
	p.Conn = xrayConnCount(x.apiAddr)
	p.SV = xrayVersion()
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

// countXrayOnline 统计 downlink 不为 0 的用户条目数
func countXrayOnline(statsOutput string) int {
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
