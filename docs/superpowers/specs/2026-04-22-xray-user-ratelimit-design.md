# Xray 用户分级限速方案设计

**日期**：2026-04-22
**适用范围**：node-agent（xray 构建）+ 后端 API + xray 节点配置
**状态**：设计确认，待实施计划

---

## 1. 目标

为 xray 协议节点引入**用户分级限速**，支持：

1. 多档位（初期 VIP / SVIP，后续可扩展）
2. 每档位独立的下行带宽池（例：VIP 100Mbps，SVIP 500Mbps）
3. 池内用户自然公平分享（无硬性每用户上限）
4. 限速数值可后端动态下发，秒级生效，无需重启 xray

**非目标**：

- 上行限速（用户 → VPS 方向）
- 硬性每用户固定上限（例"无论池多空，单用户也不得超过 50Mbps"）
- 用户流量配额（月度额度）
- v1 内动态增删 tier（只支持调整已有 tier 的限速值）

## 2. 核心决策（brainstorm 已确认）

| 决策项 | 选择 | 理由 |
|--------|------|------|
| 限速粒度 | 每档位共享池 + fq_codel 公平队列 | 用户量不可估计，硬性每用户上限实现复杂且收益低 |
| 限速方向 | 仅下行（VPS → 用户） | 代理场景 90%+ 流量为下行，单向整形简单可靠 |
| 下发模型 | 后端 API 定期拉取（复用现有 5min 节奏） | dispatcher 已有 NATS 推送能力，按需再加，v1 不做 |
| 分级数据模型 | tiers 字典由后端下发，可扩展 | 后续加档位不需改 agent 代码 |
| 流量打标路线 | 双 inbound / 每 tier 独立端口 | 升降级通过 gRPC 切 inbound 即可，无需 xray reload；routing 规则静态 |
| tc 实现 | HTB pool class + fq_codel leaf | 零配置弹性伸缩，单用户独占空闲池符合商业场景 |
| 配置改造所有权 | 一次性迁移指令触发 | 存量节点可灰度，agent 日常启动不碰配置结构 |

## 3. 架构

### 3.1 数据流

```text
后端                              agent (xray build)                      Linux 内核
  │                                     │                                     │
  ├── GET /api/agent/xray/users ────────▶│                                     │
  │   {tiers: {...}, users: [...]}      │                                     │
  │                                     │                                     │
  │                                     ├─ gRPC AddUser/RemoveUser ─▶ xray ──▶│
  │                                     │     (按 tier 选 inboundTag)         │
  │                                     │                                     │
  │                                     ├─ tc qdisc/class/filter ────────────▶│  ◀── iptables mangle
  │                                     ├─ iptables -t mangle MARK ───────────▶│       --sport <tier port>
  │                                     │                                     │       → skb->mark
  │                                     │                                     │       → tc fw filter
  │                                     │                                     │       → HTB class
  │                                     │                                     │       → fq_codel
  │                                     │                                     │       → egress
```

### 3.2 xray 配置结构（迁移后）

```jsonc
{
  "inbounds": [
    // VIP 端口：复用节点已有的随机端口区间（迁移前就是这个）
    { "tag": "proxy-vip",  "port": "${NODE_VIP_PORT_RANGE}",  "settings": {"clients": [...]}, "streamSettings": {...} },
    // SVIP 端口：迁移时为该节点新生成的随机端口区间
    { "tag": "proxy-svip", "port": "${NODE_SVIP_PORT_RANGE}", "settings": {"clients": [...]}, "streamSettings": {...} }
  ],
  // outbounds 不追加 tier 级 direct-<tier>，保留原有 direct/blocked 即可
  "outbounds": [
    { "tag": "direct", "protocol": "freedom" },
    { "tag": "blocked", "protocol": "blackhole" }
  ],
  // routing 也不追加 tier 级 routing 规则
  "routing": { "rules": [] }
}
```

**关键设计决策**：端口**每节点独立**，不是全局统一。

- 现有 xray 节点启动时生成随机端口区间（如 `34521-34524`），按 port hopping 监听
- 迁移时：VIP inbound **复用节点现有端口**（零扰动，VIP 用户分享链接保持不变）；SVIP inbound 由迁移指令 payload 传入一个**节点唯一的新随机端口区间**
- 该节点的两个端口区间由后端维护（`tb_node.xray_tier_ports`），agent 不负责生成，只负责按 payload 写入 config
- **xray 配置里 outbounds/routing 完全不感知 tier**（与本 spec 早期版本不同，见 §3.3 PoC 结论）

