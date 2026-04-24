# Xray 用户分级限速实施计划（agent 侧）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为 xray 构建的 node-agent 引入用户分级限速能力：每 tier 独立 inbound 端口 + outbound mark + Linux tc HTB/fq_codel 下行带宽池；后端推一次性迁移指令触发存量节点改造；agent 稳态按 5min 拉取用户 + tier 配置；tc 统计随 Payload 上报。

**Architecture:** 新增独立 `ratelimit/` 包管理 tc 规则（构建器函数纯计算 + 执行 wrapper），`xray/` 包改造 `XrayUserSync` 把 `current` 从 `map[string]struct{}` 升级为 `map[string]string`（uuid→tier），用户 add/remove gRPC 按 tier 选 inbound tag；新增 `xray_migrate_tier` 指令 handler 处理结构性迁移；`collect/` 扩展 tc stats Provider。对老后端、老配置完全向后兼容（tier 为空 → 单 inbound 模式）。

**Tech Stack:** Go 1.x (xray build tag), Linux tc/iproute2, xray gRPC HandlerService, NATS dispatcher，现有测试框架（标准 go test）。

**前置 spec：** [docs/superpowers/specs/2026-04-22-xray-user-ratelimit-design.md](../specs/2026-04-22-xray-user-ratelimit-design.md)

---

## 文件结构规划

**新增文件（均限 `//go:build xray`）：**

| 路径 | 职责 |
|---|---|
| `ratelimit/detect.go` | 网卡名自动探测（`ip route get 1.1.1.1`） |
| `ratelimit/detect_test.go` | detect 单元测试 |
| `ratelimit/commands.go` | 纯函数：生成 tc/iptables 命令 `[]string` 参数 |
| `ratelimit/commands_test.go` | 命令生成器单元测试 |
| `ratelimit/state.go` | 当前已应用 tier 快照（内存缓存，用于 diff 最小化命令） |
| `ratelimit/state_test.go` | state diff 逻辑测试 |
| `ratelimit/manager.go` | TCManager 接口 + 实现：Apply/Disable，依赖注入 exec |
| `ratelimit/manager_test.go` | manager 测试（mock exec） |
| `ratelimit/stats.go` | 解析 `tc -s -j class show` JSON 输出 |
| `ratelimit/stats_test.go` | stats parser 测试 |
| `command/xray_migrate_tier.go` | `xray_migrate_tier` 指令 handler |
| `command/xray_migrate_tier_test.go` | handler 测试 |
| `collect/tc_stats.go` | TcStatsProvider（仅 xray 构建） |
| `collect/tc_stats_test.go` | provider 测试 |

**修改文件：**

| 路径 | 修改点 |
|---|---|
| `collect/collector.go` | `Payload` 新增 `TcStats map[string]TierStats` 字段；新增 `TierStats` 类型 |
| `xray/xray_sync.go` | `XrayUserSync` 字段重构：新增 `tiers map[string]TierConfig`；`current` 改为 `map[string]string`（uuid→tier）；新增 `TierConfig` 类型 |
| `xray/grpc.go` | `AddUser(uuid, tier string)`；`RemoveUser(uuid)` 按 current 查 inbound；`injectUsers` 按 tier 分组注入 |
| `xray/api.go` | 新增 `fetchUsersV2` 走新格式；请求统一加 `X-Agent-Version: 2` header；fallback 到老格式时使用 `defaultTier` |
| `xray/config.go` | `writeConfig` 支持多 inbound：按 tier 分组 clients，每 tier 一个 inbound |
| `xray/schedule.go` | `diffUsers` 改为返回"add/remove/changeTier"三态；调用方顺序：先 remove，再 add |
| `xray.go` | `setupXray` 初始化 TCManager、注册 migrate handler、buildProviders 追加 TcStatsProvider |
| `main.go`（含 `Config`） | 新增 `RATELIMIT_IFACE` / `RATELIMIT_ENABLED` 环境变量；启动时解析 |
| `config.go` | 如果环境变量解析在此文件，加上对应字段 |
| `version-xray.txt` | bump 版本号 |
| `CLAUDE.md` | 更新目录结构和模块职责段落 |

---

## 执行阶段划分

- **Phase 0**：PoC 验证 mark → tc fw 透传（阻塞后续 Phase 1/2，1-2 天）
- **Phase 1**：ratelimit 包（孤立、可单测）
- **Phase 2**：xray 包数据模型 + API 改造（数据模型变化会级联，一起做）
- **Phase 3**：collect 包 tc stats Provider
- **Phase 4**：command 包迁移 handler
- **Phase 5**：main.go / xray.go 接线 + version bump
- **Phase 6**：集成验证

---

## Phase 0：mark 透传 PoC（阻塞性前置）

### Task 0: 验证 xray sockopt.mark 能被 tc fw filter 识别

**目标：** 确认设计假设"xray 写 `sockopt.mark` → Linux 内核 `skb->mark` → tc `fw` filter 分类"在真实容器环境中成立。**如果失败**，整个方案要改用 iptables CONNMARK fallback，此时需回头修订 agent/backend spec。

**环境：** 任意一台有 root 权限的 Linux 测试机，安装 iproute2 + iptables + docker。

**Files:**
- Create: `docs/superpowers/plans/2026-04-22-poc-mark-passthrough.md`（记录 PoC 过程和结论）

- [ ] **Step 1：准备最小 xray 配置（单 inbound + 单 outbound with mark）**

将以下配置保存为测试机的 `/tmp/poc-xray/config.json`：

```jsonc
{
  "log": { "loglevel": "warning" },
  "inbounds": [
    {
      "tag": "proxy",
      "listen": "0.0.0.0",
      "port": 33456,
      "protocol": "vless",
      "settings": {
        "clients": [{"id": "a1b2c3d4-0000-0000-0000-000000000001", "flow": "xtls-rprx-vision"}],
        "decryption": "none"
      },
      "streamSettings": {
        "network": "tcp",
        "security": "reality",
        "realitySettings": {
          "dest": "www.microsoft.com:443",
          "serverNames": ["www.microsoft.com"],
          "privateKey": "REPLACE_WITH_XRAY_X25519_PRIVKEY",
          "shortIds": ["01234567"]
        }
      }
    }
  ],
  "outbounds": [
    {
      "tag": "direct-marked",
      "protocol": "freedom",
      "streamSettings": { "sockopt": { "mark": 1 } }
    }
  ],
  "routing": { "rules": [] }
}
```

运行 `xray x25519` 获取私钥填入。

- [ ] **Step 2：启动 xray 容器**

```bash
docker run -d --name xray-poc --network host \
  -v /tmp/poc-xray:/etc/xray teddysun/xray:latest \
  -config /etc/xray/config.json
```

- [ ] **Step 3：在宿主机配置 tc 观测**

```bash
IFACE=$(ip route get 1.1.1.1 | awk '{for(i=1;i<=NF;i++) if($i=="dev") print $(i+1); exit}')
echo "Using iface=$IFACE"

tc qdisc del dev $IFACE root 2>/dev/null
tc qdisc add dev $IFACE root handle 1: htb default 999
tc class add dev $IFACE parent 1: classid 1:10 htb rate 10mbit ceil 10mbit
tc class add dev $IFACE parent 1: classid 1:999 htb rate 10gbit
tc qdisc add dev $IFACE parent 1:10 handle 10: fq_codel
tc filter add dev $IFACE protocol ip parent 1: prio 1 handle 1 fw flowid 1:10
```

- [ ] **Step 4：用 xray 客户端连测试用户并产生下行流量**

用任意 xray 客户端（v2rayN / xray-core client）配置 vless+reality 连上 `<测试机IP>:33456`，UUID 用 `a1b2c3d4-0000-0000-0000-000000000001`。然后通过该代理下载一个大文件（如 `wget https://speed.cloudflare.com/__down?bytes=100000000`，500MB 级别）。

- [ ] **Step 5：观察 tc class 1:10 的字节数**

在宿主机上**重复执行**下面命令，下载过程中持续观察：

```bash
tc -s class show dev $IFACE classid 1:10
```

**判定条件：**
- `Sent <bytes>` 数值**持续增长** → 透传成功，PoC 通过
- `Sent 0 bytes 0 pkt`（没变化）→ 透传失败，需 fallback

- [ ] **Step 6：如果失败，测试 iptables CONNMARK fallback**

```bash
iptables -t mangle -A OUTPUT -m mark --mark 1 -j CONNMARK --save-mark
iptables -t mangle -A PREROUTING -j CONNMARK --restore-mark
```

重复 Step 4-5 观察。若此时 1:10 有数据 → 记录结论：必须用 CONNMARK fallback。

- [ ] **Step 7：记录结论并提交**

在 `docs/superpowers/plans/2026-04-22-poc-mark-passthrough.md` 写入：

```markdown
# mark 透传 PoC 结论

**日期**：[测试日期]
**测试机**：[内核版本 `uname -r`，iproute2 版本 `tc -V`]
**结论**：[直通 / 需要 CONNMARK fallback]
**原始字节计数证据**：[tc -s class show 的截图或输出粘贴]

## 如果需要 fallback
- agent 的 ratelimit 包额外下 iptables mangle 规则（见 Phase 1 task X）
- 需要更新 spec §11.mark 透传说明章节为"已验证需 fallback"
```

```bash
git add docs/superpowers/plans/2026-04-22-poc-mark-passthrough.md
git commit -m "docs: mark passthrough PoC 结论记录"
```

**如果 PoC 失败**：在进入 Phase 1 前，先更新 `ratelimit/commands.go` 的计划（Task 2）增加 iptables 规则生成逻辑。不必修订整个 plan，只需 Task 2/4 增加对应命令。

---

## Phase 1：ratelimit 包

### Task 1：ratelimit/detect.go 网卡名探测

**Files:**
- Create: `ratelimit/detect.go`
- Create: `ratelimit/detect_test.go`

- [ ] **Step 1：写失败测试**

```go
// ratelimit/detect_test.go
//go:build xray

package ratelimit

import "testing"

func TestParseIfaceFromIPRouteOutput(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "standard output",
			output: "1.1.1.1 via 10.0.0.1 dev eth0 src 10.0.0.123 uid 0",
			want:   "eth0",
		},
		{
			name:   "ens-style iface",
			output: "1.1.1.1 via 192.168.1.1 dev ens3 src 192.168.1.10 uid 1000",
			want:   "ens3",
		},
		{
			name:   "no dev keyword",
			output: "unreachable",
			want:   "",
		},
		{
			name:   "empty",
			output: "",
			want:   "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseIfaceFromIPRouteOutput(tc.output)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2：运行测试验证失败**

```bash
cd /Users/frank/GolandProjects/kd-vps-agent
go test -tags xray ./ratelimit/... -run TestParseIfaceFromIPRouteOutput -v
```

Expected: FAIL（`parseIfaceFromIPRouteOutput` 未定义）

- [ ] **Step 3：写最小实现**

```go
// ratelimit/detect.go
//go:build xray

package ratelimit

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

const fallbackIface = "eth0"

// DetectIface 自动探测默认出口网卡。
// 优先：`ip route get 1.1.1.1` 解析 dev；失败回退 "eth0"。
func DetectIface(ctx context.Context) string {
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c, "ip", "route", "get", "1.1.1.1").Output()
	if err != nil {
		return fallbackIface
	}
	if name := parseIfaceFromIPRouteOutput(string(out)); name != "" {
		return name
	}
	return fallbackIface
}

// parseIfaceFromIPRouteOutput 从 `ip route get` 输出里提取 dev 后的第一个 token。
func parseIfaceFromIPRouteOutput(out string) string {
	fields := strings.Fields(out)
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}
```

- [ ] **Step 4：运行测试验证通过**

```bash
go test -tags xray ./ratelimit/... -run TestParseIfaceFromIPRouteOutput -v
```

Expected: PASS 全部 4 个子测试

- [ ] **Step 5：commit**

```bash
git add ratelimit/detect.go ratelimit/detect_test.go
git commit -m "feat(ratelimit): 网卡名自动探测"
```

---

### Task 2：ratelimit/commands.go 命令生成器（纯函数）

**Files:**
- Create: `ratelimit/commands.go`
- Create: `ratelimit/commands_test.go`

**设计约束**：所有命令生成函数**不执行**命令，只返回 `[]string` argv。执行在 manager.go 里做，便于 mock。

- [ ] **Step 1：写失败测试**

```go
// ratelimit/commands_test.go
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
	got := buildTierFilterArgs("eth0", 1 /*mark*/, 10 /*classid minor*/)
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

