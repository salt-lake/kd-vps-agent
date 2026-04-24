//go:build xray

package ratelimit

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

const fallbackIface = "eth0"

// DetectIface 自动探测默认出口网卡。
// 优先：`ip route get 1.1.1.1` 解析 dev；失败回退 "eth0"。
func DetectIface(ctx context.Context) string {
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c, "ip", "route", "get", "1.1.1.1").Output()
	if err != nil {
		return fallbackIface
	}
	if name := parseIfaceFromIPRouteOutput(string(out)); name != "" {
		return name
	}
	return fallbackIface
}

// parseIfaceFromIPRouteOutput 从 `ip route get` 输出里提取 dev 后的第一个 token。
func parseIfaceFromIPRouteOutput(out string) string {
	fields := strings.Fields(out)
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}
