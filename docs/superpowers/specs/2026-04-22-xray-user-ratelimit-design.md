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
  │                                     ├─ tc qdisc/class/filter ────────────▶│  ◀── xray outbound
  │                                     │                                     │       sockopt.mark
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
    { "tag": "proxy-vip",  "port": 443,  "settings": {"clients": [...]}, "streamSettings": {...} },
    { "tag": "proxy-svip", "port": 8443, "settings": {"clients": [...]}, "streamSettings": {...} }
  ],
  "outbounds": [
    { "tag": "direct-vip",  "protocol": "freedom",
      "streamSettings": {"sockopt": {"mark": 1}} },
    { "tag": "direct-svip", "protocol": "freedom",
      "streamSettings": {"sockopt": {"mark": 2}} },
    { "tag": "direct", "protocol": "freedom" }
  ],
  "routing": {
    "rules": [
      { "type": "field", "inboundTag": ["proxy-vip"],  "outboundTag": "direct-vip" },
      { "type": "field", "inboundTag": ["proxy-svip"], "outboundTag": "direct-svip" }
    ]
  }
}
```

### 3.3 tc 规则模板

```bash
IFACE=$(ip route get 1.1.1.1 | awk '{for(i=1;i<=NF;i++) if($i=="dev") print $(i+1); exit}')

tc qdisc replace dev $IFACE root handle 1: htb default 999

# VIP tier
tc class  replace dev $IFACE parent 1:   classid 1:10 htb rate ${VIP_POOL}mbit  ceil ${VIP_POOL}mbit
tc qdisc  replace dev $IFACE parent 1:10 handle 10: fq_codel
tc filter replace dev $IFACE protocol ip parent 1: prio 1 handle 1 fw flowid 1:10

# SVIP tier
tc class  replace dev $IFACE parent 1:   classid 1:20 htb rate ${SVIP_POOL}mbit ceil ${SVIP_POOL}mbit
tc qdisc  replace dev $IFACE parent 1:20 handle 20: fq_codel
tc filter replace dev $IFACE protocol ip parent 1: prio 1 handle 2 fw flowid 1:20
```

**mark 透传说明**：Linux 内核中 `SO_MARK`（xray outbound `sockopt.mark` 设置）直接写入 `skb->mark`，`tc filter ... fw` 读取的即该字段，预期无需 iptables 辅助。实施阶段需实测验证；若失败，fallback 方案是在 `mangle` 表的 `OUTPUT` chain 添加 `CONNMARK --save-mark` / `--restore-mark` 规则保证 mark 在 conntrack 往返中保留。

## 4. 后端 API 合约

### 4.1 `GET /api/agent/xray/users`（扩展）

**新格式响应**：

```json
{
  "code": 200,
  "data": {
    "tiers": {
      "vip":  { "markId": 1, "inboundTag": "proxy-vip",  "port": 443,  "poolMbps": 100 },
      "svip": { "markId": 2, "inboundTag": "proxy-svip", "port": 8443, "poolMbps": 500 }
    },
    "users": [
      { "uuid": "a1b2...", "tier": "vip" },
      { "uuid": "c3d4...", "tier": "svip" }
    ]
  }
}
```

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
    Port       int
    PoolMbps   int
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

**payload 格式**：

```json
{
  "tiers": {
    "vip":  {"markId": 1, "inboundTag": "proxy-vip",  "port": 443,  "poolMbps": 100},
    "svip": {"markId": 2, "inboundTag": "proxy-svip", "port": 8443, "poolMbps": 500}
  },
  "defaultTier": "vip",
  "migrateExisting": true
}
```

**defaultTier 语义**：迁移时从后端拉到的 users 里若有**未带 tier 字段的条目**（老数据），统一归入 `defaultTier`。若后端已为所有用户打好 tier，此字段仅作保险。

**执行步骤**（幂等，重复执行无副作用）：

1. 检测当前 config 是否已是双 inbound 结构：
   - 是 → 只更新 tiers 缓存并 `ratelimit.Apply()`，不改 config
   - 否 → 进入迁移
2. 备份当前 config 到 `config.json.bak.<timestamp>`
3. 从后端拉当前 users（全量，新格式带 tier）
4. 生成新 config：
   - inbounds：按 tiers 定义生成（复用原 inbound 的 streamSettings/流控细节，换端口换 tag）
   - outbounds：追加 `direct-<tier>`，保留原有 `direct` 兜底
   - routing：为每个 tier 追加一条 inboundTag → outboundTag 规则
   - clients：按 `user.tier`（缺失则 `defaultTier`）分组写入对应 inbound
5. 写入 `/etc/xray/config.json`
6. `docker restart xray`
7. 等待 xray gRPC 就绪（复用现有 `IsXrayReady` 轮询）
8. gRPC 重新注入所有用户（按 tier 分组 `AddUser(uuid, tier)`）
9. `tcManager.Apply(tiers)`
10. 上报执行结果

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

## 11. 风险与待验证项

1. **xray `sockopt.mark` 与 tc `fw` filter 的透传**：设计假设 `skb->mark` 可直接被 tc 识别，需在实施阶段最小化 PoC 验证；失败时启用 `CONNMARK` fallback
2. **双 inbound 端口分配**：VPS 上两个端口需都不冲突；现网各节点端口情况需运维确认，迁移指令 payload 里的 port 由后端按节点下发
3. **xray reload 的停机时间**：迁移期间 xray 会 restart，用户连接会断一次；迁移需挑低峰执行
4. **tier 字段的后端存储改造**：用户表需加 tier 字段，初始化数据迁移策略由后端方案处理，不在本 spec 范围

## 12. v2 可能的扩展（明确排除）

- NATS 推送限速变更（秒级响应，当前定期拉取 5min 足够）
- 硬性每用户上限（切换到 per-IP 哈希分桶）
- 上行限速（加 ifb + ingress policing）
- 动态增删 tier（需要 xray 配置热更机制支持）
- 用户流量配额统计（xray stats API 已有，需后端存储方案）