// TestClassIDFromMark 验证 mark → classid minor 的映射规则（mark*10）。
func TestClassIDFromMark(t *testing.T) {
	tests := []struct {
		mark int
		want int // classid minor
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
```

- [ ] **Step 2：运行测试验证失败**

```bash
go test -tags xray ./ratelimit/... -run "TestBuild|TestClassID" -v
```

Expected: FAIL

- [ ] **Step 3：写实现**

```go
// ratelimit/commands.go
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
```

- [ ] **Step 4：运行测试验证通过**

```bash
go test -tags xray ./ratelimit/... -run "TestBuild|TestClassID" -v
```

Expected: PASS

- [ ] **Step 5：（可选，仅 PoC 需要 CONNMARK fallback 时）补 iptables 规则生成**

**只在 Phase 0 PoC 结论要求 fallback 时才执行。** 追加：

```go
// ratelimit/commands.go 追加

// buildConnmarkSaveArgs 返回 OUTPUT chain 把 skb->mark 保存到 conntrack mark 的 iptables 命令。
func buildConnmarkSaveArgs() []string {
	return []string{"-t", "mangle", "-A", "OUTPUT", "-j", "CONNMARK", "--save-mark"}
}

// buildConnmarkRestoreArgs 返回 PREROUTING chain 把 conntrack mark 恢复到 skb->mark 的命令。
func buildConnmarkRestoreArgs() []string {
	return []string{"-t", "mangle", "-A", "PREROUTING", "-j", "CONNMARK", "--restore-mark"}
}
```

对应测试：

```go
func TestBuildConnmarkArgs(t *testing.T) {
	save := buildConnmarkSaveArgs()
	wantSave := []string{"-t", "mangle", "-A", "OUTPUT", "-j", "CONNMARK", "--save-mark"}
	if !reflect.DeepEqual(save, wantSave) { t.Errorf("save got %v", save) }

	restore := buildConnmarkRestoreArgs()
	wantRestore := []string{"-t", "mangle", "-A", "PREROUTING", "-j", "CONNMARK", "--restore-mark"}
	if !reflect.DeepEqual(restore, wantRestore) { t.Errorf("restore got %v", restore) }
}
```

- [ ] **Step 6：commit**

```bash
git add ratelimit/commands.go ratelimit/commands_test.go
git commit -m "feat(ratelimit): tc 命令生成器（纯函数）"
```

---

### Task 3：ratelimit/state.go 已应用状态缓存

**Files:**
- Create: `ratelimit/state.go`
- Create: `ratelimit/state_test.go`

**目的**：记录"当前已下发的 tier 配置快照"，Apply 时 diff 新配置，只发变化的命令。

- [ ] **Step 1：写失败测试**

```go
// ratelimit/state_test.go
//go:build xray

package ratelimit

import (
	"reflect"
	"sort"
	"testing"
)

// 测试 DiffState：给定旧状态和新状态，应返回需要 add、change、remove 的 tier 列表。
func TestDiffState(t *testing.T) {
	oldState := map[string]TierState{
		"vip":  {MarkID: 1, PoolMbps: 100},
		"svip": {MarkID: 2, PoolMbps: 500},
	}
	newState := map[string]TierState{
		"vip":  {MarkID: 1, PoolMbps: 200}, // pool 改了
		"svip": {MarkID: 2, PoolMbps: 500}, // 没变
		"trial":{MarkID: 3, PoolMbps: 50},  // 新增
	}
	// 预期：
	// - add:   trial
	// - change: vip
	// - remove: (none)

	add, change, remove := DiffState(oldState, newState)

	sort.Strings(add)
	sort.Strings(change)
	sort.Strings(remove)

	if !reflect.DeepEqual(add, []string{"trial"}) {
		t.Errorf("add got %v, want [trial]", add)
	}
	if !reflect.DeepEqual(change, []string{"vip"}) {
		t.Errorf("change got %v, want [vip]", change)
	}
	if len(remove) != 0 {
		t.Errorf("remove got %v, want empty", remove)
	}
}

func TestDiffState_RemoveOnly(t *testing.T) {
	oldState := map[string]TierState{"vip": {MarkID: 1, PoolMbps: 100}}
	newState := map[string]TierState{}

	add, change, remove := DiffState(oldState, newState)
	if len(add) != 0 || len(change) != 0 {
		t.Errorf("add/change should be empty")
	}
	if !reflect.DeepEqual(remove, []string{"vip"}) {
		t.Errorf("remove got %v, want [vip]", remove)
	}
}

func TestDiffState_MarkIDChangeCountsAsRemoveAdd(t *testing.T) {
	// mark_id 变化：旧 tier 要 remove（旧 classid 不再用），新 tier 要 add（新 classid）
	// 语义上是"换 classid"，用 remove+add 保证干净
	oldState := map[string]TierState{"vip": {MarkID: 1, PoolMbps: 100}}
	newState := map[string]TierState{"vip": {MarkID: 5, PoolMbps: 100}}

	add, change, remove := DiffState(oldState, newState)
	if !reflect.DeepEqual(add, []string{"vip"}) || !reflect.DeepEqual(remove, []string{"vip"}) || len(change) != 0 {
		t.Errorf("mark change: add=%v change=%v remove=%v", add, change, remove)
	}
}
```

- [ ] **Step 2：运行验证失败**

```bash
go test -tags xray ./ratelimit/... -run TestDiffState -v
```

Expected: FAIL

- [ ] **Step 3：写实现**

```go
// ratelimit/state.go
//go:build xray

package ratelimit

// TierState 单个 tier 的已应用状态快照。
type TierState struct {
	MarkID   int
	PoolMbps int
}

// DiffState 返回新旧状态的三态差分：add / change / remove。
// mark_id 变化按 remove+add 处理，避免 classid 漂移。
func DiffState(oldState, newState map[string]TierState) (add, change, remove []string) {
	for name, nt := range newState {
		ot, ok := oldState[name]
		if !ok {
			add = append(add, name)
			continue
		}
		if ot.MarkID != nt.MarkID {
			// mark 变了：先 remove 旧，再 add 新
			remove = append(remove, name)
			add = append(add, name)
			continue
		}
		if ot.PoolMbps != nt.PoolMbps {
			change = append(change, name)
		}
	}
	for name := range oldState {
		if _, ok := newState[name]; !ok {
			remove = append(remove, name)
		}
	}
	return
}
```

- [ ] **Step 4：运行验证通过**

```bash
go test -tags xray ./ratelimit/... -run TestDiffState -v
```

Expected: PASS 全部 3 个测试

- [ ] **Step 5：commit**

```bash
git add ratelimit/state.go ratelimit/state_test.go
git commit -m "feat(ratelimit): 三态 diff 状态追踪"
```

---

### Task 4：ratelimit/manager.go TCManager 接口与实现

**Files:**
- Create: `ratelimit/manager.go`
- Create: `ratelimit/manager_test.go`

**设计要点：** 
- `execFn func(cmd string, args ...string) error` 作为结构体字段注入，便于 mock
- `Apply` 内部先 diff 状态，然后依次执行命令
- `Disable` 清理所有规则（下 `qdisc del ... root`，忽略 "No such file" 错误）

- [ ] **Step 1：写失败测试**

```go
// ratelimit/manager_test.go
//go:build xray

package ratelimit

import (
	"fmt"
	"strings"
	"testing"
)

// recordingExec 记录所有 exec 调用的参数，用于断言。
type recordingExec struct {
	calls [][]string
	err   error // 设置后对所有调用返回错误
}

func (r *recordingExec) run(cmd string, args ...string) error {
	combined := append([]string{cmd}, args...)
	r.calls = append(r.calls, combined)
	return r.err
}

func TestManager_ApplyFirstTime(t *testing.T) {
	rec := &recordingExec{}
	m := NewManager("eth0", rec.run)

	tiers := map[string]TierConfig{
		"vip":  {MarkID: 1, PoolMbps: 100},
		"svip": {MarkID: 2, PoolMbps: 500},
	}
	if err := m.Apply(tiers); err != nil {
		t.Fatalf("Apply err: %v", err)
	}

	// 首次 Apply 应包含 root qdisc + 每 tier 3 条（class / fq_codel / filter）
	// 总 tc 调用 = 1 + 2*3 = 7
	tcCalls := 0
	for _, c := range rec.calls {
		if c[0] == "tc" {
			tcCalls++
		}
	}
	if tcCalls != 7 {
		t.Errorf("expected 7 tc calls, got %d (calls=%v)", tcCalls, rec.calls)
	}
}

func TestManager_ApplyIdempotent(t *testing.T) {
	rec := &recordingExec{}
	m := NewManager("eth0", rec.run)

	tiers := map[string]TierConfig{
		"vip": {MarkID: 1, PoolMbps: 100},
	}
	if err := m.Apply(tiers); err != nil { t.Fatal(err) }
	firstCount := len(rec.calls)

	// 同样配置再 Apply 一次，不应有新命令（状态无 diff）
	if err := m.Apply(tiers); err != nil { t.Fatal(err) }
	if len(rec.calls) != firstCount {
		t.Errorf("second Apply should be no-op; calls grew from %d to %d", firstCount, len(rec.calls))
	}
}

func TestManager_ApplyPoolChange(t *testing.T) {
	rec := &recordingExec{}
	m := NewManager("eth0", rec.run)

	tiers1 := map[string]TierConfig{"vip": {MarkID: 1, PoolMbps: 100}}
	_ = m.Apply(tiers1)

	baseline := len(rec.calls)
	tiers2 := map[string]TierConfig{"vip": {MarkID: 1, PoolMbps: 200}} // pool 改
	if err := m.Apply(tiers2); err != nil { t.Fatal(err) }

	// 改 pool 只应执行 class replace 一条命令
	newCalls := rec.calls[baseline:]
	if len(newCalls) != 1 {
		t.Errorf("pool change should emit 1 cmd, got %d: %v", len(newCalls), newCalls)
	}
	argStr := strings.Join(newCalls[0], " ")
	if !strings.Contains(argStr, "class") || !strings.Contains(argStr, "200mbit") {
		t.Errorf("expected class change to 200mbit, got %q", argStr)
	}
}

func TestManager_ApplyExecError(t *testing.T) {
	rec := &recordingExec{err: fmt.Errorf("mock err")}
	m := NewManager("eth0", rec.run)

	tiers := map[string]TierConfig{"vip": {MarkID: 1, PoolMbps: 100}}
	if err := m.Apply(tiers); err == nil {
		t.Error("expected error, got nil")
	}
}

func TestManager_Disable(t *testing.T) {
	rec := &recordingExec{}
	m := NewManager("eth0", rec.run)

	tiers := map[string]TierConfig{"vip": {MarkID: 1, PoolMbps: 100}}
	_ = m.Apply(tiers)
	baseline := len(rec.calls)

	if err := m.Disable(); err != nil { t.Fatal(err) }
	calls := rec.calls[baseline:]
	if len(calls) != 1 || calls[0][0] != "tc" || !strings.Contains(strings.Join(calls[0], " "), "qdisc del") {
		t.Errorf("expected single 'tc qdisc del' call, got %v", calls)
	}

	// Disable 后再 Apply 应该重新下发所有规则
	if err := m.Apply(tiers); err != nil { t.Fatal(err) }
	afterReApply := len(rec.calls) - baseline - 1
	if afterReApply != 4 { // 1 root qdisc + 3 tier
		t.Errorf("re-apply after Disable should emit 4 cmds, got %d", afterReApply)
	}
}
```

- [ ] **Step 2：运行验证失败**

```bash
go test -tags xray ./ratelimit/... -run TestManager -v
```

Expected: FAIL

- [ ] **Step 3：写实现**

```go
// ratelimit/manager.go
//go:build xray

package ratelimit

import (
	"fmt"
	"strings"
	"sync"
)

// TierConfig 传给 Apply 的输入。
type TierConfig struct {
	MarkID   int
	PoolMbps int
}

// ExecFunc 执行单条命令的函数类型，便于测试时注入 mock。
// cmd 通常是 "tc"，args 是后续参数。
type ExecFunc func(cmd string, args ...string) error

// Manager tc 规则管理器。
type Manager struct {
	iface string
	exec  ExecFunc

	mu           sync.Mutex
	rootInstalled bool
	state        map[string]TierState
}

// NewManager 构造。iface 为目标网卡名，exec 为执行器（生产用 exec.Command 的封装，测试用 mock）。
func NewManager(iface string, exec ExecFunc) *Manager {
	return &Manager{
		iface: iface,
		exec:  exec,
		state: make(map[string]TierState),
	}
}

// Apply 幂等下发 tc 规则。
// 1) 首次调用：先下 root qdisc，再遍历每 tier 下 class/qdisc/filter。
// 2) 后续调用：diff 状态，只下变化的命令。
func (m *Manager) Apply(tiers map[string]TierConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.rootInstalled {
		if err := m.runTC(buildRootQdiscArgs(m.iface)); err != nil {
			return fmt.Errorf("root qdisc: %w", err)
		}
		m.rootInstalled = true
	}

	newState := make(map[string]TierState, len(tiers))
	for name, t := range tiers {
		newState[name] = TierState{MarkID: t.MarkID, PoolMbps: t.PoolMbps}
	}

	add, change, remove := DiffState(m.state, newState)

	for _, name := range remove {
		ot := m.state[name]
		cid := ClassIDMinorFromMark(ot.MarkID)
		// 先删 filter，再删 class（顺序避免依赖错误）
		_ = m.runTC([]string{"filter", "del", "dev", m.iface, "protocol", "ip", "parent", "1:", "prio", "1", "handle", fmt.Sprintf("%d", ot.MarkID), "fw"})
		_ = m.runTC([]string{"class", "del", "dev", m.iface, "classid", fmt.Sprintf("1:%d", cid)})
		delete(m.state, name)
	}

	for _, name := range add {
		nt := newState[name]
		cid := ClassIDMinorFromMark(nt.MarkID)
		if err := m.runTC(buildTierClassArgs(m.iface, cid, nt.PoolMbps)); err != nil {
			return fmt.Errorf("add class %s: %w", name, err)
		}
		if err := m.runTC(buildTierLeafQdiscArgs(m.iface, cid)); err != nil {
			return fmt.Errorf("add leaf qdisc %s: %w", name, err)
		}
		if err := m.runTC(buildTierFilterArgs(m.iface, nt.MarkID, cid)); err != nil {
			return fmt.Errorf("add filter %s: %w", name, err)
		}
		m.state[name] = nt
	}

	for _, name := range change {
		nt := newState[name]
		cid := ClassIDMinorFromMark(nt.MarkID)
		if err := m.runTC(buildTierClassArgs(m.iface, cid, nt.PoolMbps)); err != nil {
			return fmt.Errorf("change class %s: %w", name, err)
		}
		m.state[name] = nt
	}

	return nil
}

// Disable 拆除所有 tc 规则（qdisc del root 会级联清理 class/filter）。
func (m *Manager) Disable() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	err := m.runTC(buildTeardownArgs(m.iface))
	if err != nil && !isNoSuchFileErr(err) {
		return err
	}
	m.rootInstalled = false
	m.state = make(map[string]TierState)
	return nil
}

// runTC 调用注入的 exec，第一个参数固定为 "tc"。
func (m *Manager) runTC(args []string) error {
	return m.exec("tc", args...)
}

func isNoSuchFileErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "No such file")
}
```

- [ ] **Step 4：运行验证通过**

```bash
go test -tags xray ./ratelimit/... -run TestManager -v
```

Expected: PASS 全部 5 个测试

- [ ] **Step 5：commit**

```bash
git add ratelimit/manager.go ratelimit/manager_test.go
git commit -m "feat(ratelimit): TCManager 核心（Apply/Disable 幂等下发）"
```

---

### Task 5：ratelimit/stats.go tc 统计解析

**Files:**
- Create: `ratelimit/stats.go`
- Create: `ratelimit/stats_test.go`

- [ ] **Step 1：写失败测试**

```go
// ratelimit/stats_test.go
//go:build xray

