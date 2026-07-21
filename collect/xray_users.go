//go:build xray

package collect

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc"

	xraypkg "github.com/salt-lake/kd-vps-agent/xray"
	statscmd "github.com/salt-lake/kd-vps-agent/xray/proto/app/stats/command"
)

// xrayUserTraffic 通过 StatsService.QueryStats(pattern="user>>>", reset=true)
// 拉取自上次采集以来的每用户流量增量（字节）。
//
// reset=true 是破坏性读取：取走即清零，语义为"两次采集之间的增量"。上报丢失
// 即该批增量丢失——排行榜场景可接受，换取无状态实现。
// RPC 失败（老 xray 无接口/连接失败）返回 nil，本 tick 静默跳过。
func xrayUserTraffic(conn *grpc.ClientConn) map[string][2]int64 {
	if conn == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := statscmd.NewStatsServiceClient(conn)
	resp, err := client.QueryStats(ctx, &statscmd.QueryStatsRequest{Pattern: "user>>>", Reset_: true})
	if err != nil {
		return nil
	}
	return aggregateUserStats(resp.GetStat())
}

// aggregateUserStats 把 user 计数器聚合为 {uuid: [uplink, downlink]}，丢弃零值；
// 无有效数据返回 nil（配合 Payload 的 omitempty 省掉整个字段）。
func aggregateUserStats(stats []*statscmd.Stat) map[string][2]int64 {
	out := make(map[string][2]int64)
	for _, s := range stats {
		uuid, uplink, ok := parseUserCounter(s.GetName())
		if !ok || s.GetValue() <= 0 {
			continue
		}
		v := out[uuid]
		if uplink {
			v[0] += s.GetValue()
		} else {
			v[1] += s.GetValue()
		}
		out[uuid] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseUserCounter 解析计数器名 "user>>>xray@<uuid>>>>traffic>>>uplink|downlink"。
// email 前缀与注入侧共享 xray.EmailPrefix，剥掉后还原 uuid；无该前缀的 email
// （如配置文件里的静态占位用户 default@test）不是本系统注入的用户，跳过。
func parseUserCounter(name string) (uuid string, uplink bool, ok bool) {
	parts := strings.Split(name, ">>>")
	if len(parts) != 4 || parts[0] != "user" || parts[2] != "traffic" {
		return "", false, false
	}
	uuid = strings.TrimPrefix(parts[1], xraypkg.EmailPrefix)
	if uuid == "" || uuid == parts[1] {
		return "", false, false
	}
	switch parts[3] {
	case "uplink":
		return uuid, true, true
	case "downlink":
		return uuid, false, true
	}
	return "", false, false
}