### 3.3 限速打标机制（PoC 验证后修正）

**最终方案**：iptables 按**源端口**打 mark，tc 按 mark 分类，HTB/fq_codel 限速。

```bash
IFACE=$(ip route get 1.1.1.1 | awk '{for(i=1;i<=NF;i++) if($i=="dev") print $(i+1); exit}')

# 1. 建 tc 分类树（与之前设计一致）
tc qdisc replace dev $IFACE root handle 1: htb default 999

# VIP tier
tc class  replace dev $IFACE parent 1:   classid 1:10 htb rate ${VIP_POOL}mbit  ceil ${VIP_POOL}mbit
tc qdisc  replace dev $IFACE parent 1:10 handle 10: fq_codel
tc filter replace dev $IFACE protocol ip parent 1: prio 1 handle 1 fw flowid 1:10

# SVIP tier
tc class  replace dev $IFACE parent 1:   classid 1:20 htb rate ${SVIP_POOL}mbit ceil ${SVIP_POOL}mbit
tc qdisc  replace dev $IFACE parent 1:20 handle 20: fq_codel
tc filter replace dev $IFACE protocol ip parent 1: prio 1 handle 2 fw flowid 1:20

# 2. 打 mark：匹配 xray inbound 监听端口的 egress 包（即 VPS → 用户下行）
iptables -t mangle -A OUTPUT -p tcp --sport 34521:34524 -j MARK --set-mark 1  # VIP 端口
iptables -t mangle -A OUTPUT -p tcp --sport 45000:45003 -j MARK --set-mark 2  # SVIP 端口
```

**PoC 验证结论**（2026-04-22 在 Ubuntu 22.04 / kernel 5.15 上）：

1. ✅ `iptables MARK → skb->mark → tc fw filter → HTB class` 链路完全正常
2. ✅ `overlimits` 计数实锤证明限速生效（10Mbit 限速下 11155 个包被整形）
3. ❌ 早期设计用 xray outbound `sockopt.mark` 是**方向错误**的：outbound 的 sockopt 只作用在 xray→target 方向的 socket（上行），而**用户下行流量走的是 xray inbound 接受的 socket**（xray→user），那个 socket 没有 mark
4. 实测：通过 xray 代理下载 10MB，tc 只计到 49KB（全是上行 TCP ACK），payload 完全漏过

**所以最终实现**：完全放弃 xray sockopt.mark，改由 agent 的 ratelimit 包通过 iptables 生成并维护规则。
这样还有两个额外好处：

- xray 配置改造面积减小（outbounds/routing 不动）
- 打标逻辑与 xray 解耦（将来换 v2ray / 其它代理软件也能复用）

## 4. 后端 API 合约

### 4.1 `GET /api/agent/xray/users`（扩展）

**新格式响应**：

```json
{
  "code": 200,
  "data": {
    "tiers": {
      "vip":  { "markId": 1, "inboundTag": "proxy-vip",  "poolMbps": 100 },
      "svip": { "markId": 2, "inboundTag": "proxy-svip", "poolMbps": 500 }
    },
    "users": [
      { "uuid": "a1b2...", "tier": "vip" },
      { "uuid": "c3d4...", "tier": "svip" }
    ]
  }
}
```

**注意**：稳态 API 不含 `port` 字段。端口信息在迁移时烘焙进 xray config.json，之后 agent 只用 `inboundTag` 路由 gRPC add/remove 调用，用 `markId` + `poolMbps` 建 tc 规则，与端口无关。

**向后兼容策略**：

- 老 agent 期望 `data: [...]`（直接是数组），新格式 `data: {tiers, users}` 会导致老 agent 解析失败
- **方案**：新 agent 发请求时统一带 `X-Agent-Version: 2` header
  - 后端识别到 `X-Agent-Version: 2` → 返回 `data: {tiers, users: [{uuid, tier}]}`
  - 未带该 header（老 agent）→ 返回 `data: [{uuid}, ...]`（保持现状）
- 该约定同时适用于 `/api/agent/xray/users` 和 `/api/agent/xray/users/delta`