package ratelimit

import "testing"

// 真实 tc -s -j class show 输出（Debian 11 iproute2 5.10 格式）
const tcJSONSample = `[
  {
    "class": "htb",
    "handle": "1:10",
    "parent": "1:",
    "rate": 26214400,
    "ceil": 26214400,
    "stats": {
      "bytes": 123456789,
      "packets": 12345,
      "drops": 67,
      "overlimits": 89,
      "requeues": 0,
      "backlog": 2048,
      "qlen": 3
    }
  },
  {
    "class": "htb",
    "handle": "1:20",
    "parent": "1:",
    "stats": {
      "bytes": 987654321,
      "packets": 98765,
      "drops": 1,
      "overlimits": 2,
      "backlog": 0
    }
  }
]`

func TestParseTcStatsJSON(t *testing.T) {
	stats, err := ParseTcStatsJSON([]byte(tcJSONSample))
	if err != nil {
		t.Fatalf("parse err: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("expected 2 classes, got %d", len(stats))
	}
	vip, ok := stats["1:10"]
	if !ok { t.Fatal("1:10 missing") }
	if vip.SentBytes != 123456789 || vip.Dropped != 67 || vip.Overlimits != 89 || vip.BacklogBytes != 2048 {
		t.Errorf("1:10 mismatch: %+v", vip)
	}
	svip, ok := stats["1:20"]
	if !ok { t.Fatal("1:20 missing") }
	if svip.SentBytes != 987654321 || svip.Dropped != 1 {
		t.Errorf("1:20 mismatch: %+v", svip)
	}
}

func TestParseTcStatsJSON_Empty(t *testing.T) {
	stats, err := ParseTcStatsJSON([]byte("[]"))
	if err != nil { t.Fatal(err) }
	if len(stats) != 0 { t.Errorf("expected empty, got %d", len(stats)) }
}

func TestParseTcStatsJSON_Malformed(t *testing.T) {
	_, err := ParseTcStatsJSON([]byte("not json"))
	if err == nil { t.Error("expected error on malformed input") }
}
```

- [ ] **Step 2：运行验证失败**

```bash
go test -tags xray ./ratelimit/... -run TestParseTcStatsJSON -v
```

Expected: FAIL

- [ ] **Step 3：写实现**

```go
// ratelimit/stats.go
//go:build xray

package ratelimit

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

// TierStats tc class 的统计快照。对应 Payload.TcStats 的 value。
type TierStats struct {
	ClassID      string `json:"classId"`
	SentBytes    uint64 `json:"sent"`
	Dropped      uint64 `json:"dropped"`
	Overlimits   uint64 `json:"overlimits"`
	BacklogBytes uint64 `json:"backlog"`
}

type tcClassEntry struct {
	Handle string `json:"handle"`
	Stats  struct {
		Bytes      uint64 `json:"bytes"`
		Drops      uint64 `json:"drops"`
		Overlimits uint64 `json:"overlimits"`
		Backlog    uint64 `json:"backlog"`
	} `json:"stats"`
}

// ParseTcStatsJSON 解析 `tc -s -j class show dev X` 的 JSON 输出。
// 返回 map[classid]TierStats，其中 classid 形如 "1:10"。
func ParseTcStatsJSON(raw []byte) (map[string]TierStats, error) {
	var entries []tcClassEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("parse tc json: %w", err)
	}
	out := make(map[string]TierStats, len(entries))
	for _, e := range entries {
		if e.Handle == "" {
			continue
		}
		out[e.Handle] = TierStats{
			ClassID:      e.Handle,
			SentBytes:    e.Stats.Bytes,
			Dropped:      e.Stats.Drops,
			Overlimits:   e.Stats.Overlimits,
			BacklogBytes: e.Stats.Backlog,
		}
	}
	return out, nil
}

// CollectTcStats 调用 tc -s -j class show 并解析。
// ctx 控制超时。
func CollectTcStats(ctx context.Context, iface string) (map[string]TierStats, error) {
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c, "tc", "-s", "-j", "class", "show", "dev", iface).Output()
	if err != nil {
		return nil, fmt.Errorf("run tc: %w", err)
	}
	return ParseTcStatsJSON(out)
}
```

- [ ] **Step 4：运行验证通过**

```bash
go test -tags xray ./ratelimit/... -run TestParseTcStatsJSON -v
```

Expected: PASS

- [ ] **Step 5：commit**

```bash
git add ratelimit/stats.go ratelimit/stats_test.go
git commit -m "feat(ratelimit): tc -j class stats 解析器"
```

---

## Phase 2：xray 包数据模型与 API 改造

### Task 6：XrayUserSync 重构（tiers 字段 + current 类型升级）

**Files:**
- Modify: `xray/xray_sync.go`

**影响：** 这是破坏性改动，会让 `xray/grpc.go`、`xray/schedule.go` 编译失败，必须 Task 6-10 一起走才能让包再度 build 过。因此本 Task 的"测试通过"目标暂时放在 Task 10 完成后的集成编译。

**Task 6 自身只改 `xray_sync.go`，暂时让包不能编译；Task 7-10 逐个修复。**

- [ ] **Step 1：读当前 xray_sync.go**

已在 spec 上下文中确认现结构。内容参考 `xray/xray_sync.go:21-48`。

- [ ] **Step 2：改写 XrayUserSync 结构**

```go
// xray/xray_sync.go
//go:build xray

package xray

import (
	"context"
	"sync"
	"time"
)

const (
	defaultUUID = "a1b2c3d4-0000-0000-0000-000000000001"

	deltaSyncInterval   = 5 * time.Minute
	tempSyncInterval    = 5 * time.Minute
	healthCheckInterval = 30 * time.Second
)

// TierConfig 由后端下发的 tier 配置（稳态 API 返回）。
type TierConfig struct {
	MarkID     int
	InboundTag string
	PoolMbps   int
}

// XrayUserSync 管理 xray 用户的全量同步和实时增量操作。
type XrayUserSync struct {
	apiBase    string
	token      string
	apiAddr    string
	inboundTag string // 兼容模式用：tiers 为空时退化为单 inbound
	configPath string

	mu                  sync.Mutex
	tiers               map[string]TierConfig // 从后端拉取，空则兼容模式
	defaultTier         string                // 当用户 tier 缺失时回退目标；兼容模式下不使用
	current             map[string]string     // uuid → tier name；兼容模式下 tier = ""
	xrayAPI             XrayAPI
	tempSync            *TempUserSync
	restartSyncInFlight int32
}

func (s *XrayUserSync) SetTempSync(ts *TempUserSync) {
	s.tempSync = ts
}

func NewXrayUserSync(apiBase, token, apiAddr, inboundTag, configPath string) *XrayUserSync {
	return &XrayUserSync{
		apiBase:    apiBase,
		token:      token,
		apiAddr:    apiAddr,
		inboundTag: inboundTag,
		configPath: configPath,
		current:    make(map[string]string),
		tiers:      make(map[string]TierConfig),
	}
}

// Tiers 返回当前缓存的 tier 字典（供外部 ratelimit manager 调用）。
func (s *XrayUserSync) Tiers() map[string]TierConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]TierConfig, len(s.tiers))
	for k, v := range s.tiers {
		out[k] = v
	}
	return out
}

// inboundTagForTier 根据 tier 名选 inbound tag；空 tier 或兼容模式用 s.inboundTag。
func (s *XrayUserSync) inboundTagForTier(tier string) string {
	if tier == "" {
		return s.inboundTag
	}
	if t, ok := s.tiers[tier]; ok {
		return t.InboundTag
	}
	// 兜底：找不到 tier 用 default
	if t, ok := s.tiers[s.defaultTier]; ok {
		return t.InboundTag
	}
	return s.inboundTag
}

func (s *XrayUserSync) TriggerResync(ctx context.Context) {
	s.syncAfterRestart(ctx)
}

