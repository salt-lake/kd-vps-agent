//go:build xray

package collect

import "testing"

type fakeStatsFn struct {
	stats map[string]TierStatsDTO
	err   error
}

func (f *fakeStatsFn) collect() (map[string]TierStatsDTO, error) {
	return f.stats, f.err
}

type fakeErr string

func (e fakeErr) Error() string { return string(e) }

func TestTcStatsProvider_Fills(t *testing.T) {
	fake := &fakeStatsFn{
		stats: map[string]TierStatsDTO{
			"1:10": {ClassID: "1:10", SentBytes: 100, Dropped: 5},
		},
	}
	p := &TcStatsProvider{fetcher: fake.collect}
	var pl Payload
	p.Collect(&pl)
	if len(pl.TcStats) != 1 || pl.TcStats["1:10"].SentBytes != 100 {
		t.Errorf("TcStats not populated: %+v", pl.TcStats)
	}
}

func TestTcStatsProvider_SilentOnError(t *testing.T) {
	fake := &fakeStatsFn{err: fakeErr("mock")}
	p := &TcStatsProvider{fetcher: fake.collect}
	var pl Payload
	p.Collect(&pl) // 不应 panic
	if pl.TcStats != nil {
		t.Errorf("TcStats should stay nil on error, got %+v", pl.TcStats)
	}
}

func TestTcStatsProvider_EmptyStats(t *testing.T) {
	fake := &fakeStatsFn{stats: map[string]TierStatsDTO{}}
	p := &TcStatsProvider{fetcher: fake.collect}
	var pl Payload
	p.Collect(&pl)
	if pl.TcStats != nil {
		t.Errorf("TcStats should stay nil on empty stats, got %+v", pl.TcStats)
	}
}

func TestTcStatsProvider_Disabled(t *testing.T) {
	p := NewTcStatsProvider("eth0", false)
	var pl Payload
	p.Collect(&pl) // disabled 时 fetcher 返回 nil，nil
	if pl.TcStats != nil {
		t.Errorf("TcStats should stay nil when disabled, got %+v", pl.TcStats)
	}
}