### 4.2 `GET /api/agent/xray/users/delta?since=...`（扩展）

`added` 数组的元素由字符串变为对象：

```json
{
  "code": 200,
  "data": {
    "added":   [{"uuid": "...", "tier": "vip"}],
    "removed": ["uuid1", "uuid2"]
  }
}
```

同样按 agent 版本分发老/新格式。

**Tier 变更（升降级）的处理**：若一个用户 tier 从 vip 变成 svip，delta 返回：
- `removed: [该 uuid]`（需从 vip inbound 移除）
- `added: [{uuid, tier: svip}]`（需加到 svip inbound）

后端保证同一 delta 内"removed + added 同一 uuid"的顺序或语义清晰。

## 5. agent 代码变更范围

### 5.1 `xray/xray_sync.go` — `XrayUserSync` 结构改造

```go
type TierConfig struct {
    MarkID     int
    InboundTag string
    PoolMbps   int
    // 不持有 Port：端口在迁移时已写入 xray config，稳态运行不需要
}

type XrayUserSync struct {
    apiBase    string
    token      string
    apiAddr    string
    configPath string

    mu       sync.Mutex
    tiers    map[string]TierConfig   // 由后端下发缓存；空则单 inbound 兼容模式
    current  map[string]string       // uuid → tier name（原本是 map[string]struct{}）
    xrayAPI  XrayAPI
    // ... 其余保留
}
```

### 5.2 `xray/grpc.go` — 加用户接口签名变化

```go
func (s *XrayUserSync) AddUser(uuid, tier string) error
func (s *XrayUserSync) RemoveUser(uuid string) error  // 根据 current[uuid] 找 inboundTag
```

### 5.3 `xray/api.go` — fetch 解析改造

- 解析新结构 `data: {tiers, users}`
- 请求带 `X-Agent-Version: 2`
- 增量接口 added 元素改为对象

### 5.4 `xray/config.go` — writeConfig 兼容双 inbound

- 按 tier 分组 clients，分别写入两个 inbound 的 settings.clients
- `defaultUUID`（固定测试用户）写入**所有 tier 的 inbound**，便于各端口独立健康检查
- 单 inbound 模式（tiers 为空）走老逻辑

### 5.5 新增 `ratelimit/` 包

```text
ratelimit/
├── manager.go   # TCManager.Apply(tiers) / Disable()
├── commands.go  # 封装 tc 命令执行、解析错误
├── detect.go    # 网卡名探测（ip route get 1.1.1.1）
└── state.go     # 当前已下发状态缓存，用于 diff 最小化命令
```

核心接口：

```go
type TCManager interface {
    Apply(tiers map[string]TierConfig) error  // 幂等：内部 diff 已下发状态，仅执行变化
    Disable() error                            // tc qdisc del dev $IFACE root
}
```

### 5.6 新增 `command/xray_migrate_tier.go`

一次性迁移指令 handler：

```go
type XrayMigrateTierHandler struct { ... }

func (h *XrayMigrateTierHandler) Name() string { return "xray_migrate_tier" }

func (h *XrayMigrateTierHandler) Handle(data []byte) ([]byte, error) {
    // 1. 解析指令 payload（含 tiers 定义、默认 tier）
    // 2. 备份 /etc/xray/config.json → config.json.bak.<timestamp>
    // 3. 读取当前 clients → 按默认 tier 全部迁入，生成双 inbound config
    // 4. 写入新 config
    // 5. docker restart xray
    // 6. 等待 xray 就绪
    // 7. gRPC 重新注入用户（按 tier 分组到对应 inbound）
    // 8. 调用 ratelimit.Apply(tiers)
    // 9. 返回结果
}
```

### 5.7 `main.go` 注册流程

- `buildProviders`（xray 构建）里初始化 `TCManager`
- 定期拉取用户循环里，拿到 tiers 后调用 `tcManager.Apply(tiers)`
- 注册 `xray_migrate_tier` handler 到 dispatcher

## 6. 下发与调控流程

### 6.1 周期同步（每 5min）

1. `fetchUsers()` 从后端拉 `{tiers, users}`
2. diff `tiers`：
   - 值变化（如 `vip.poolMbps: 100 → 200`） → `tc class change`，秒级生效
   - tiers 结构性变化（tier 增删） → **v1 不支持**，记录日志告警，等运维手动处理