func (s *XrayUserSync) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.xrayAPI != nil {
		err := s.xrayAPI.Close()
		s.xrayAPI = nil
		return err
	}
	return nil
}
```

- [ ] **Step 3：暂不编译**（后续 Task 7-10 会跟上）

Note: 此时 `go build -tags xray ./xray/...` 会失败（grpc.go/schedule.go 使用了旧的 `current map[string]struct{}`）。这是**预期的过渡状态**。

- [ ] **Step 4：commit（允许临时 broken build）**

```bash
git add xray/xray_sync.go
git commit -m "refactor(xray): XrayUserSync 支持 tiers 字段（WIP：含依赖待 Task 7-10 修复）" --no-verify
```

**注意 `--no-verify`**：如果仓库有 pre-commit hook 跑 build，这一步会失败。理想做法是把 Task 6-10 合并成一个大 commit（放弃中间 checkpoint），或暂时在 grpc.go/schedule.go 里留老字段做过渡。以下两种选择：

**选 A（推荐）**：把 Task 6+7+8+9+10 合并成一次大 commit（不 split），每个 step 改一个文件，最后一起测一起 commit。这样避免 broken 中间态。

**选 B**：保留单独 commit 但跳过 pre-commit hook（用 `--no-verify`）。

**判断**：看当前仓库是否有 pre-commit hook。如果无（`ls .git/hooks/pre-commit*`），可单独 commit；有则合并。

**采用的方案：合并 Task 6-10 为一个 commit**，见 Task 10 结尾统一 commit。Task 6-9 不单独 commit。

---

### Task 7：xray/grpc.go AddUser 签名改造

**Files:**
- Modify: `xray/grpc.go`

- [ ] **Step 1：改写 AddUser/RemoveUser/injectUsers**

```go
// xray/grpc.go
//go:build xray

package xray

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func (s *XrayUserSync) getAPI() (XrayAPI, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.xrayAPI != nil {
		return s.xrayAPI, nil
	}
	api, err := NewGRPCXrayAPI(s.apiAddr, s.inboundTag)
	if err != nil {
		return nil, err
	}
	s.xrayAPI = api
	return s.xrayAPI, nil
}

// addOrReplaceToInbound 使用指定 inboundTag 注入用户（不改变 getAPI 默认 tag）。
// 复用同一连接，但需要在每次调用时覆盖 inbound tag —— 实际实现见 xray.go 的 XrayAPI 接口适配。
func (s *XrayUserSync) addOrReplaceToInbound(ctx context.Context, inboundTag, uuid string) error {
	api, err := s.getAPI()
	if err != nil {
		return err
	}
	return api.AddOrReplaceToTag(ctx, inboundTag, &User{ID: uuid, UUID: uuid, Flow: "xtls-rprx-vision"})
}

// removeFromInbound 使用指定 inboundTag 移除用户。
func (s *XrayUserSync) removeFromInbound(ctx context.Context, inboundTag, uuid string) error {
	api, err := s.getAPI()
	if err != nil {
		return err
	}
	return api.RemoveUserFromTag(ctx, inboundTag, uuid)
}

// injectUsers 全量注入：按 tier 分组到对应 inbound。
// 兼容模式（tiers 为空）时全部注入到 s.inboundTag。
func (s *XrayUserSync) injectUsers(users []userDTO) error {
	api, err := s.getAPI()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !api.IsXrayReady(ctx) {
		return fmt.Errorf("xray gRPC not ready")
	}

	for _, u := range users {
		if u.UUID == defaultUUID {
			continue
		}
		inbound := s.inboundTagForTier(u.Tier)
		if err := api.AddOrReplaceToTag(ctx, inbound, &User{ID: u.UUID, UUID: u.UUID, Flow: "xtls-rprx-vision"}); err != nil {
			return fmt.Errorf("inject user %s to %s: %w", u.UUID, inbound, err)
		}
	}

	s.mu.Lock()
	s.current = make(map[string]string, len(users))
	for _, u := range users {
		s.current[u.UUID] = u.Tier
	}
	s.mu.Unlock()
	return nil
}

// AddUser 按 tier 注入用户到对应 inbound。兼容模式下 tier 传 ""。
func (s *XrayUserSync) AddUser(uuid, tier string) error {
	inbound := s.inboundTagForTier(tier)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.addOrReplaceToInbound(ctx, inbound, uuid); err != nil {
		if strings.Contains(err.Error(), "connection") || strings.Contains(err.Error(), "unavailable") {
			s.mu.Lock()
			if s.xrayAPI != nil {
				_ = s.xrayAPI.Close()
				s.xrayAPI = nil
			}
			s.mu.Unlock()
		}
		return fmt.Errorf("AddUser uuid=%s tier=%s: %w", uuid, tier, err)
	}
	s.mu.Lock()
	s.current[uuid] = tier
	s.mu.Unlock()
	return nil
}

// RemoveUser 按 current 记录的 tier 定位 inbound 移除。
func (s *XrayUserSync) RemoveUser(uuid string) error {
	s.mu.Lock()
	tier, ok := s.current[uuid]
	s.mu.Unlock()
	if !ok {
		// 兜底：尝试所有已知 inbound 移除
		tier = ""
	}
	inbound := s.inboundTagForTier(tier)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.removeFromInbound(ctx, inbound, uuid); err != nil {
		if strings.Contains(err.Error(), "connection") || strings.Contains(err.Error(), "unavailable") {
			s.mu.Lock()
			if s.xrayAPI != nil {
				_ = s.xrayAPI.Close()
				s.xrayAPI = nil
			}
			s.mu.Unlock()
		}
		return fmt.Errorf("RemoveUser uuid=%s tier=%s: %w", uuid, tier, err)
	}
	s.mu.Lock()
	delete(s.current, uuid)
	s.mu.Unlock()
	return nil
}
```

**注意**：引用了 `AddOrReplaceToTag` / `RemoveUserFromTag`，需在 `xray/types.go` 的 `XrayAPI` 接口上补这两个方法，实际实现转发到现有 gRPC 调用，但显式接收 inbound tag 参数。

- [ ] **Step 2：扩展 XrayAPI 接口与 gRPC 实现**

查看 `xray/xray.go` 和 `xray/types.go`，在 `XrayAPI` 接口加两个方法并实现（复用现有 `HandlerService.AlterInbound` 调用逻辑，把 tag 作为参数而不是 `s.inboundTag`）。

**具体实现细节留给执行者，但签名必须是：**
```go
type XrayAPI interface {
    // ... 已有方法保留
    AddOrReplaceToTag(ctx context.Context, inboundTag string, u *User) error
    RemoveUserFromTag(ctx context.Context, inboundTag, uuid string) error
}
```

- [ ] **Step 3：暂不编译、不 commit**（继续下一个 Task）

---

### Task 8：xray/api.go 新格式响应解析

**Files:**
- Modify: `xray/api.go`

- [ ] **Step 1：userDTO 加 Tier 字段**

```go
// xray/api.go 修改 userDTO
type userDTO struct {
	UUID string `json:"uuid"`
	Tier string `json:"tier,omitempty"` // 新增，老后端返回没有，JSON 解析时为空字符串
}
```

- [ ] **Step 2：定义新格式响应结构体**

```go
// xray/api.go 追加

type tierDTO struct {
	MarkID     int    `json:"markId"`
	InboundTag string `json:"inboundTag"`
	PoolMbps   int    `json:"poolMbps"`
}

type apiRespV2 struct {
	Code int `json:"code"`
	Data struct {
		Tiers map[string]tierDTO `json:"tiers"`
		Users []userDTO          `json:"users"`
	} `json:"data"`
}

type deltaRespV2 struct {
	Code int `json:"code"`
	Data struct {
		Added   []userDTO `json:"added"`
		Removed []string  `json:"removed"`
	} `json:"data"`
}
```

- [ ] **Step 3：给所有现有 HTTP 请求加 `X-Agent-Version: 2` header**

```go
// xray/api.go 在每个 http.NewRequest 之后统一：
req.Header.Set("X-Agent-Version", "2")
req.Header.Set("Authorization", "Bearer "+s.token)
```

修改 `fetchUsers` 和 `fetchDelta` 的请求构造部分。

- [ ] **Step 4：fetchUsers 改为解析新格式并更新 s.tiers**

```go
// xray/api.go 改 fetchUsers

