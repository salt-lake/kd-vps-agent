# node-agent CLAUDE.md

## 模块职责

node-agent 是部署在 VPS 节点上的独立 Go 二进制，负责：

1. **指标采集上报**：定期采集系统指标（CPU/内存/磁盘/流量/连接数），通过 NATS 上报给后端
2. **指令订阅执行**：监听 NATS 下发的运维指令（如 docker restart）
3. **自更新**：每小时检查后端 API 版本，如有新版本则下载替换二进制并 `systemctl restart`
4. **每日任务**：北京时间 04:00 清空 charon.log

---

## 目录结构

```
cmd/node-agent/
├── main.go           # 入口：NATS 连接、Provider 组装、主循环
├── job_daily.go      # 每日定时任务（clearCharonLog）
├── collect/
│   ├── collector.go  # Payload 结构、MetricProvider 接口、Collector 组合
│   ├── sys.go        # CPU(/proc/stat)、内存(/proc/meminfo)、磁盘(syscall.Statfs)
│   ├── traffic.go    # TX 流量累积（/proc/net/dev），持久化到 /var/lib/node-agent/traffic.json
│   ├── swan.go       # IKEv2/StrongSwan：连接数 + 版本（docker exec ipsec）
│   └── xray.go       # Xray：连接数（stats API）+ 版本（docker exec xray version）
├── command/
│   ├── dispatcher.go      # NATS 消息路由到 Handler
│   └── docker_restart.go  # docker pull + restart 指令
└── update/
    └── updater.go    # 检查版本 → 下载新二进制 → 替换 → systemctl restart
```

---

## 核心接口

### MetricProvider（collect 包）

```go
type MetricProvider interface {
    Collect(p *Payload)  // 只填自己负责的字段，其余保持零值
}
```

新增采集项：实现此接口，在 `main.go` 的 `providers` 切片中注册。

### Handler（command 包）

```go
type Handler interface {
    Name() string
    Handle(data []byte) ([]byte, error)  // data 为 cmdMsg.Data 原始 JSON
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
| `XRAY_CONTAINER` | `xray` | Xray 容器名 |
| `XRAY_API_ADDR` | `127.0.0.1:10085` | Xray stats API 地址 |
| `NET_IFACE` | `eth0` | 流量统计网卡 |
| `REPORT_INTERVAL` | `2m` | 上报间隔（Go duration 格式） |
| `API_BASE` | — | 后端 API 基地址，启用自更新需设置 |
| `SCRIPT_TOKEN` | 同 `NATS_AUTH_TOKEN` | 访问后端 API 的 Bearer token |

---

## NATS Subject 规则

- `NODE_HOST` 中的 `.` 替换为 `-` 得到 `hostKey`
- 上报：`node.report.{hostKey}`
- 专属指令：`node.cmd.{hostKey}`
- 协议广播：`node.cmd.proto.{protocol}`（向同协议所有节点广播）

---

## 上报 Payload 字段

```go
type Payload struct {
    C    string `json:"c"`    // 固定 "tr"
    M    string `json:"m"`    // 月累积 TX 流量（如 "12.3G"）
    D    string `json:"d"`    // 日累积 TX 流量（如 "0.8G"）
    Conn string `json:"conn"` // 连接数，格式 "N,0"（ipv4,ipv6）
    Mem  string `json:"mem"`  // 内存使用率（整数百分比字符串）
    CPU  string `json:"cpu"`  // CPU 使用率（差分计算，首次为空）
    Disk string `json:"disk"` // 磁盘使用率
    SV   string `json:"s_v"`  // 协议软件版本号
}
```

---

## 版本管理

**每次修改 agent 代码后必须更新版本号**，否则自更新机制不会触发（agent 对比版本字符串决定是否下载）。

版本号**唯一来源**是 `cmd/node-agent/version.txt`，通过 `//go:embed` 嵌入二进制：

```
cmd/node-agent/version.txt   ← 修改这里
```

**每次修改 agent 代码，只需更新 `version.txt` 中的版本号**，构建时自动嵌入二进制，Dockerfile 同时将此文件复制为 `/app/node-agent.version`，后端 `GET /api/agent/version` 读取该文件返回版本号。

**发布流程**：

1. 修改 `version.txt`（如改为 `1.0.1`）
2. 正常构建镜像：`docker build .`
   产物：`/app/node-agent`（内嵌版本）+ `/app/node-agent.version`（供后端读取）
3. 触发节点拉取：`POST /api/nodes/actions/agent-self-update {"nodeIds": [...]}`
   或等待 agent 每小时自动检查

---

## 构建

```bash
# 开发构建
go build ./cmd/node-agent/

# 生产构建（注入版本号）
go build -ldflags "-X main.Version=1.2.3" ./cmd/node-agent/
```

---

## 开发约定

- **新增协议支持**：在 `collect/` 下新建 `{proto}.go`，实现 `MetricProvider`，在 `main.go` switch 中添加 case
- **新增指令**：在 `command/` 下新建 handler 文件，在 `main.go` 中 `Register`
- **流量状态持久化路径**：`/var/lib/node-agent/traffic.json`（重启不丢数据）
- **CPU 采集**：差分计算，第一次采集返回空字符串属正常行为
