//go:build xray

package ratelimit

import "fmt"

// ClassIDMinorFromMark 将 tier 的 markId 映射为 HTB class minor 号。
// 约定 mark=N → classid 1:N*10，保证 handle 数字空间清晰。
func ClassIDMinorFromMark(mark int) int {
	return mark * 10
}

func buildRootQdiscArgs(iface string) []string {
	return []string{"qdisc", "replace", "dev", iface, "root", "handle", "1:", "htb", "default", "999"}
}

func buildTierClassArgs(iface string, classidMinor, poolMbps int) []string {
	return []string{
		"class", "replace", "dev", iface,
		"parent", "1:", "classid", fmt.Sprintf("1:%d", classidMinor),
		"htb", "rate", fmt.Sprintf("%dmbit", poolMbps), "ceil", fmt.Sprintf("%dmbit", poolMbps),
	}
}

func buildTierLeafQdiscArgs(iface string, classidMinor int) []string {
	return []string{
		"qdisc", "replace", "dev", iface,
		"parent", fmt.Sprintf("1:%d", classidMinor),
		"handle", fmt.Sprintf("%d:", classidMinor),
		"fq_codel",
	}
}

// buildTierFilterArgs 把指定 mark 的包分流到 1:<classidMinor>。
// handle 复用 mark 值（fw filter 的 handle 就是要匹配的 mark）。
func buildTierFilterArgs(iface string, mark, classidMinor int) []string {
	return []string{
		"filter", "replace", "dev", iface,
		"protocol", "ip", "parent", "1:",
		"prio", "1", "handle", fmt.Sprintf("%d", mark),
		"fw", "flowid", fmt.Sprintf("1:%d", classidMinor),
	}
}

func buildTeardownArgs(iface string) []string {
	return []string{"qdisc", "del", "dev", iface, "root"}
}

// NormalizeIptablesPort 把 port range 字符串（如 "34521-34524" 或 "443"）
// 转换为 iptables multiport 可接受的格式（"34521:34524" / "443"）。
// 同时供 xray 包的防火墙规则下发复用。
func NormalizeIptablesPort(portRange string) string {
	// iptables 用 `:` 作为端口范围分隔符
	for i := range portRange {
		if portRange[i] == '-' {
			return portRange[:i] + ":" + portRange[i+1:]
		}
	}
	return portRange
}

// buildIptablesAddMarkArgs 生成 iptables -t mangle -A OUTPUT -p tcp --sport <range> -j MARK --set-mark <mark>
// 这条规则匹配 agent 节点从 xray inbound 监听端口发出的所有包（即发给用户的下行流量），打上指定 mark。
func buildIptablesAddMarkArgs(portRange string, mark int) []string {
	return []string{
		"-t", "mangle", "-A", "OUTPUT",
		"-p", "tcp", "--sport", NormalizeIptablesPort(portRange),
		"-j", "MARK", "--set-mark", fmt.Sprintf("%d", mark),
	}
}

// buildIptablesCheckMarkArgs 等价于上面的 -A 但用 -C（探测规则是否已存在）。
// exit code 0 = 存在；非 0 = 不存在。
func buildIptablesCheckMarkArgs(portRange string, mark int) []string {
	return []string{
		"-t", "mangle", "-C", "OUTPUT",
		"-p", "tcp", "--sport", NormalizeIptablesPort(portRange),
		"-j", "MARK", "--set-mark", fmt.Sprintf("%d", mark),
	}
}

// buildIptablesDelMarkArgs 等价于上面但用 -D（删除一次匹配的规则）。
func buildIptablesDelMarkArgs(portRange string, mark int) []string {
	return []string{
		"-t", "mangle", "-D", "OUTPUT",
		"-p", "tcp", "--sport", NormalizeIptablesPort(portRange),
		"-j", "MARK", "--set-mark", fmt.Sprintf("%d", mark),
	}
}