3. diff `users`（对照 agent 本地 `current map[string]string`）：
   - 后端有、本地无 → `AddUser(uuid, tier)`
   - 本地有、后端无 → `RemoveUser(uuid)`
   - 两边都有但 tier 不同（升降级） → `RemoveUser(uuid)` 从旧 inbound，再 `AddUser(uuid, newTier)` 到新 inbound
4. tcManager.Apply(tiers) 幂等下发

### 6.2 增量同步（每 5min，与全量错开）

走 `/delta` 接口，只处理 added/removed。tier 值变更不走 delta，走全量。

### 6.3 限速调整生效路径

后端改 tier `poolMbps` → agent 5min 内拉到 → `tc class change` → 秒级生效，**现有连接不断**。

## 7. 一次性迁移指令详解

**payload 格式**（端口由后端按节点定制填入）：

```json
{
  "tiers": {
    "vip":  {"markId": 1, "inboundTag": "proxy-vip",  "portRange": "34521-34524", "poolMbps": 100},
    "svip": {"markId": 2, "inboundTag": "proxy-svip", "portRange": "45012-45015", "poolMbps": 500}
  },
  "defaultTier": "vip",
  "migrateExisting": true
}
```

**端口分配规则**（后端负责）：

- VIP 的 `portRange` = 该节点现有 `tb_node.RealityConfig.PortRange`（复用，零扰动）
- SVIP 的 `portRange` = 后端在迁移前生成的一个新随机 4 端口区间，记录到 `tb_node.xray_tier_ports`
- 指令里的 `portRange` 格式与现有 RealityConfig 一致（`start-end` 或单端口字符串）

**defaultTier 语义**：迁移时从后端拉到的 users 里若有**未带 tier 字段的条目**（老数据），统一归入 `defaultTier`。若后端已为所有用户打好 tier，此字段仅作保险。

**执行步骤**（幂等，重复执行无副作用）：

1. 检测当前 config 是否已是双 inbound 结构：
   - 是 → 只更新 tiers 缓存并 `ratelimit.Apply()`，不改 config
   - 否 → 进入迁移
2. 备份当前 config 到 `config.json.bak.<timestamp>`
3. 从后端拉当前 users（全量，新格式带 tier）
4. 生成新 config：
   - inbounds：按 `tiers[*].inboundTag` + `tiers[*].portRange` 生成；**VIP inbound 复用原 inbound 的完整 streamSettings**（reality 密钥/shortIds/dest 都原样照抄）；SVIP inbound 共用同一套 reality 配置（仅端口不同）
   - outbounds 和 routing **保持原样不动**（打标由 iptables 完成，见 §3.3）
   - clients：按 `user.tier`（缺失则 `defaultTier`）分组写入对应 inbound
5. 写入 `/etc/xray/config.json`
6. `systemctl restart xray`
7. 等待 xray gRPC 就绪（复用现有 `IsXrayReady` 轮询）
8. gRPC 重新注入所有用户（按 tier 分组 `AddUser(uuid, tier)`）
9. `tcManager.Apply(tiers)` —— 同时下 tc 规则和 iptables mangle 规则（含 SVIP 端口的 INPUT ACCEPT 开放由 tcManager 负责，或由单独步骤处理）
10. 上报执行结果（含 SVIP 实际监听端口，便于后端核对）

## 8. 兜底与容错矩阵

| 场景 | 行为 |
|------|------|
| 后端返回空 tiers 或老格式 | agent 进入"兼容模式"：单 inbound 逻辑跑，不动 tc |
| 后端下发了 tiers，但 xray config 还是单 inbound | agent 不启用限速，日志告警，等迁移指令；用户同步仍按单 inbound 走 |
| tc 命令失败 | 不影响 xray 流量（tc 独立于 xray）；上报错误，下个周期重试 |
| xray reload/restart 失败 | 迁移指令返回错误，运维介入；老 config 有备份可回滚 |
| agent 崩溃重启 | tc 规则在内核，不丢失；启动时重新 `Apply()` 保证一致 |
| 紧急关停限速 | 后端下发空 tiers，或节点侧设 `RATELIMIT_ENABLED=false` 重启 agent |
| 网卡名探测失败 | fallback 到 `eth0`；仍失败则日志报错、不 Apply tc，但不影响用户同步 |

