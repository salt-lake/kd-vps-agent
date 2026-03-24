package collect

import "os/exec"

// Payload 上报数据结构（所有协议共用字段）
type Payload struct {
	C      string `json:"c"`
	M      string `json:"m"`       // 月流量
	D      string `json:"d"`       // 日流量
	Conn   string `json:"conn"`    // 连接数
	Mem    string `json:"mem"`     // 内存占用百分比
	CPU    string `json:"cpu"`     // CPU 占用百分比
	Disk   string `json:"disk"`    // 磁盘占用百分比
	SV     string `json:"s_v"`               // 协议软件版本
	AV     string `json:"a_v"`               // agent 版本
	NodeID string `json:"node_id"`           // 节点 ID
	Health string `json:"health,omitempty"` // 代理端口可达性："ok" / "err"（xray 专用）
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