func (s *XrayUserSync) fetchUsers() ([]userDTO, error) {
	var result []userDTO
	err := doWithRetry(2, apiRetryDelay, func() error {
		req, err := http.NewRequest(http.MethodGet, s.apiBase+"/api/agent/xray/users", nil)
		if err != nil { return err }
		req.Header.Set("X-Agent-Version", "2")
		req.Header.Set("Authorization", "Bearer "+s.token)

		resp, err := httpClient.Do(req)
		if err != nil { return err }
		defer resp.Body.Close()

		if err := checkHTTPStatus(resp); err != nil {
			return fmt.Errorf("fetch users: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil { return err }

		// 尝试新格式 {tiers, users}
		var v2 apiRespV2
		if err := json.Unmarshal(body, &v2); err == nil && (len(v2.Data.Tiers) > 0 || len(v2.Data.Users) > 0) {
			// 确认：只有 Data 是 object 而不是 array 时才认为是新格式
			// 上面的 Unmarshal 若 Data 是数组会失败，所以走到这里就是 object
			if v2.Code == 200 {
				tiers := make(map[string]TierConfig, len(v2.Data.Tiers))
				for name, t := range v2.Data.Tiers {
					tiers[name] = TierConfig{MarkID: t.MarkID, InboundTag: t.InboundTag, PoolMbps: t.PoolMbps}
				}
				s.mu.Lock()
				s.tiers = tiers
				s.mu.Unlock()
				result = v2.Data.Users
				return nil
			}
		}

		// 老格式 {code, data: [{uuid}]} fallback（兼容老后端）
		var v1 apiResp
		if err := json.Unmarshal(body, &v1); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}
		if v1.Code != 200 {
			return fmt.Errorf("api returned code=%d", v1.Code)
		}
		// 老格式下 tier 为空，走兼容模式
		s.mu.Lock()
		s.tiers = map[string]TierConfig{} // 清空，进入兼容模式
		s.mu.Unlock()
		result = v1.Data
		return nil
	})
	return result, err
}
```

- [ ] **Step 4b：fetchDelta 类似处理**

`fetchDelta` 内部同样先尝试 `deltaRespV2`（added 是对象数组），失败 fallback 到老格式（added 是字符串数组），将老格式的 `[]string` 转换为 `[]userDTO{uuid: s, tier: ""}` 再返回统一结构。返回类型从 `*deltaData` 改为 `*deltaDataV2`：

```go
type deltaDataV2 struct {
	Added   []userDTO
	Removed []string
}

func (s *XrayUserSync) fetchDelta(since int64) (*deltaDataV2, error) { ... }
```

所有调用方（`schedule.go`）需要 follow up 改签名（Task 10）。

- [ ] **Step 5：不 commit，继续**

---

### Task 9：xray/config.go 多 inbound 写入

**Files:**
- Modify: `xray/config.go`

- [ ] **Step 1：改写 writeConfig 支持多 inbound**

```go
// xray/config.go
//go:build xray

package xray

import (
	"encoding/json"
	"fmt"
	"os"
)

func emailFromUUID(uuid string) string {
	return fmt.Sprintf("xray@%s", uuid)
}

// writeConfig 读取 configPath，按 tier 分组用户写入对应 inbound 的 clients。
// 兼容模式（s.tiers 为空）下退化为单 inbound 老逻辑。
func (s *XrayUserSync) writeConfig(users []userDTO) error {
	data, err := os.ReadFile(s.configPath)
	if err != nil {
		return fmt.Errorf("read config %s: %w", s.configPath, err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	inboundsRaw, ok := raw["inbounds"]
	if !ok {
		return fmt.Errorf("config has no inbounds")
	}

	var inbounds []map[string]json.RawMessage
	if err := json.Unmarshal(inboundsRaw, &inbounds); err != nil {
		return fmt.Errorf("parse inbounds: %w", err)
	}

	s.mu.Lock()
	tiersCopy := make(map[string]TierConfig, len(s.tiers))
	for k, v := range s.tiers {
		tiersCopy[k] = v
	}
	defaultTier := s.defaultTier
	singleInboundMode := len(tiersCopy) == 0
	fallbackTag := s.inboundTag
	s.mu.Unlock()

	// 按 inbound tag 分组 clients
	byTag := map[string][]map[string]string{}
	// defaultUUID 加到每个 tier inbound（双 inbound 模式）或 fallback inbound（兼容模式）
	if singleInboundMode {
		byTag[fallbackTag] = []map[string]string{
			{"id": defaultUUID, "email": "default@test", "flow": "xtls-rprx-vision"},
		}
	} else {
		for _, t := range tiersCopy {
			byTag[t.InboundTag] = []map[string]string{
				{"id": defaultUUID, "email": "default@test", "flow": "xtls-rprx-vision"},
			}
		}
	}

	for _, u := range users {
		if u.UUID == defaultUUID {
			continue
		}
		var tag string
		if singleInboundMode {
			tag = fallbackTag
		} else {
			tier := u.Tier
			if tier == "" || tiersCopy[tier].InboundTag == "" {
				tier = defaultTier
			}
			t, ok := tiersCopy[tier]
			if !ok {
				continue // 跳过 tier 找不到的用户
			}
			tag = t.InboundTag
		}
		byTag[tag] = append(byTag[tag], map[string]string{
			"id": u.UUID, "email": emailFromUUID(u.UUID), "flow": "xtls-rprx-vision",
		})
	}

	// 写回每个 inbound
	for i, inbound := range inbounds {
		var tag string
		if t, ok := inbound["tag"]; ok {
			if err := json.Unmarshal(t, &tag); err != nil {
				return fmt.Errorf("parse inbound tag: %w", err)
			}
		}
		clients, matched := byTag[tag]
		if !matched {
			continue // 该 inbound 不是 tier inbound（如 api listener），跳过
		}

		var settings map[string]json.RawMessage
		if inbound["settings"] != nil {
			if err := json.Unmarshal(inbound["settings"], &settings); err != nil {
				return fmt.Errorf("parse inbound settings: %w", err)
			}
		} else {
			settings = map[string]json.RawMessage{}
		}
		clientsJSON, err := json.Marshal(clients)
		if err != nil {
			return fmt.Errorf("marshal clients: %w", err)
		}
		settings["clients"] = clientsJSON
		settingsJSON, err := json.Marshal(settings)
		if err != nil {
			return fmt.Errorf("marshal inbound settings: %w", err)
		}
		inbounds[i]["settings"] = settingsJSON
	}

	newInboundsJSON, err := json.Marshal(inbounds)
	if err != nil {
		return fmt.Errorf("marshal inbounds: %w", err)
	}
	raw["inbounds"] = newInboundsJSON

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return os.WriteFile(s.configPath, out, 0644)
}
```

- [ ] **Step 2：继续 Task 10（不单独 commit）**

---

### Task 10：xray/schedule.go 三态 diff + 统一编译 + commit

**Files:**
- Modify: `xray/schedule.go`

- [ ] **Step 1：diffUsers 改为三态返回**

```go
// xray/schedule.go 替换 diffUsers

// userChange 升降级场景：tier 变化。
type userChange struct {
	UUID    string
	FromTier string
	ToTier   string
}

// diffUsers 返回 add / remove / changeTier 三态。
// remote: uuid → tier name。
// exclude: 临时用户 UUID（永不 remove）。
func (s *XrayUserSync) diffUsers(remote map[string]string, exclude map[string]struct{}) (toAdd []userDTO, toRemove []string, toChange []userChange) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for uuid, rtier := range remote {
		ctier, ok := s.current[uuid]
		if !ok {
			toAdd = append(toAdd, userDTO{UUID: uuid, Tier: rtier})
			continue
		}
		if ctier != rtier {
			toChange = append(toChange, userChange{UUID: uuid, FromTier: ctier, ToTier: rtier})
		}
	}
	for uuid := range s.current {
		if uuid == defaultUUID {
			continue
		}
		if _, ok := exclude[uuid]; ok {
			continue
		}
		if _, ok := remote[uuid]; !ok {
			toRemove = append(toRemove, uuid)
		}
	}
	return
}
```

- [ ] **Step 2：HourlySync / FullSync / DeltaSync 更新使用三态**

```go
// xray/schedule.go 修改 FullSync

func (s *XrayUserSync) FullSync() error {
	users, err := s.fetchUsers()
	if err != nil {
		return fmt.Errorf("fetch users: %w", err)
	}
	remote := make(map[string]string, len(users))
	for _, u := range users {
		remote[u.UUID] = u.Tier
	}
	toAdd, toRemove, toChange := s.diffUsers(remote, s.tempUserSet())

	// 顺序：remove → change(先 remove 旧 tier 再 add 新) → add
	for _, uuid := range toRemove {
		if err := s.RemoveUser(uuid); err != nil {
			log.Printf("xray_sync: full sync remove user=%s err=%v", uuid, err)
		}
	}
	for _, c := range toChange {
		if err := s.RemoveUser(c.UUID); err != nil {
			log.Printf("xray_sync: full sync change remove user=%s err=%v", c.UUID, err)
			continue
		}
		if err := s.AddUser(c.UUID, c.ToTier); err != nil {
			log.Printf("xray_sync: full sync change add user=%s tier=%s err=%v", c.UUID, c.ToTier, err)
		}
	}
	for _, u := range toAdd {
		if err := s.AddUser(u.UUID, u.Tier); err != nil {
			log.Printf("xray_sync: full sync add user=%s tier=%s err=%v", u.UUID, u.Tier, err)
		}
	}
	log.Printf("xray_sync: full sync add=%d remove=%d change=%d", len(toAdd), len(toRemove), len(toChange))
	return nil
}
```

对 `HourlySync` 应用同样的改造（若与 FullSync 重复可删除，或共享实现）。

**DeltaSync**：`delta.Added` 现在是 `[]userDTO`，循环里调 `AddUser(u.UUID, u.Tier)`；`delta.Removed` 仍是 `[]string`。顺序：先 Removed 再 Added，保证 tier 变化时干净。

```go
// xray/schedule.go 修改 DeltaSync

for _, uuid := range delta.Removed {
	if err := s.RemoveUser(uuid); err != nil {
		return fmt.Errorf("delta remove user=%s: %w", uuid, err)
	}
}
for _, u := range delta.Added {
	if err := s.AddUser(u.UUID, u.Tier); err != nil {
		return fmt.Errorf("delta add user=%s tier=%s: %w", u.UUID, u.Tier, err)
	}
}
```

**StartupSync**：`injectUsers` 已按 tier 分组内部处理，保持原状即可。

**syncAfterRestart**：同样 `injectUsers` 内部按 tier 处理。

- [ ] **Step 3：完整编译**

```bash
go build -tags xray ./...
```

Expected: 编译通过。如果报错，逐个文件查 missing method（通常是 `XrayAPI` 接口补全 `AddOrReplaceToTag`/`RemoveUserFromTag` 没完成）。

- [ ] **Step 4：跑全部测试**

```bash
go test -tags xray ./...
```

Expected: 全部 PASS（原有测试 + Phase 1 新测试）。**如果 `command/xray_user_test.go` 因为 AddUser 签名变了失败 → Task 7 里没完全覆盖接口**，需要把 test 的 `AddUser(uuid string)` 改为 `AddUser(uuid, tier string)`。

- [ ] **Step 5：统一 commit Task 6-10**

```bash
git add xray/xray_sync.go xray/grpc.go xray/api.go xray/config.go xray/schedule.go xray/types.go xray/xray.go
git commit -m "refactor(xray): 支持 tier 分级（XrayUserSync + API + config + schedule）

- XrayUserSync 新增 tiers/defaultTier 字段，current 改为 uuid→tier 映射
- AddUser/RemoveUser 按 tier 选 inbound tag
- API 请求带 X-Agent-Version:2，解析新旧两种格式
- writeConfig 按 tier 分组 clients 到多 inbound
- schedule 的 diff 返回 add/remove/change 三态，tier 变化走 remove+add"
```

---

## Phase 3：collect 包 tc stats Provider

### Task 11：Payload TcStats 字段 + TcStatsProvider

**Files:**
- Modify: `collect/collector.go`
- Create: `collect/tc_stats.go`
- Create: `collect/tc_stats_test.go`

- [ ] **Step 1：扩展 Payload**

```go
// collect/collector.go 修改 Payload

type Payload struct {
	C      string `json:"c"`
	M      string `json:"m"`
	D      string `json:"d"`
	MR     string `json:"m_r"`
	DR     string `json:"d_r"`
	Conn   string `json:"conn"`
	Mem    string `json:"mem"`
	CPU    string `json:"cpu"`
	Disk   string `json:"disk"`
	SV     string `json:"s_v"`
	AV     string `json:"a_v"`
	NodeID string `json:"node_id"`
	Health string `json:"health,omitempty"`
	TcStats map[string]TierStatsDTO `json:"tc_stats,omitempty"` // 新增
}

// TierStatsDTO tc class 统计，key = classid（如 "1:10"）。
type TierStatsDTO struct {
	ClassID      string `json:"classId"`
	SentBytes    uint64 `json:"sent"`
	Dropped      uint64 `json:"dropped"`
	Overlimits   uint64 `json:"overlimits"`
	BacklogBytes uint64 `json:"backlog"`
}
```

- [ ] **Step 2：写失败测试**

```go
// collect/tc_stats_test.go
//go:build xray

package collect

import "testing"

// fakeStatsFn 注入到 TcStatsProvider 用于测试。
type fakeStatsFn struct {
	stats map[string]TierStatsDTO
	err   error
}

func (f *fakeStatsFn) collect() (map[string]TierStatsDTO, error) {
	return f.stats, f.err
}

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

type fakeErr string
func (e fakeErr) Error() string { return string(e) }
```

- [ ] **Step 3：运行测试验证失败**

```bash
go test -tags xray ./collect/... -run TestTcStatsProvider -v
```

Expected: FAIL

- [ ] **Step 4：写实现**

```go
// collect/tc_stats.go
//go:build xray

package collect

import (
	"context"
	"log"

	"github.com/salt-lake/kd-vps-agent/ratelimit"
)

// TcStatsProvider 采集 tc class 统计写入 Payload.TcStats。
type TcStatsProvider struct {
	fetcher func() (map[string]TierStatsDTO, error)
}

// NewTcStatsProvider 构造。iface 为目标网卡名；enabled=false 时 Collect 是 no-op。
func NewTcStatsProvider(iface string, enabled bool) *TcStatsProvider {
	if !enabled {
		return &TcStatsProvider{fetcher: func() (map[string]TierStatsDTO, error) { return nil, nil }}
	}
	return &TcStatsProvider{fetcher: func() (map[string]TierStatsDTO, error) {
		raw, err := ratelimit.CollectTcStats(context.Background(), iface)
		if err != nil {
			return nil, err
		}
		out := make(map[string]TierStatsDTO, len(raw))
		for k, v := range raw {
			out[k] = TierStatsDTO{
				ClassID:      v.ClassID,
				SentBytes:    v.SentBytes,
				Dropped:      v.Dropped,
				Overlimits:   v.Overlimits,
				BacklogBytes: v.BacklogBytes,
			}
		}
		return out, nil
	}}
}

func (p *TcStatsProvider) Collect(pl *Payload) {
	stats, err := p.fetcher()
	if err != nil {
		log.Printf("tc stats: collect err=%v (skip)", err)
		return
	}
	if len(stats) == 0 {
		return
	}
	pl.TcStats = stats
}
```

- [ ] **Step 5：运行测试验证通过**

```bash
go test -tags xray ./collect/... -run TestTcStatsProvider -v
go test -tags xray ./...
```

Expected: PASS（全部测试）

- [ ] **Step 6：commit**

```bash
git add collect/collector.go collect/tc_stats.go collect/tc_stats_test.go
git commit -m "feat(collect): tc stats Provider（上报到 Payload.TcStats）"
```

---

## Phase 4：command 包迁移 handler

### Task 12：xray_migrate_tier 指令 handler

**Files:**
- Create: `command/xray_migrate_tier.go`
- Create: `command/xray_migrate_tier_test.go`

**职责：**
1. 解析 payload（tiers、defaultTier、migrateExisting）
2. 检测当前 config 是否已双 inbound（检查 inbound tag 集合）
3. 若否：备份 config，拉用户，重写 config，docker restart xray
4. 等 xray 就绪后 gRPC 全量注入（按 tier）
5. 调 ratelimit manager Apply
6. 回报结果

**接口**：定义 `TierMigrator` 接口让 XrayUserSync 实现，便于测试 mock。

- [ ] **Step 1：写失败测试**

```go
// command/xray_migrate_tier_test.go
//go:build xray

package command

import (
	"encoding/json"
	"errors"
	"testing"
)

type mockTierMigrator struct {
	migrateCalled bool
	migrateErr    error
	applyCalled   bool
	appliedTiers  map[string]struct {
		MarkID int
		PortRange string
		PoolMbps int
	}
}

func (m *mockTierMigrator) MigrateToTiers(payload []byte) error {
	m.migrateCalled = true
	return m.migrateErr
}

func TestXrayMigrateTier_Success(t *testing.T) {
	m := &mockTierMigrator{}
	h := NewXrayMigrateTierHandler(m)

	payload, _ := json.Marshal(map[string]any{
		"tiers": map[string]any{
			"vip":  map[string]any{"markId": 1, "inboundTag": "proxy-vip", "portRange": "34521-34524", "poolMbps": 100},
			"svip": map[string]any{"markId": 2, "inboundTag": "proxy-svip", "portRange": "45000-45003", "poolMbps": 500},
		},
		"defaultTier": "vip",
		"migrateExisting": true,
	})
	out, err := h.Handle(payload)
	if err != nil { t.Fatal(err) }
	ok, _ := respBody(t, out)
	if !ok { t.Errorf("expected ok, got err resp: %s", out) }
	if !m.migrateCalled { t.Error("MigrateToTiers not called") }
}

func TestXrayMigrateTier_InvalidPayload(t *testing.T) {
	m := &mockTierMigrator{}
	h := NewXrayMigrateTierHandler(m)
	out, err := h.Handle([]byte("not json"))
	if err != nil { t.Fatal(err) }
	ok, _ := respBody(t, out)
	if ok { t.Error("expected err resp for invalid payload") }
	if m.migrateCalled { t.Error("should not call migrator on bad payload") }
}

func TestXrayMigrateTier_MigratorError(t *testing.T) {
	m := &mockTierMigrator{migrateErr: errors.New("boom")}
	h := NewXrayMigrateTierHandler(m)

	payload, _ := json.Marshal(map[string]any{
		"tiers": map[string]any{"vip": map[string]any{"markId": 1, "inboundTag": "proxy-vip", "portRange": "34521-34524", "poolMbps": 100}},
		"defaultTier": "vip",
	})
	out, _ := h.Handle(payload)
	ok, msg := respBody(t, out)
	if ok || msg != "boom" {
		t.Errorf("expected err resp with 'boom', got ok=%v msg=%s", ok, msg)
	}
}
```

- [ ] **Step 2：运行验证失败**

```bash
go test -tags xray ./command/... -run TestXrayMigrateTier -v
```

Expected: FAIL

- [ ] **Step 3：写 handler**

```go
// command/xray_migrate_tier.go
//go:build xray

package command

import (
	"log"
)

// TierMigrator 定义 command 包对迁移能力的依赖，由 xray 包实现。
type TierMigrator interface {
	// MigrateToTiers 执行一次性迁移：读当前 config、备份、按 payload 的 tier 定义重写、restart xray、重注入用户、应用 tc 规则。
	// payload 是原始 JSON（解析由 xray 包内实现）。
	MigrateToTiers(payload []byte) error
}

type XrayMigrateTierHandler struct {
	migrator TierMigrator
}

func NewXrayMigrateTierHandler(m TierMigrator) XrayMigrateTierHandler {
	return XrayMigrateTierHandler{migrator: m}
}

func (XrayMigrateTierHandler) Name() string { return "xray_migrate_tier" }

func (h XrayMigrateTierHandler) Handle(data []byte) ([]byte, error) {
	if h.migrator == nil {
		return errResp("tier migrator not available"), nil
	}
	// 基本校验 JSON 格式（详细解析在 migrator 里）
	if len(data) == 0 || data[0] != '{' {
		return errResp("invalid payload: not an object"), nil
	}
	if err := h.migrator.MigrateToTiers(data); err != nil {
		log.Printf("xray_migrate_tier: err=%v", err)
		return errResp(err.Error()), nil
	}
	return okResp("ok"), nil
}
```

- [ ] **Step 4：运行验证通过**

```bash
go test -tags xray ./command/... -run TestXrayMigrateTier -v
```

Expected: PASS

- [ ] **Step 5：commit**

```bash
git add command/xray_migrate_tier.go command/xray_migrate_tier_test.go
git commit -m "feat(command): xray_migrate_tier 指令 handler"
```

---

### Task 13：xray 包实现 MigrateToTiers

**Files:**
- Create: `xray/migrate.go`
- Create: `xray/migrate_test.go`
- Modify: `xray/xray_sync.go`（加 ratelimitManager 字段 + SetRatelimit）

- [ ] **Step 1：定义 migrate payload 结构**

```go
// xray/migrate.go
//go:build xray

package xray

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/salt-lake/kd-vps-agent/ratelimit"
)

type migrateTierPayload struct {
	Tiers map[string]struct {
		MarkID     int    `json:"markId"`
		InboundTag string `json:"inboundTag"`
		PortRange  string `json:"portRange"`
		PoolMbps   int    `json:"poolMbps"`
	} `json:"tiers"`
	DefaultTier     string `json:"defaultTier"`
	MigrateExisting bool   `json:"migrateExisting"`
}

// TCApplier 由 ratelimit.Manager 实现。
type TCApplier interface {
	Apply(tiers map[string]ratelimit.TierConfig) error
}

// SetRatelimit 注入 ratelimit manager，供迁移流程和后续稳态应用限速规则。
func (s *XrayUserSync) SetRatelimit(m TCApplier) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ratelimit = m
}