## 9. 环境变量新增

| 变量 | 默认 | 说明 |
|------|------|------|
| `RATELIMIT_IFACE` | 自动探测 | tc 工作的网卡；探测用 `ip route get 1.1.1.1`，失败 fallback 到 `eth0` |
| `RATELIMIT_ENABLED` | `true` | 节点级紧急总开关；`false` 时 agent 完全不碰 tc，但用户同步正常 |

## 10. 兼容性承诺

| 版本组合 | 行为 |
|----------|------|
| 新 agent + 老后端（无 tiers 字段） | agent 进入兼容模式，单 inbound，不限速 |
| 老 agent + 新后端 | 后端按 `X-Agent-Version` 缺失返回老格式（`data: [...]`），老 agent 正常工作 |
| 新 agent + 双 inbound config | 正常工作 |
| 新 agent + 单 inbound config | 兼容模式，等迁移指令 |

## 11. 运营建议（池大小与用户数配置）

限速效果是 **"池大小 ÷ 争抢时并发重度用户数"** 两个维度相乘的结果，**档位差异需要两边一起控制**，不仅仅调 `pool_mbps`。

**基本公式**：

```text
单用户争抢体感 ≈ pool_mbps ÷ 并发重度用户数
```

其中"并发重度用户数"经验值约为 tier 在线人数的 5%-10%（做视频/浏览的占多数，下载/高清串流的占少数）。

**初始配置建议**（基于现网单节点 40 人规模，SVIP 占比 15%-20%）：

| tier | 建议 pool_mbps | 建议单节点用户数上限 | 争抢时重度用户人均 |
|------|----------------|----------------------|--------------------|
| VIP  | 200            | 32（80%）            | ~80 Mbps（1-2 人抢） |
| SVIP | 500            | 8（20%）             | ~400 Mbps（1 人独占）|

**分布依据**：SVIP 当前占总用户约 15%，预期天花板 20%。按 20% 作为容量规划上限。

效果对比（同池 200 vs 不同池 200/500）：

- 同池：SVIP 因为人数少，体感已经好 **~4 倍**
- 不同池 + 不同人数：SVIP 体感好 **~10 倍**，档位感清晰

**VPS 网卡是真正的天花板**：

- 所有 tier 的 `pool_mbps` 之和建议不超过节点出口带宽的 80%
- 大多数低端 VPS 出口 1 Gbps → 总 `pool_mbps` ≤ 800
- 高配 VPS（10 Gbps / 40 Gbps）可放宽
- 若 VPS 网卡本身就是瓶颈，tc 限速只是做**区分度**而非硬限

**上线后的调优方式**：

```bash
tc -s class show dev $IFACE  # 看每个 class 的 drop / backlog / bytes
```

- `dropped` 高 → 说明池子紧张，可考虑扩大
- `backlog` 持续堆积 → 用户体验会卡顿，优先处理
- `bytes` 增长斜率 → 实时流量，对照 `pool_mbps` 评估利用率

**v2 可以考虑加入的辅助工具**：

- 后端管理页面展示每节点带宽余量（`节点出口 - 已分配 pool_mbps 之和`）
- 调 `pool_mbps` 时对照节点网卡上限做前置校验
- 可视化显示每节点各 tier 的实时利用率

## 12. tc 统计上报（v1 范围内）

为了让"调整池子大小"有依据，agent 侧采集 tc stats 并随 Payload 上报，后端做时序存储供管理端查询。

### 12.1 采集实现（collect 包新增 Provider）

新增 `collect/tc_stats.go`（仅 xray 构建注册），实现 `MetricProvider` 接口：

```go
//go:build xray

package collect

import (
    "os/exec"
    "strings"
)

type TcStatsProvider struct {
    iface string // 从 ratelimit 包复用网卡探测结果
}

func NewTcStatsProvider(iface string) *TcStatsProvider {
    return &TcStatsProvider{iface: iface}
}

// TierStats 每个 tier 的 tc class 统计，对应 Payload.TcStats
type TierStats struct {
    ClassID  string `json:"classId"`  // "1:10" / "1:20"
    SentBytes uint64 `json:"sent"`
    Dropped  uint64 `json:"dropped"`
    Overlimits uint64 `json:"overlimits"`
    BacklogBytes uint64 `json:"backlog"`
}

func (t *TcStatsProvider) Collect(p *Payload) {
    out, err := exec.Command("tc", "-s", "-j", "class", "show", "dev", t.iface).Output()
    if err != nil {
        return // 静默失败，不影响其他采集
    }
    p.TcStats = parseTcJSON(out) // 解析 JSON 格式输出为 map[classId]TierStats
}
```

