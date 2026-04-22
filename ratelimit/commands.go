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
