# node-agent CLAUDE.md

## 模块职责

node-agent 是部署在 VPS 节点上的独立 Go 二进制，负责：

1. **指标采集上报**：定期采集系统指标（CPU/内存/磁盘/流量/连接数），通过 NATS 上报给后端
2. **指令订阅执行**：监听 NATS 下发的运维指令（如 docker restart）
3. **自更新**：每天北京时间 02:00（含随机 jitter）检查 GitHub Releases 最新版本，若不同则下载替换二进制并 `systemctl restart node-agent`
4. **每日任务**：ikev2 节点北京时间 04:00 清空 charon.log；xray 节点 03:00 全量同步用户

---

## 目录结构

```
kd-vps-agent/
├── main.go           # 入口：NATS 连接、主循环、dailyScheduler 工具函数
├── ikev2.go          # !xray 构建：setupXray stub、buildProviders、startDailyJobs
├── xray.go           # xray 构建：setupXray 实现、buildProviders、startDailyJobs
├── version-ikev2.txt # ikev2 构建版本号（go:embed 嵌入二进制），格式：1.0.3（无 v 前缀）
├── version-xray.txt  # xray 构建版本号（go:embed 嵌入二进制），格式：1.0.3（无 v 前缀）
├── collect/
│   ├── collector.go  # Payload 结构（含 TcStats 字段）、MetricProvider 接口、Collector 组合
│   ├── sys.go        # CPU(/proc/stat)、内存(/proc/meminfo)、磁盘(syscall.Statfs)
│   ├── traffic.go    # TX 流量累积（/proc/net/dev），持久化到 /var/lib/node-agent/traffic.json
│   ├── swan.go       # IKEv2/StrongSwan：连接数 + 版本（docker exec ipsec）
│   ├── xray.go       # Xray：连接数（stats API）+ 版本（docker exec xray version）
│   └── tc_stats.go   # tc class 统计采集（仅 xray 构建，限速启用时）
├── command/
│   ├── dispatcher.go        # NATS 消息路由到 Handler
│   ├── swan_update.go       # swan_update 指令
│   ├── bootstrap.go         # bootstrap 指令
│   ├── self_update.go       # agent:self_update 指令
│   ├── xray_update.go       # xray_update 指令（仅 xray 构建）
│   ├── xray_user.go         # xray 用户增删指令（含 tier 字段，仅 xray 构建）
│   └── xray_migrate_tier.go # xray_migrate_tier 指令（一次性结构性迁移，仅 xray 构建）
├── update/
│   └── updater.go    # 检查 GitHub Releases → 下载新二进制 → 替换 → systemctl restart
├── ratelimit/        # tier 限速的 tc 规则管理（仅 xray 构建）
│   ├── detect.go     # 网卡名自动探测（ip route get 1.1.1.1）
│   ├── commands.go   # tc 命令生成器（纯函数）
│   ├── state.go      # 已应用 tier 状态三态 diff
│   ├── manager.go    # TCManager：Apply/Disable 幂等下发 + mock 注入
│   └── stats.go      # tc -s -j class show JSON 解析
└── xray/             # xray 专属逻辑（仅 xray 构建）
    ├── xray.go       # GRPCXrayAPI：gRPC 连接封装，支持 per-tag AlterInbound
    ├── types.go      # XrayAPI 接口（AddOrReplaceToTag / RemoveUserFromTag）
    ├── xray_sync.go  # XrayUserSync 结构（含 tiers 字典 + current uuid→tier 映射）
    ├── grpc.go       # 按 tier 选 inbound 的用户增删
    ├── api.go        # 后端 HTTP API（X-Agent-Version:2，解析新/老两种格式）
    ├── config.go     # xray 配置文件读写（支持多 inbound）
    ├── schedule.go   # 定时同步（三态 diff：add/remove/change tier）
    ├── migrate.go    # xray_migrate_tier 指令实现（单→多 inbound 结构改造）
    ├── state.go      # 同步状态持久化
    └── proto/        # protobuf 生成代码
```

---

## 核心接口

### MetricProvider（collect 包）

```go
type MetricProvider interface {
    Collect(p *Payload)  // 只填自己负责的字段，其余保持零值
}
```

新增采集项：实现此接口，在 `ikev2.go` 或 `xray.go` 的 `buildProviders` 中注册。

### Handler（command 包）