**注意**：`tc -j` 需要 iproute2 足够新（Debian 11+、Ubuntu 20.04+ 都有），现网节点应满足；若失败 fallback 解析文本格式。

### 12.2 Payload 扩展

```go
type Payload struct {
    // ... 现有字段
    TcStats map[string]TierStats `json:"tc_stats,omitempty"` // key = classId
}
```

- 老字段不动，新字段 omitempty，对后端/前端完全透明
- 采集失败或 ratelimit 未启用 → 字段为空，不上报

### 12.3 后端接收与存储

后端 report 消费者扩展：收到 `tc_stats` 非空时，写入时序表 `xray_tc_stats_history`：

```sql
CREATE TABLE IF NOT EXISTS xray_tc_stats_history (
    id           BIGSERIAL   PRIMARY KEY,
    node_id      VARCHAR(64) NOT NULL,
    class_id     VARCHAR(16) NOT NULL,  -- "1:10" 对应 VIP，"1:20" 对应 SVIP
    sent_bytes   BIGINT      NOT NULL,
    dropped      BIGINT      NOT NULL,
    overlimits   BIGINT      NOT NULL,
    backlog      BIGINT      NOT NULL,
    collected_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_tc_stats_node_time ON xray_tc_stats_history (node_id, collected_at DESC);
CREATE INDEX IF NOT EXISTS idx_tc_stats_class_time ON xray_tc_stats_history (class_id, collected_at DESC);
```

数据保留策略：建议 **7 天**（跟 user_sync_event 对齐）；更长期存储留 v2 做聚合。

### 12.4 管理端查询 API（v1.5 后补）

```text
GET /api/admin/xray/nodes/:id/tc-stats?tier=vip&from=<unix>&to=<unix>
  → [{collectedAt, sent, dropped, backlog, ...}]
```

前端可在节点详情页画曲线。

### 12.5 指标与阈值（运营使用）

agent 采样间隔 = Payload 上报间隔（`REPORT_INTERVAL`，默认 2min），每次上报一条。

**报警规则建议**（告警规则可放后端 cron）：

| 规则 | 阈值 | 动作 |
|---|---|---|
| `dropped / sent > 1%` 持续 5 分钟 | 严重拥塞 | 告警运营扩池 |
| `backlog > 50KB` 持续 3 分钟 | bufferbloat | 告警 |
| `sent_Mbps / pool_Mbps < 20%` 持续 24 小时 | 池子利用率过低 | 告警"可考虑降池" |

---

## 13. 风险与待验证项

1. ~~**xray `sockopt.mark` 与 tc `fw` filter 的透传**~~ → **已验证并废弃**：PoC（§3.3）确认 outbound sockopt.mark 方向不对，改用 iptables `-t mangle --sport` 打 mark，`iptables MARK → skb->mark → tc fw filter` 链路工作正常
2. **双 inbound 端口分配**：VPS 上两个端口需都不冲突；现网各节点端口情况需运维确认，迁移指令 payload 里的 port 由后端按节点下发
3. **xray reload 的停机时间**：迁移期间 xray 会 restart，用户连接会断一次；迁移需挑低峰执行
4. **tier 字段的后端存储改造**：用户表需加 tier 字段，初始化数据迁移策略由后端方案处理，不在本 spec 范围
5. **`tc -j` JSON 输出格式版本**：老系统（Debian 10-）可能不支持，需 fallback 解析文本格式

## 14. v2 可能的扩展（明确排除）

- NATS 推送限速变更（秒级响应，当前定期拉取 5min 足够）
- 硬性每用户上限（切换到 per-IP 哈希分桶）
- 上行限速（加 ifb + ingress policing）
- 动态增删 tier（需要 xray 配置热更机制支持）
- 用户流量配额统计（xray stats API 已有，需后端存储方案）
- tc stats 长期聚合（日/周/月下采样）与智能告警
- 前端可视化面板（带宽利用率曲线、告警看板）
