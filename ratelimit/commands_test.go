//go:build xray

package ratelimit

import (
	"reflect"
	"testing"
)

func TestBuildRootQdiscArgs(t *testing.T) {
	got := buildRootQdiscArgs("eth0")
	want := []string{"qdisc", "replace", "dev", "eth0", "root", "handle", "1:", "htb", "default", "999"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildTierClassArgs(t *testing.T) {
	got := buildTierClassArgs("eth0", 10, 200)
	want := []string{"class", "replace", "dev", "eth0", "parent", "1:", "classid", "1:10", "htb", "rate", "200mbit", "ceil", "200mbit"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildTierLeafQdiscArgs(t *testing.T) {
	got := buildTierLeafQdiscArgs("eth0", 10)
	want := []string{"qdisc", "replace", "dev", "eth0", "parent", "1:10", "handle", "10:", "fq_codel"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildTierFilterArgs(t *testing.T) {
	got := buildTierFilterArgs("eth0", 1, 10)
	want := []string{"filter", "replace", "dev", "eth0", "protocol", "ip", "parent", "1:", "prio", "1", "handle", "1", "fw", "flowid", "1:10"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildTeardownArgs(t *testing.T) {
	got := buildTeardownArgs("eth0")
	want := []string{"qdisc", "del", "dev", "eth0", "root"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestClassIDMinorFromMark(t *testing.T) {
	tests := []struct {
		mark int
		want int
	}{
		{1, 10},
		{2, 20},
		{3, 30},
	}
	for _, tc := range tests {
		if got := ClassIDMinorFromMark(tc.mark); got != tc.want {
			t.Errorf("mark=%d got %d, want %d", tc.mark, got, tc.want)
		}
	}
}

func TestNormalizeIptablesPort(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"34521-34524", "34521:34524"}, // 范围转冒号分隔
		{"443", "443"},                 // 单端口保持
		{"", ""},
	}
	for _, tc := range tests {
		if got := normalizeIptablesPort(tc.in); got != tc.want {
			t.Errorf("in=%q got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestBuildIptablesAddMarkArgs(t *testing.T) {
	got := buildIptablesAddMarkArgs("34521-34524", 1)
	want := []string{"-t", "mangle", "-A", "OUTPUT", "-p", "tcp", "--sport", "34521:34524", "-j", "MARK", "--set-mark", "1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildIptablesCheckMarkArgs(t *testing.T) {
	got := buildIptablesCheckMarkArgs("443", 2)
	want := []string{"-t", "mangle", "-C", "OUTPUT", "-p", "tcp", "--sport", "443", "-j", "MARK", "--set-mark", "2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildIptablesDelMarkArgs(t *testing.T) {
	got := buildIptablesDelMarkArgs("45000-45003", 2)
	want := []string{"-t", "mangle", "-D", "OUTPUT", "-p", "tcp", "--sport", "45000:45003", "-j", "MARK", "--set-mark", "2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
