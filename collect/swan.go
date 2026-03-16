package collect

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// swanProvider 采集 IKEv2/StrongSwan 连接数和版本
type swanProvider struct {
	container string
}

func NewSwanProvider(container string) MetricProvider {
	return &swanProvider{container: container}
}

func (s *swanProvider) Collect(p *Payload) {
	p.Conn = swanConnCount(s.container)
	p.SV = swanVersion(s.container)
}

var (
	swanIPv4Re    = regexp.MustCompile(`^(\d{1,3}\.){3}\d{1,3}$`)
	swanPeerRe    = regexp.MustCompile(`ESTABLISHED.*\.\.\.\s*([^\s\[]+)`)
	swanVersionRe = regexp.MustCompile(`U(\S+?)/`)
)

func swanConnCount(container string) string {
	var out string
	var err error
	if container == "" || container == "none" {
		var b []byte
		b, err = exec.Command("ipsec", "statusall").Output()
		out = string(b)
	} else {
		out, err = dockerExec(container, "ipsec", "statusall")
	}
	if err != nil {
		return "0"
	}
	return parseSwanConnCount(out)
}

func parseSwanConnCount(out string) string {
	v4 := 0
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "ESTABLISHED") {
			continue
		}
		m := swanPeerRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ip := strings.TrimRight(m[1], "[]")
		if swanIPv4Re.MatchString(ip) {
			v4++
		}
	}
	return fmt.Sprintf("%d", v4)
}

func swanVersion(container string) string {
	var out string
	var err error
	if container == "" || container == "none" {
		var b []byte
		b, err = exec.Command("ipsec", "version").Output()
		out = string(b)
	} else {
		out, err = dockerExec(container, "ipsec", "version")
	}
	if err != nil {
		return ""
	}
	// "Linux strongSwan U6.0.4/K..." → "6.0.4"
	m := swanVersionRe.FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	return m[1]
}