```go
type Handler interface {
    Name() string
    Handle(data []byte) ([]byte, error)
}
```

新增指令：实现此接口，在 `main.go` 中 `dispatcher.Register(...)` 注册。

---

## 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `NODE_HOST` | **必填** | 节点 IP，用于构造 NATS subject |
| `NATS_URL` | `nats://127.0.0.1:4222` | NATS 服务地址 |
| `NATS_AUTH_TOKEN` | — | NATS 认证 token |
| `NODE_PROTOCOL` | `ikev2` | 协议类型：`ikev2` / `xray` |
| `SWAN_CONTAINER` | `strongswan` | StrongSwan 容器名 |
| `SWAN_IMAGE` | `mooc1988/swan:latest` | StrongSwan 默认镜像；swan_update 未传 image 时使用 |
| `XRAY_CONTAINER` | `xray` | Xray 容器名 |
| `XRAY_API_ADDR` | `127.0.0.1:10085` | Xray stats API 地址 |
| `XRAY_INBOUND_TAG` | `proxy` | Xray 入站 tag |
| `XRAY_CONFIG_PATH` | `/etc/xray/config.json` | Xray 配置文件路径 |
| `REPORT_INTERVAL` | `2m` | 上报间隔（Go duration 格式） |
| `API_BASE` | — | 后端 API 基地址（xray 用户同步需要） |
| `SCRIPT_TOKEN` | 同 `NATS_AUTH_TOKEN` | 访问后端 API 的 Bearer token |
| `RATELIMIT_ENABLED` | `true` | 限速总开关（xray 构建）；`false` 时 agent 不碰 tc 规则 |
| `RATELIMIT_IFACE` | 自动探测 | tc 工作的网卡；探测用 `ip route get 1.1.1.1`，失败 fallback 到 `eth0` |

---

## NATS Subject 规则

- `NODE_HOST` 中的 `.` 替换为 `-` 得到 `hostKey`
- 上报：`node.report.{hostKey}`
- 专属指令：`node.cmd.{hostKey}`
- 协议广播：`node.cmd.proto.{protocol}`

---

## 构建

### 本地开发构建

```bash
# ikev2 版（默认，无 xray 依赖，~6.7MB）
GOOS=linux GOARCH=amd64 go build -o node-agent-ikev2 .

# xray 版（含 gRPC/protobuf，~12MB）
GOOS=linux GOARCH=amd64 go build -tags xray -o node-agent-xray .
```

两个版本通过 Go build tag `xray` 区分：
- 默认构建：`ikev2.go` 生效，排除 `xray/`、`command/xray_user.go`、`collect/xray.go`
- `-tags xray`：`xray.go` 生效，包含完整 xray gRPC 用户管理栈

### 正式发布

版本号来源：`version-ikev2.txt` 和 `version-xray.txt`（格式 `1.0.5`，无 `v` 前缀）。

每个协议独立发布，tag 必须带协议后缀：

| 改动范围 | 操作 |
|---------|------|
| 仅 xray | bump `version-xray.txt`，推 `v1.0.6-xray` tag |
| 仅 ikev2 | bump `version-ikev2.txt`，推 `v1.0.6-ikev2` tag |
| 公共代码 | 两个版本文件都 bump，分别推 `v1.0.6-xray` 和 `v1.0.6-ikev2` 两个 tag |

```bash
# 公共改动示例
echo "1.0.6" > version-ikev2.txt && echo "1.0.6" > version-xray.txt
git add version-ikev2.txt version-xray.txt && git commit -m "chore: bump version to 1.0.6"
git tag v1.0.6-ikev2 && git tag v1.0.6-xray
git push origin v1.0.6-ikev2 v1.0.6-xray
```

GitHub Actions 根据 tag 后缀构建对应的 linux/amd64 产物并创建各自的 Release。

节点每天北京时间 02:00（含随机 jitter）自动检查更新，或通过后端下发 `agent:self_update` 立即触发。版本比较时自动忽略 `v` 前缀。

---

## 节点安装

由后端 `scripts/agent_setup.sh` 自动完成，bootstrap 链路触发：
1. 后端下发 `bootstrap` 指令
2. 节点运行协议脚本（xray/ikev2）
3. 成功后自动拉取并执行 `agent_setup.sh`，从 GitHub Releases 下载二进制并写入 systemd service