// MigrateToTiers 执行一次性结构性迁移。
func (s *XrayUserSync) MigrateToTiers(raw []byte) error {
	var p migrateTierPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("parse migrate payload: %w", err)
	}
	if len(p.Tiers) == 0 {
		return fmt.Errorf("migrate payload: tiers is empty")
	}
	if _, ok := p.Tiers[p.DefaultTier]; !ok {
		return fmt.Errorf("migrate payload: defaultTier %q not in tiers", p.DefaultTier)
	}

	// 1. 检测是否已迁移
	if s.configAlreadyMultiInbound(p) {
		log.Printf("xray_migrate: config already multi-inbound, only refreshing tiers + tc")
		return s.refreshTiersAndApplyTC(p)
	}

	// 2. 备份 config
	backupPath := fmt.Sprintf("%s.bak.%d", s.configPath, time.Now().Unix())
	orig, err := os.ReadFile(s.configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	if err := os.WriteFile(backupPath, orig, 0644); err != nil {
		return fmt.Errorf("backup config: %w", err)
	}
	log.Printf("xray_migrate: config backup -> %s", backupPath)

	// 3. 拉全量用户
	users, err := s.fetchUsers()
	if err != nil {
		return fmt.Errorf("fetch users: %w", err)
	}

	// 4. 更新 tiers + defaultTier 缓存
	s.mu.Lock()
	s.tiers = make(map[string]TierConfig, len(p.Tiers))
	for name, t := range p.Tiers {
		s.tiers[name] = TierConfig{MarkID: t.MarkID, InboundTag: t.InboundTag, PoolMbps: t.PoolMbps}
	}
	s.defaultTier = p.DefaultTier
	s.mu.Unlock()

	// 5. 生成并写入新 config（含多 inbound 结构）
	if err := s.writeMultiInboundConfig(orig, p, users); err != nil {
		// 失败：尝试恢复备份
		_ = os.WriteFile(s.configPath, orig, 0644)
		return fmt.Errorf("write multi-inbound config: %w", err)
	}

	// 6. 开防火墙（对每个 tier 的 portRange 加 iptables ACCEPT）
	for _, t := range p.Tiers {
		if err := openFirewallPort(t.PortRange); err != nil {
			log.Printf("xray_migrate: open firewall %s err=%v (continuing)", t.PortRange, err)
		}
	}

	// 7. docker restart xray
	if err := restartXrayContainer(); err != nil {
		return fmt.Errorf("restart xray: %w", err)
	}

	// 8. 等 xray 就绪并重注入（复用 syncAfterRestart）
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s.syncAfterRestart(ctx)

	// 9. 应用 tc 规则
	if err := s.applyTC(p); err != nil {
		return fmt.Errorf("apply tc: %w", err)
	}

	log.Printf("xray_migrate: migration done, tiers=%d", len(p.Tiers))
	return nil
}

// configAlreadyMultiInbound 检查 config 是否已经有所有 tier 的 inbound tag。
func (s *XrayUserSync) configAlreadyMultiInbound(p migrateTierPayload) bool {
	data, err := os.ReadFile(s.configPath)
	if err != nil {
		return false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	var inbounds []map[string]json.RawMessage
	if err := json.Unmarshal(raw["inbounds"], &inbounds); err != nil {
		return false
	}
	existingTags := map[string]bool{}
	for _, ib := range inbounds {
		var tag string
		_ = json.Unmarshal(ib["tag"], &tag)
		existingTags[tag] = true
	}
	for _, t := range p.Tiers {
		if !existingTags[t.InboundTag] {
			return false
		}
	}
	return true
}

// refreshTiersAndApplyTC 已双 inbound 状态下：只更新 tiers 缓存并重新 Apply tc。
func (s *XrayUserSync) refreshTiersAndApplyTC(p migrateTierPayload) error {
	s.mu.Lock()
	s.tiers = make(map[string]TierConfig, len(p.Tiers))
	for name, t := range p.Tiers {
		s.tiers[name] = TierConfig{MarkID: t.MarkID, InboundTag: t.InboundTag, PoolMbps: t.PoolMbps}
	}
	s.defaultTier = p.DefaultTier
	s.mu.Unlock()
	return s.applyTC(p)
}

func (s *XrayUserSync) applyTC(p migrateTierPayload) error {
	s.mu.Lock()
	rl := s.ratelimit
	s.mu.Unlock()
	if rl == nil {
		log.Printf("xray_migrate: ratelimit manager not set, skip tc apply")
		return nil
	}
	tiers := make(map[string]ratelimit.TierConfig, len(p.Tiers))
	for name, t := range p.Tiers {
		tiers[name] = ratelimit.TierConfig{MarkID: t.MarkID, PoolMbps: t.PoolMbps}
	}
	return rl.Apply(tiers)
}

// writeMultiInboundConfig 基于 orig 配置，按 payload 里的 tiers 生成多 inbound 版本。
// 核心：保留原 inbound 的 streamSettings/realitySettings，为每个 tier 生成一个 inbound（仅 tag + port + clients 不同）；
// 追加 direct-<tier> outbound（带 sockopt.mark）和 routing 规则。
func (s *XrayUserSync) writeMultiInboundConfig(orig []byte, p migrateTierPayload, users []userDTO) error {
	// 具体实现略（执行时按模板 fill-in）：
	// 1. 解析 orig 找到唯一的原 inbound，提取其 streamSettings
	// 2. 生成新 inbounds 数组：每个 tier 一个 inbound，port 用 p.Tiers[name].PortRange，共享原 streamSettings
	// 3. outbounds 追加或替换：direct-<tier> 带 sockopt.mark；保留现有 direct/blocked 兜底
	// 4. routing.rules 追加：inboundTag:[tag] → outboundTag:direct-<tier>
	// 5. clients 按 u.Tier 分组，defaultUUID 写入所有 tier inbound
	//
	// 执行时参考 xray/config.go:writeConfig 的 JSON 操作风格。
	return fmt.Errorf("TODO: implement writeMultiInboundConfig")
}

// openFirewallPort 对一个 port range（如 "45000-45003"）加 iptables ACCEPT 规则。
func openFirewallPort(portRange string) error {
	pr := portRange
	// iptables 多端口用 `:` 分隔
	for i := range pr {
		if pr[i] == '-' {
			pr = pr[:i] + ":" + pr[i+1:]
			break
		}
	}
	// 幂等：先查后加
	chk := exec.Command("iptables", "-C", "INPUT", "-p", "tcp", "--dport", pr, "-j", "ACCEPT")
	if chk.Run() == nil {
		return nil // 已存在
	}
	return exec.Command("iptables", "-I", "INPUT", "-p", "tcp", "--dport", pr, "-j", "ACCEPT").Run()
}

func restartXrayContainer() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "restart", "xray").CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker restart xray: %w, output: %s", err, out)
	}
	return nil
}

// 占位以压编译通过（filepath 防止未使用报错）
var _ = filepath.Join
```

**⚠️ 关键 TODO**：`writeMultiInboundConfig` 的具体实现比较长（要在 JSON 里操作 inbounds/outbounds/routing），本 plan 在此只给出骨架和注释，**执行时按 spec §3.2 的目标结构照 xray/config.go 的风格一步步填**。

- [ ] **Step 2：写 MigrateToTiers 的最小单元测试**

```go
// xray/migrate_test.go
//go:build xray

