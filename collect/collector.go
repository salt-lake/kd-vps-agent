package collect

import "os/exec"

// Payload 上报数据结构（所有协议共用字段）
type Payload struct {
	C       string                  `json:"c"`
	M       string                  `json:"m"`                 // 月出站流量
	D       string                  `json:"d"`                 // 日出站流量
	MR      string                  `json:"m_r"`               // 月入站流量
	DR      string                  `json:"d_r"`               // 日入站流量
	Conn    string                  `json:"conn"`              // 连接数（迁移后为所有 proxy* inbound 唯一源 IP 去重合计）
	ConnByTag map[string]string     `json:"connByTag,omitempty"` // 按 inbound tag 细分的在线数（老 agent / 单 inbound 节点不填）
	Mem     string                  `json:"mem"`               // 内存占用百分比
	CPU     string                  `json:"cpu"`               // CPU 占用百分比
	Disk    string                  `json:"disk"`              // 磁盘占用百分比
	SV      string                  `json:"s_v"`               // 协议软件版本
	AV      string                  `json:"a_v"`               // agent 版本
	NodeID  string                  `json:"node_id"`           // 节点 ID
	Health  string                  `json:"health,omitempty"`  // 代理端口可达性："ok" / "err"（xray 专用）
	TcStats map[string]TierStatsDTO `json:"tc_stats,omitempty"` // tc class 统计（仅 xray 限速启用时）
}

// TierStatsDTO tc class 统计快照，key = classid（如 "1:10"）。
type TierStatsDTO struct {
	ClassID      string `json:"classId"`
	SentBytes    uint64 `json:"sent"`
	Dropped      uint64 `json:"dropped"`
	Overlimits   uint64 `json:"overlimits"`
	BacklogBytes uint64 `json:"backlog"`
}

// MetricProvider 单项采集接口，只填自己负责的字段
type MetricProvider interface {
	Collect(p *Payload)
}

// Collector 组合多个 Provider
type Collector interface {
	Collect() Payload
}

type collector struct {
	providers []MetricProvider
}

func NewCollector(providers ...MetricProvider) Collector {
	return &collector{providers: providers}
}

func (c *collector) Collect() Payload {
	p := Payload{C: "tr"}
	for _, pv := range c.providers {
		pv.Collect(&p)
	}
	return p
}

func dockerExec(container string, args ...string) (string, error) {
	cmdArgs := append([]string{"exec", container}, args...)
	out, err := exec.Command("docker", cmdArgs...).Output()
	return string(out), err
}
