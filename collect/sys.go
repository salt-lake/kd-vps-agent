package collect

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// sysProvider 采集内存、CPU、磁盘基础系统指标
type sysProvider struct {
	prevIdle  uint64
	prevTotal uint64
}

func NewSysProvider() MetricProvider { return &sysProvider{} }

func (s *sysProvider) Collect(p *Payload) {
	p.Mem = getMemPercent()
	p.CPU = s.getCPUPercent()
	p.Disk = getDiskPercent()
}

func getMemPercent() string {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return ""
	}
	return parseMemInfo(string(data))
}

func parseMemInfo(data string) string {
	var total, available int64
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseInt(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			available = v
		}
	}
	if total <= 0 {
		return ""
	}
	return strconv.Itoa(int((total-available)*100 / total))
}

// getCPUPercent 计算两次采集之间的 CPU 使用率
// 第一次调用因无历史数据返回空字符串
func (s *sysProvider) getCPUPercent() string {
	idle, total, err := readCPUStat()
	if err != nil {
		return ""
	}
	if s.prevTotal == 0 {
		s.prevIdle = idle
		s.prevTotal = total
		return ""
	}
	deltaIdle := idle - s.prevIdle
	deltaTotal := total - s.prevTotal
	s.prevIdle = idle
	s.prevTotal = total
	if deltaTotal == 0 {
		return "0"
	}
	used := (deltaTotal - deltaIdle) * 100 / deltaTotal
	return strconv.FormatUint(used, 10)
}

// readCPUStat 读取 /proc/stat 第一行，返回 idle 和 total ticks
func readCPUStat() (idle, total uint64, err error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, err
	}
	idle, total = parseCPUStat(string(data))
	return idle, total, nil
}

func parseCPUStat(data string) (idle, total uint64) {
	for _, line := range strings.Split(data, "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		// fields: cpu user nice system idle iowait irq softirq steal guest guest_nice
		if len(fields) < 5 {
			break
		}
		for i, f := range fields[1:] {
			v, _ := strconv.ParseUint(f, 10, 64)
			total += v
			if i == 3 { // idle
				idle = v
			}
		}
		return idle, total
	}
	return 0, 0
}

func getDiskPercent() string {
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		return ""
	}
	total := stat.Blocks * uint64(stat.Bsize)
	free := stat.Bfree * uint64(stat.Bsize)
	if total == 0 {
		return ""
	}
	used := (total - free) * 100 / total
	return strconv.FormatUint(used, 10)
}