package xray

import "testing"

func TestMigrateToTiers_InvalidJSON(t *testing.T) {
	s := &XrayUserSync{}
	err := s.MigrateToTiers([]byte("not json"))
	if err == nil { t.Error("expected error on invalid json") }
}

func TestMigrateToTiers_EmptyTiers(t *testing.T) {
	s := &XrayUserSync{}
	err := s.MigrateToTiers([]byte(`{"defaultTier":"vip","tiers":{}}`))
	if err == nil { t.Error("expected error on empty tiers") }
}

func TestMigrateToTiers_DefaultTierNotInTiers(t *testing.T) {
	s := &XrayUserSync{}
	err := s.MigrateToTiers([]byte(`{"defaultTier":"gold","tiers":{"vip":{"markId":1,"inboundTag":"proxy-vip","portRange":"443","poolMbps":100}}}`))
	if err == nil { t.Error("expected error when defaultTier not in tiers") }
}
```

- [ ] **Step 3：添加 XrayUserSync.ratelimit 字段**

```go
// xray/xray_sync.go 在 struct 末尾加一行
	ratelimit TCApplier // 由 main.go 注入
```

（`TCApplier` 定义在 `xray/migrate.go`，避免循环依赖。）

- [ ] **Step 4：编译 + 跑单测**

```bash
go build -tags xray ./...
go test -tags xray ./xray/... -run TestMigrateToTiers -v
```

Expected: 编译 PASS；3 个测试 PASS。`writeMultiInboundConfig` 的 TODO 使得 happy-path 测试暂时不能写；该函数需要执行阶段独立完成（或作为 Task 13b 单独一个任务）。

- [ ] **Step 5：commit 骨架**

```bash
git add xray/migrate.go xray/migrate_test.go xray/xray_sync.go
git commit -m "feat(xray): MigrateToTiers 骨架（writeMultiInboundConfig 待后续实现）"
```

---

### Task 13b：writeMultiInboundConfig 具体实现

**Files:**
- Modify: `xray/migrate.go`（填充函数体）
- Create: `xray/migrate_config_test.go`

- [ ] **Step 1：先写黄金路径测试**

```go
// xray/migrate_config_test.go
//go:build xray

package xray

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const origSingleInboundConfig = `{
  "log": {"loglevel":"info"},
  "inbounds": [{
    "tag": "proxy",
    "listen": "0.0.0.0",
    "port": "34521-34524",
    "protocol": "vless",
    "settings": {"clients": [{"id":"a1b2c3d4-0000-0000-0000-000000000001","email":"default@test","flow":"xtls-rprx-vision"}], "decryption":"none"},
    "streamSettings": {"network":"tcp","security":"reality","realitySettings":{"dest":"www.microsoft.com:443","serverNames":["www.microsoft.com"],"privateKey":"XXX","shortIds":["01234567"]}}
  }],
  "outbounds": [{"tag":"direct","protocol":"freedom"},{"tag":"blocked","protocol":"blackhole"}],
  "routing": {"rules": []}
}`

func TestWriteMultiInboundConfig(t *testing.T) {
	// 准备临时 config 文件
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := ioutil.WriteFile(configPath, []byte(origSingleInboundConfig), 0644); err != nil {
		t.Fatal(err)
	}

	s := &XrayUserSync{configPath: configPath}
	p := migrateTierPayload{
		Tiers: map[string]struct {
			MarkID int "json:\"markId\""
			InboundTag string "json:\"inboundTag\""
			PortRange string "json:\"portRange\""
			PoolMbps int "json:\"poolMbps\""
		}{
			"vip":  {MarkID: 1, InboundTag: "proxy-vip",  PortRange: "34521-34524", PoolMbps: 100},
			"svip": {MarkID: 2, InboundTag: "proxy-svip", PortRange: "45000-45003", PoolMbps: 500},
		},
		DefaultTier: "vip",
	}
	users := []userDTO{
		{UUID: "user-vip-1",  Tier: "vip"},
		{UUID: "user-svip-1", Tier: "svip"},
	}

	if err := s.writeMultiInboundConfig([]byte(origSingleInboundConfig), p, users); err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(configPath)
	var parsed map[string]interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil { t.Fatal(err) }

	inbounds := parsed["inbounds"].([]interface{})
	if len(inbounds) != 2 { t.Fatalf("expected 2 inbounds, got %d", len(inbounds)) }

	tags := map[string]bool{}
	for _, ib := range inbounds {
		m := ib.(map[string]interface{})
		tag := m["tag"].(string)
		tags[tag] = true
	}
	if !tags["proxy-vip"] || !tags["proxy-svip"] {
		t.Errorf("missing tier inbounds, got %v", tags)
	}

	// outbounds 应含 direct-vip / direct-svip
	outbounds := parsed["outbounds"].([]interface{})
	foundVipOut := false
	foundSvipOut := false
	for _, ob := range outbounds {
		m := ob.(map[string]interface{})
		switch m["tag"].(string) {
		case "direct-vip": foundVipOut = true
		case "direct-svip": foundSvipOut = true
		}
	}
	if !foundVipOut || !foundSvipOut { t.Error("missing direct-<tier> outbounds") }

	// routing.rules 应含 2 条
	routing := parsed["routing"].(map[string]interface{})
	rules := routing["rules"].([]interface{})
	if len(rules) < 2 { t.Errorf("expected >=2 routing rules, got %d", len(rules)) }

	// clients 分布：proxy-vip 应含 user-vip-1 + defaultUUID；proxy-svip 应含 user-svip-1 + defaultUUID
	allContent := string(raw)
	if !strings.Contains(allContent, "user-vip-1") || !strings.Contains(allContent, "user-svip-1") {
		t.Errorf("users not in config")
	}
}
```

- [ ] **Step 2：运行验证失败**

```bash
go test -tags xray ./xray/... -run TestWriteMultiInboundConfig -v
```

Expected: FAIL（函数返回 `TODO` 错误）

- [ ] **Step 3：填充 writeMultiInboundConfig**

把 Task 13 里的骨架替换为完整实现：

```go
func (s *XrayUserSync) writeMultiInboundConfig(orig []byte, p migrateTierPayload, users []userDTO) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(orig, &raw); err != nil {
		return fmt.Errorf("parse orig config: %w", err)
	}

	// 解析原 inbounds，取第一个作为模板
	var origInbounds []map[string]json.RawMessage
	if err := json.Unmarshal(raw["inbounds"], &origInbounds); err != nil {
		return fmt.Errorf("parse inbounds: %w", err)
	}
	if len(origInbounds) == 0 {
		return fmt.Errorf("no inbound in config to use as template")
	}
	template := origInbounds[0] // 假设第一个就是唯一的 proxy inbound

	// 按 tier 分组 clients
	byTag := map[string][]map[string]string{}
	for _, t := range p.Tiers {
		byTag[t.InboundTag] = []map[string]string{
			{"id": defaultUUID, "email": "default@test", "flow": "xtls-rprx-vision"},
		}
	}
	for _, u := range users {
		if u.UUID == defaultUUID { continue }
		tier := u.Tier
		if _, ok := p.Tiers[tier]; !ok {
			tier = p.DefaultTier
		}
		tag := p.Tiers[tier].InboundTag
		byTag[tag] = append(byTag[tag], map[string]string{
			"id": u.UUID, "email": emailFromUUID(u.UUID), "flow": "xtls-rprx-vision",
		})
	}

	// 生成新 inbounds 数组
	newInbounds := []map[string]json.RawMessage{}
	for name, t := range p.Tiers {
		ib := map[string]json.RawMessage{}
		for k, v := range template {
			ib[k] = v
		}
		tagJSON, _ := json.Marshal(t.InboundTag)
		portJSON, _ := json.Marshal(t.PortRange)
		ib["tag"] = tagJSON
		ib["port"] = portJSON

		// 替换 settings.clients
		var settings map[string]json.RawMessage
		if ib["settings"] != nil {
			_ = json.Unmarshal(ib["settings"], &settings)
		} else {
			settings = map[string]json.RawMessage{}
		}
		clientsJSON, _ := json.Marshal(byTag[t.InboundTag])
		settings["clients"] = clientsJSON
		// 补 decryption 字段（vless 要求）
		if _, ok := settings["decryption"]; !ok {
			settings["decryption"] = json.RawMessage(`"none"`)
		}
		settingsJSON, _ := json.Marshal(settings)
		ib["settings"] = settingsJSON

		newInbounds = append(newInbounds, ib)
		_ = name
	}
	newInboundsJSON, err := json.Marshal(newInbounds)
	if err != nil { return fmt.Errorf("marshal new inbounds: %w", err) }
	raw["inbounds"] = newInboundsJSON

	// outbounds：追加 direct-<tier>，保留现有
	var outbounds []map[string]json.RawMessage
	_ = json.Unmarshal(raw["outbounds"], &outbounds)
	for _, t := range p.Tiers {
		tag := "direct-" + findTierName(p.Tiers, t.MarkID)
		// 幂等：已存在同 tag 跳过
		exists := false
		for _, ob := range outbounds {
			var oTag string
			_ = json.Unmarshal(ob["tag"], &oTag)
			if oTag == tag { exists = true; break }
		}
		if exists { continue }
		ob := map[string]json.RawMessage{}
		tagJSON, _ := json.Marshal(tag)
		ob["tag"] = tagJSON
		ob["protocol"] = json.RawMessage(`"freedom"`)
		sockopt, _ := json.Marshal(map[string]any{"sockopt": map[string]any{"mark": t.MarkID}})
		ob["streamSettings"] = sockopt
		outbounds = append(outbounds, ob)
	}
	outboundsJSON, _ := json.Marshal(outbounds)
	raw["outbounds"] = outboundsJSON

	// routing.rules：追加 inboundTag → outboundTag 规则
	var routing map[string]json.RawMessage
	if raw["routing"] != nil {
		_ = json.Unmarshal(raw["routing"], &routing)
	} else {
		routing = map[string]json.RawMessage{}
	}
	var rules []map[string]json.RawMessage
	if routing["rules"] != nil {
		_ = json.Unmarshal(routing["rules"], &rules)
	}
	for tierName, t := range p.Tiers {
		targetOut := "direct-" + tierName
		// 幂等：已存在相同 inboundTag 的规则跳过
		exists := false
		for _, r := range rules {
			var ibTags []string
			_ = json.Unmarshal(r["inboundTag"], &ibTags)
			for _, ibt := range ibTags {
				if ibt == t.InboundTag { exists = true; break }
			}
			if exists { break }
		}
		if exists { continue }
		rule := map[string]json.RawMessage{}
		rule["type"] = json.RawMessage(`"field"`)
		inboundTagJSON, _ := json.Marshal([]string{t.InboundTag})
		outboundTagJSON, _ := json.Marshal(targetOut)
		rule["inboundTag"] = inboundTagJSON
		rule["outboundTag"] = outboundTagJSON
		rules = append(rules, rule)
	}
	rulesJSON, _ := json.Marshal(rules)
	routing["rules"] = rulesJSON
	routingJSON, _ := json.Marshal(routing)
	raw["routing"] = routingJSON

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil { return fmt.Errorf("marshal config: %w", err) }
	return os.WriteFile(s.configPath, out, 0644)
}

// findTierName 反查 markId 对应的 tier 名（p.Tiers 的 key）。
func findTierName(tiers map[string]struct {
	MarkID int "json:\"markId\""
	InboundTag string "json:\"inboundTag\""
	PortRange string "json:\"portRange\""
	PoolMbps int "json:\"poolMbps\""
}, markID int) string {
	for name, t := range tiers {
		if t.MarkID == markID { return name }
	}
	return ""
}
```

- [ ] **Step 4：运行验证通过**

```bash
go test -tags xray ./xray/... -run TestWriteMultiInboundConfig -v
```

Expected: PASS

- [ ] **Step 5：跑全部测试**

```bash
go test -tags xray ./...
```

Expected: 全部 PASS

- [ ] **Step 6：commit**

```bash
git add xray/migrate.go xray/migrate_config_test.go
git commit -m "feat(xray): writeMultiInboundConfig 完整实现 + 测试"
```

---

## Phase 5：main.go / xray.go 接线 + version bump

### Task 14：main.go 环境变量扩展 + xray.go 组装

**Files:**
- Modify: `config.go`（Config 结构体）
- Modify: `xray.go`
- Modify: `main.go`（可能不用改，看 dispatcher 注册位置）

- [ ] **Step 1：给 Config 加字段**

```go
// config.go 在 Config struct 里加

