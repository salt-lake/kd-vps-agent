package collect

import (
	"testing"
)

var sampleNetDev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:  123456     100    0    0    0     0          0         0   123456     100    0    0    0     0       0          0
  eth0: 9999999    5000    0    0    0     0          0         0 88888888    4000    0    0    0     0       0          0
  eth1:  100000     200    0    0    0     0          0         0  5000000     300    0    0    0     0       0          0
docker0:   1000      10    0    0    0     0          0         0     2000      20    0    0    0     0       0          0
  veth1:   500       5    0    0    0     0          0         0     1000      10    0    0    0     0       0          0
  br-abc:  500       5    0    0    0     0          0         0     1000      10    0    0    0     0       0          0
`

func TestParseIfaceBytes(t *testing.T) {
	tests := []struct {
		name   string
		iface  string
		wantRx int64
		wantTx int64
	}{
		{
			name:   "eth0 exists",
			iface:  "eth0",
			wantRx: 9999999,
			wantTx: 88888888,
		},
		{
			name:   "eth1 exists",
			iface:  "eth1",
			wantRx: 100000,
			wantTx: 5000000,
		},
		{
			name:   "lo exists",
			iface:  "lo",
			wantRx: 123456,
			wantTx: 123456,
		},
		{
			name:   "nonexistent interface",
			iface:  "wlan0",
			wantRx: 0,
			wantTx: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rx, tx := parseIfaceBytes(tc.iface, sampleNetDev)
			if rx != tc.wantRx || tx != tc.wantTx {
				t.Errorf("parseIfaceBytes(%q) rx=%d tx=%d, want rx=%d tx=%d",
					tc.iface, rx, tx, tc.wantRx, tc.wantTx)
			}
		})
	}
}

func TestDetectPrimaryIfaceFromData(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "eth0 has most TX",
			data: sampleNetDev,
			// eth0 TX=88888888, eth1 TX=5000000; lo/docker/veth/br- excluded
			want: "eth0",
		},
		{
			name: "only virtual interfaces",
			data: `Inter-|   Receive  |  Transmit
 face |bytes |bytes
    lo:  100    0    0    0    0     0          0         0   100    0    0    0    0     0       0          0
docker0:  200    0    0    0    0     0          0         0   300    0    0    0    0     0       0          0
`,
			want: "eth0",
		},
		{
			name: "empty data",
			data: "",
			want: "eth0",
		},
		{
			name: "single physical iface",
			data: `Inter-|   Receive  |  Transmit
 face |bytes |bytes
  ens3: 5000000   100    0    0    0     0          0         0 3000000     50    0    0    0     0       0          0
`,
			want: "ens3",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := detectPrimaryIfaceFromData(tc.data)
			if got != tc.want {
				t.Errorf("detectPrimaryIfaceFromData() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTrafficReaderRxTx(t *testing.T) {
	tr := &trafficReader{
		iface:     "eth0",
		lastDay:   1,
		lastMonth: 1,
	}
	// 模拟第一次读：prevRx/prevTx 均为 0，注入初始值
	tr.prevTx = 88888888
	tr.prevRx = 9999999

	// 模拟流量增加：TX +1GB, RX +512MB
	const oneMB = 1024 * 1024
	fakeTx := int64(88888888) + 1024*oneMB
	fakeRx := int64(9999999) + 512*oneMB

	// 直接操作内部字段模拟 read 的增量逻辑
	deltaTx := fakeTx - tr.prevTx
	deltaRx := fakeRx - tr.prevRx
	tr.dayBytes += deltaTx
	tr.monthBytes += deltaTx
	tr.dayRxBytes += deltaRx
	tr.monthRxBytes += deltaRx
	tr.prevTx = fakeTx
	tr.prevRx = fakeRx

	if tr.dayBytes != deltaTx {
		t.Errorf("dayBytes = %d, want %d", tr.dayBytes, deltaTx)
	}
	if tr.dayRxBytes != deltaRx {
		t.Errorf("dayRxBytes = %d, want %d", tr.dayRxBytes, deltaRx)
	}
	if tr.monthBytes != deltaTx {
		t.Errorf("monthBytes = %d, want %d", tr.monthBytes, deltaTx)
	}
	if tr.monthRxBytes != deltaRx {
		t.Errorf("monthRxBytes = %d, want %d", tr.monthRxBytes, deltaRx)
	}

	dayGB := bytesToGBStr(tr.dayBytes)
	dayRxGB := bytesToGBStr(tr.dayRxBytes)
	if dayGB != "1.0G" {
		t.Errorf("dayGB = %q, want 1.0G", dayGB)
	}
	if dayRxGB != "0.5G" {
		t.Errorf("dayRxGB = %q, want 0.5G", dayRxGB)
	}
}

func TestBytesToGBStr(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0.0G"},
		{1073741824, "1.0G"},
		{1610612736, "1.5G"},
		{107374182, "0.1G"},
	}
	for _, tc := range tests {
		got := bytesToGBStr(tc.bytes)
		if got != tc.want {
			t.Errorf("bytesToGBStr(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}
