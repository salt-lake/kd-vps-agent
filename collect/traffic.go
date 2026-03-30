package collect

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

const trafficStateFile = "/var/lib/node-agent/traffic.json"

type trafficProvider struct {
	tr *trafficReader
}

func NewTrafficProvider(iface string) MetricProvider {
	return &trafficProvider{tr: newTrafficReader(iface)}
}

func (t *trafficProvider) Collect(p *Payload) {
	p.D, p.M, p.DR, p.MR = t.tr.read()
}

// trafficState 持久化到磁盘的状态
type trafficState struct {
	DayBytes     int64 `json:"day_bytes"`
	MonthBytes   int64 `json:"month_bytes"`
	DayRxBytes   int64 `json:"day_rx_bytes"`
	MonthRxBytes int64 `json:"month_rx_bytes"`
	LastDay      int   `json:"last_day"`
	LastMonth    int   `json:"last_month"`
}

// trafficReader 读取 /proc/net/dev 追踪出站（TX）和入站（RX）流量累积
type trafficReader struct {
	iface        string
	prevTx       int64
	prevRx       int64
	dayBytes     int64
	monthBytes   int64
	dayRxBytes   int64
	monthRxBytes int64
	lastDay      int
	lastMonth    int
}

func newTrafficReader(iface string) *trafficReader {
	now := time.Now()
	tr := &trafficReader{
		iface:     iface,
		lastDay:   now.Day(),
		lastMonth: int(now.Month()),
	}
	// 从文件恢复历史累计值
	if s, err := loadTrafficState(); err == nil {
		if s.LastDay == now.Day() {
			tr.dayBytes = s.DayBytes
			tr.dayRxBytes = s.DayRxBytes
		}
		if s.LastMonth == int(now.Month()) {
			tr.monthBytes = s.MonthBytes
			tr.monthRxBytes = s.MonthRxBytes
		}
	}
	tr.prevRx, tr.prevTx = readIfaceBytes(iface)
	return tr
}

func (tr *trafficReader) read() (dayGB, monthGB, dayRxGB, monthRxGB string) {
	now := time.Now()
	rx, tx := readIfaceBytes(tr.iface)

	deltaTx := tx - tr.prevTx
	if deltaTx < 0 {
		deltaTx = 0
	}
	deltaRx := rx - tr.prevRx
	if deltaRx < 0 {
		deltaRx = 0
	}

	dayReset := now.Day() != tr.lastDay
	monthReset := int(now.Month()) != tr.lastMonth

	if dayReset {
		tr.dayBytes = 0
		tr.dayRxBytes = 0
		tr.lastDay = now.Day()
	}
	if monthReset {
		tr.monthBytes = 0
		tr.monthRxBytes = 0
		tr.lastMonth = int(now.Month())
	}

	tr.dayBytes += deltaTx
	tr.monthBytes += deltaTx
	tr.dayRxBytes += deltaRx
	tr.monthRxBytes += deltaRx
	tr.prevTx = tx
	tr.prevRx = rx

	if deltaTx > 0 || deltaRx > 0 || dayReset || monthReset {
		if err := saveTrafficState(trafficState{
			DayBytes:     tr.dayBytes,
			MonthBytes:   tr.monthBytes,
			DayRxBytes:   tr.dayRxBytes,
			MonthRxBytes: tr.monthRxBytes,
			LastDay:      tr.lastDay,
			LastMonth:    tr.lastMonth,
		}); err != nil {
			log.Printf("traffic: save state err=%v", err)
		}
	}

	return bytesToGBStr(tr.dayBytes), bytesToGBStr(tr.monthBytes), bytesToGBStr(tr.dayRxBytes), bytesToGBStr(tr.monthRxBytes)
}

func loadTrafficState() (trafficState, error) {
	data, err := os.ReadFile(trafficStateFile)
	if err != nil {
		return trafficState{}, err
	}
	var s trafficState
	return s, json.Unmarshal(data, &s)
}

func saveTrafficState(s trafficState) error {
	if err := os.MkdirAll("/var/lib/node-agent", 0755); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(trafficStateFile, data, 0644)
}

func readIfaceBytes(iface string) (rx, tx int64) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0
	}
	return parseIfaceBytes(iface, string(data))
}

func parseIfaceBytes(iface, data string) (rx, tx int64) {
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, iface+":") {
			continue
		}
		line = strings.TrimPrefix(line, iface+":")
		fields := strings.Fields(line)
		if len(fields) < 9 {
			return 0, 0
		}
		rx, _ = strconv.ParseInt(fields[0], 10, 64)
		tx, _ = strconv.ParseInt(fields[8], 10, 64)
		return rx, tx
	}
	return 0, 0
}

// DetectPrimaryIface 从 /proc/net/dev 选出 TX 字节最多的物理网卡。
// 排除 lo、docker*、veth*、br-* 等虚拟接口。
// 当 NET_IFACE 未设置或设置的网卡 TX 为 0 时用于自动探测。
func DetectPrimaryIface() string {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return "eth0"
	}
	return detectPrimaryIfaceFromData(string(data))
}

func detectPrimaryIfaceFromData(data string) string {
	var bestIface string
	var bestTX int64
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		name := strings.TrimSpace(line[:idx])
		if name == "lo" ||
			strings.HasPrefix(name, "docker") ||
			strings.HasPrefix(name, "veth") ||
			strings.HasPrefix(name, "br-") {
			continue
		}
		rest := line[idx+1:]
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			continue
		}
		tx, _ := strconv.ParseInt(fields[8], 10, 64)
		if tx > bestTX {
			bestTX = tx
			bestIface = name
		}
	}
	if bestIface == "" {
		return "eth0"
	}
	return bestIface
}

func bytesToGBStr(b int64) string {
	return fmt.Sprintf("%.1fG", float64(b)/(1024*1024*1024))
}