RatelimitIface   string
RatelimitEnabled bool
```

环境变量解析（loadConfig 函数内）：

```go
cfg.RatelimitEnabled = strings.ToLower(os.Getenv("RATELIMIT_ENABLED")) != "false" // 默认 true
cfg.RatelimitIface = os.Getenv("RATELIMIT_IFACE") // 空则自动探测
```

- [ ] **Step 2：xray.go setupXray 里接 ratelimit 和 migrate handler**

```go
// xray.go 改写 setupXray 和 buildProviders

func setupXray(ctx context.Context, cfg Config, d *command.Dispatcher) {
	if cfg.APIBase == "" || cfg.ScriptToken == "" {
		log.Println("xray sync disabled: API_BASE or SCRIPT_TOKEN not set")
		return
	}
	syncer := xray.NewXrayUserSync(
		cfg.APIBase, cfg.ScriptToken,
		cfg.XrayAPIAddr, cfg.XrayInboundTag, cfg.XrayConfigPath,
	)
	tempSync := xray.NewTempUserSync(cfg.APIBase, cfg.ScriptToken, syncer)
	syncer.SetTempSync(tempSync)

	// 初始化 ratelimit manager（若启用）
	if cfg.RatelimitEnabled {
		iface := cfg.RatelimitIface
		if iface == "" {
			iface = ratelimit.DetectIface(ctx)
		}
		rl := ratelimit.NewManager(iface, func(cmd string, args ...string) error {
			out, err := exec.Command(cmd, args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s: %v, output=%s", cmd, err, string(out))
			}
			return nil
		})
		syncer.SetRatelimit(rl)
		log.Printf("ratelimit manager enabled on iface=%s", iface)
	} else {
		log.Println("ratelimit disabled via RATELIMIT_ENABLED=false")
	}

	syncer.Start(ctx)
	tempSync.Start(ctx)
	d.Register(command.NewXrayUserAddHandler(syncer))
	d.Register(command.NewXrayUserRemoveHandler(syncer))
	d.Register(command.NewXrayUpdateHandler(ctx, syncer, cfg.XrayConfigPath))
	d.Register(command.NewXrayFullSyncHandler(syncer))
	d.Register(command.NewXrayMigrateTierHandler(syncer)) // 新增
	go dailyScheduler(ctx, 3, hostJitter(cfg.Host), func() {
		log.Println("daily full sync: start")
		if err := syncer.FullSync(); err != nil {
			log.Printf("xray full sync: %v", err)
		}
	})
}

func buildProviders(cfg Config) []collect.MetricProvider {
	providers := []collect.MetricProvider{
		collect.NewSysProvider(),
		collect.NewTrafficProvider(cfg.Iface),
		collect.NewXrayProvider(cfg.XrayAPIAddr, cfg.XrayConfigPath, cfg.XrayInboundTag),
	}
	if cfg.RatelimitEnabled {
		iface := cfg.RatelimitIface
		if iface == "" {
			iface = ratelimit.DetectIface(context.Background())
		}
		providers = append(providers, collect.NewTcStatsProvider(iface, true))
	}
	return providers
}
```

**注意 import**：加上 `"os/exec"`、`"github.com/salt-lake/kd-vps-agent/ratelimit"`。

- [ ] **Step 3：注意 AddUser 签名变化对 command 包的影响**

command/xray_user.go 里的 `XrayUserManager` 接口和 `XrayUserAddHandler.Handle` 调用了 `syncer.AddUser(uuid)`（单参）。需要同步改为：

```go
// command/xray_user.go 修改

type XrayUserManager interface {
	AddUser(uuid, tier string) error  // 签名变化
	RemoveUser(uuid string) error
}

// 修改 xrayUserPayload 加 tier：
type xrayUserPayload struct {
	UUID  string `json:"uuid"`
	Email string `json:"email"`
	Tier  string `json:"tier"` // 新增，老后端发 "" 不影响
}

// XrayUserAddHandler.Handle 内部调用改为：
if err := h.syncer.AddUser(p.UUID, p.Tier); err != nil {
```

同时 `command/xray_user_test.go` 的 mock 需要更新签名：

```go
func (m *mockSyncer) AddUser(uuid, tier string) error {
	m.addCalled = uuid
	return m.addErr
}
```

- [ ] **Step 4：编译 + 跑全部测试**

```bash
go build -tags xray ./...
go test -tags xray ./...
```

Expected: 全部 PASS。如果编译失败，排查 XrayUserManager 接口实现不完整（syncer 需要满足 AddUser 新签名）。

- [ ] **Step 5：commit**

```bash
git add config.go xray.go command/xray_user.go command/xray_user_test.go
git commit -m "feat: wire ratelimit manager 和 migrate handler 到 xray setup

- Config 新增 RatelimitIface/RatelimitEnabled 环境变量
- setupXray 初始化 ratelimit.Manager 并注入 syncer
- buildProviders 追加 TcStatsProvider
- AddUser 签名扩展为 (uuid, tier)，XrayUserManager 接口同步"
```

---

### Task 15：bump version

**Files:**
- Modify: `version-xray.txt`
- Modify: `CLAUDE.md`

- [ ] **Step 1：查当前版本**

```bash
cat version-xray.txt
```

- [ ] **Step 2：bump 次版本号**

比如当前是 `2.0.29`，bump 到 `2.1.0`（引入新功能，bump minor）：

```bash
echo "2.1.0" > version-xray.txt
```

- [ ] **Step 3：更新 CLAUDE.md**

加上新模块说明：

```markdown
├── ratelimit/        # tier 限速 tc 规则管理（仅 xray 构建）
│   ├── manager.go    # TCManager.Apply/Disable
│   ├── commands.go   # tc 命令生成器
│   ├── state.go      # 已应用状态缓存
│   ├── stats.go      # tc -s -j 解析
│   └── detect.go     # 网卡名探测
```

以及在"核心接口"段落补充 TCManager 接口；环境变量表补充 `RATELIMIT_IFACE` 和 `RATELIMIT_ENABLED`。

- [ ] **Step 4：commit**

```bash
git add version-xray.txt CLAUDE.md
git commit -m "chore: bump xray version to 2.1.0（引入 tier 限速）"
```

**不打 tag**（用户 memory：完成改动后不自动发布，发布前先问用户）。

---

## Phase 6：集成验证

### Task 16：单节点手动验证

**目标**：在一台测试 xray 节点上跑一遍完整流程，确认限速生效。

**Files:** 不改代码，只执行命令 + 观察。

- [ ] **Step 1：构建二进制**

```bash
GOOS=linux GOARCH=amd64 go build -tags xray -o node-agent-xray-test .
```

- [ ] **Step 2：上传到测试节点并替换**

```bash
scp node-agent-xray-test root@<test-node>:/tmp/
ssh root@<test-node> "systemctl stop node-agent && cp /tmp/node-agent-xray-test /usr/local/bin/node-agent && systemctl start node-agent"
```

- [ ] **Step 3：观察启动日志**

```bash
ssh root@<test-node> "journalctl -u node-agent -f --since '1 min ago'"
```

Expected: 看到 "ratelimit manager enabled on iface=xxx"，之后看到周期 `/api/agent/xray/users` 拉取日志。

- [ ] **Step 4：模拟后端下发迁移指令（后端 spec 实现完之前可手动测试）**

用 `nats` CLI 或测试脚本往 `node.cmd.<host-dashified>` 发：

```bash
nats --server="nats://<nats-addr>" --auth="<token>" pub "node.cmd.<host-dashified>" '{
  "cmd": "xray_migrate_tier",
  "data": {
    "tiers": {
      "vip":  {"markId":1,"inboundTag":"proxy-vip","portRange":"<node 原 reality 端口区间>","poolMbps":10},
      "svip": {"markId":2,"inboundTag":"proxy-svip","portRange":"<新随机 4 端口>","poolMbps":50}
    },
    "defaultTier": "vip",
    "migrateExisting": true
  }
}'
```

> **注意**：第一次验证时**故意把 poolMbps 调小（10 / 50）**，便于肉眼看到限速效果。

- [ ] **Step 5：验证 config.json 已改写为双 inbound**

```bash
ssh root@<test-node> "jq '.inbounds | length, [.[] | .tag]' /etc/xray/config.json"
```

Expected: `2`, `["proxy-vip", "proxy-svip"]`

- [ ] **Step 6：验证 tc 规则**

```bash
ssh root@<test-node> "tc qdisc show dev eth0 | head; tc class show dev eth0; tc filter show dev eth0"
```

Expected: 看到 root htb、class 1:10 (10mbit)、class 1:20 (50mbit)、fq_codel leafs、两条 fw filter。

- [ ] **Step 7：用测试用户从客户端实测限速**

- VIP 用户（连原 reality 端口）→ speedtest，应限制在 ~10mbit
- SVIP 用户（连新 SVIP 端口）→ speedtest，应限制在 ~50mbit

如果速度不符合预期，查 tc stats：

```bash
ssh root@<test-node> "tc -s class show dev eth0"
```

观察 `Sent` 字节是否计入对应 class、`dropped` 是否有增长。

- [ ] **Step 8：验证 Payload 上报含 tc_stats**

```bash
ssh root@<test-node> "journalctl -u node-agent --since '5 min ago' | grep -i 'tc_stats\\|Payload'"
```

或者后端那边检查 nats 消息流，应看到 Payload 里有 `tc_stats: {"1:10": {...}, "1:20": {...}}`。

- [ ] **Step 9：验证限速调整**

再发一次迁移指令，把 VIP poolMbps 从 10 改成 100。观察：

```bash
ssh root@<test-node> "tc class show dev eth0 classid 1:10"
```

Expected: rate 变为 100mbit，**现有客户端连接不断**。

- [ ] **Step 10：记录结论**

如果全部通过 → 方案验证完毕，可以灰度到更多节点。
如果有问题 → 排查并定位到对应 Task 回炉。

---

## Self-Review Notes

（本节由 plan 作者 self-review 后填入。执行者忽略。）

### 覆盖检查

| Spec 要求 | 实现 Task |
|---|---|
| §3 双 inbound / tc 架构 | Task 13b writeMultiInboundConfig + Task 4 Manager |
| §4.1 X-Agent-Version header | Task 8 |
| §4.2 delta 新格式 | Task 8 |
| §5 XrayUserSync 改造 | Task 6-10 |
| §5.5 ratelimit 包 | Task 1-5 |
| §5.6 migrate handler | Task 12-13 |
| §7 迁移执行步骤 | Task 13 + 13b |
| §9 环境变量 | Task 14 |
| §12 tc stats 上报 | Task 11 |
| §6.3 限速秒级调整 | Task 4 Manager.Apply 已支持 pool 变化只发一条 class replace |

### 遗漏说明

- **Task 7 的 XrayAPI 接口扩展**：`AddOrReplaceToTag`/`RemoveUserFromTag` 的具体实现没展开（依赖 xray gRPC HandlerService 的现有封装）。执行者需要看 `xray/grpc.go` 里现有 `AddOrReplace` / `RemoveUserById` 如何构造 gRPC 请求，把 `inboundTag` 从字段提到参数即可。影响小。
- **mark 透传 fallback**（CONNMARK）：Phase 0 PoC 失败时才需要，Task 2 Step 5 已预留骨架。

### Type 一致性

- `TierConfig` 在 `ratelimit` 和 `xray` 两个包都有同名 struct，但字段不同（ratelimit 版只有 MarkID+PoolMbps；xray 版有 InboundTag）。执行者注意 import 时不要混淆 —— 这是**有意设计**，避免跨包耦合。

---

## 执行方式选择

**Plan complete and saved to `docs/superpowers/plans/2026-04-22-xray-user-ratelimit.md`. Two execution options:**

**1. Subagent-Driven (recommended)** - I dispatch a fresh subagent per task, review between tasks, fast iteration

**2. Inline Execution** - Execute tasks in this session using executing-plans, batch execution with checkpoints

**Which approach?**
