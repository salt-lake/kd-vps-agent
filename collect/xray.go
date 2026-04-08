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

	stats "github.com/salt-lake/kd-vps-agent/xray/proto/app/stats/command"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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

// xrayConnCount 通过 gRPC 直连 Xray stats API 查询在线用户数。
// xray v26 的 CLI statsquery 命令有 bug（"QueryStats only works its own stats.Manager"），
// 因此改用 gRPC 直接调用。
func xrayConnCount(apiAddr string) string {
	conn, err := grpc.NewClient(apiAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return "0"
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	client := stats.NewStatsServiceClient(conn)
	resp, err := client.QueryStats(ctx, &stats.QueryStatsRequest{
		Pattern: "user>>>",
		Reset_:  true,
	})
	if err != nil {
		return "0"
	}

	count := 0
	for _, s := range resp.GetStat() {
		if strings.Contains(s.Name, ">>>traffic>>>downlink") && s.Value > 0 {
			count++
		}
	}
	return strconv.Itoa(count)
}
