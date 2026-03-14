# node-agent CLAUDE.md

## 模块职责

node-agent 是部署在 VPS 节点上的独立 Go 二进制，负责：

1. **指标采集上报**：定期采集系统指标（CPU/内存/磁盘/流量/连接数），通过 NATS 上报给后端
2. **指令订阅执行**：监听 NATS 下发的运维指令（如 docker restart）
3. **自更新**：每小时检查 GitHub Releases 最新版本，若不同则下载替换二进制并 `systemctl restart node-agent`
4. **每日任务**：北京时间 04:00 清空 charon.log

---

## 目录结构

```
kd-vps-agent/
├── main.go           # 入口：NATS 连接、Provider 组装、主循环
├── job_daily.go      # 每日定时任务（clearCharonLog）
├── version.txt       # 版本号（go:embed 嵌入二进制），格式：1.0.3（无 v 前缀）
├── collect/
│   ├── collector.go  # Payload 结构、MetricProvider 接口、Collector 组合
│   ├── sys.go        # CPU(/proc/stat)、内存(/proc/meminfo)、磁盘(syscall.Statfs)
│   ├── traffic.go    # TX 流量累积（/proc/net/dev），持久化到 /var/lib/node-agent/traffic.json
│   ├── swan.go       # IKEv2/StrongSwan：连接数 + 版本（docker exec ipsec）
│   └── xray.go       # Xray：连接数（stats API）+ 版本（docker exec xray version）
├── command/
│   ├── dispatcher.go      # NATS 消息路由到 Handler
│   ├── docker_restart.go  # docker pull + restart 指令
│   ├── bootstrap.go       # bootstrap 指令
│   ├── self_update.go     # agent:self_update 指令（触发立即自更新）
│   └── xray_user.go       # xray 用户增删指令
├── sync/                  # Xray 用户同步
├── update/
│   └── updater.go    # 检查 GitHub Releases → 下载新二进制 → 替换 → systemctl restart
├── xray/                  # Xray gRPC API 封装
└── deploy/
    ├── node-agent.service  # systemd service 文件
    └── install.sh          # 节点安装脚本
```

---

## 核心接口

### MetricProvider（collect 包）

```go
type MetricProvider interface {
    Collect(p *Payload)  // 只填自己负责的字段，其余保持零值
}
```

新增采集项：实现此接口，在 `main.go` 的 `buildProviders` 中注册。

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
| `XRAY_CONTAINER` | `xray` | Xray 容器名 |
| `XRAY_API_ADDR` | `127.0.0.1:10085` | Xray stats API 地址 |
| `REPORT_INTERVAL` | `2m` | 上报间隔（Go duration 格式） |
| `API_BASE` | — | 后端 API 基地址（xray 用户同步需要） |
| `SCRIPT_TOKEN` | 同 `NATS_AUTH_TOKEN` | 访问后端 API 的 Bearer token |

---

## NATS Subject 规则

- `NODE_HOST` 中的 `.` 替换为 `-` 得到 `hostKey`
- 上报：`node.report.{hostKey}`
- 专属指令：`node.cmd.{hostKey}`
- 协议广播：`node.cmd.proto.{protocol}`

---

## 版本管理与发布

版本号唯一来源：`version.txt`（格式 `1.0.3`，无 `v` 前缀）。

**发布流程**：
1. 修改 `version.txt`（如改为 `1.0.4`）
2. 提交并打 tag：
   ```bash
   git add version.txt && git commit -m "chore: bump version to 1.0.4"
   git tag v1.0.4 && git push origin v1.0.4
   ```
3. GitHub Actions 自动构建 linux/amd64 二进制并挂到 Release
4. 节点每小时自动检查更新，或通过后端下发 `agent:self_update` 立即触发

版本比较时自动忽略 `v` 前缀（`v1.0.4` == `1.0.4`）。

---

## 节点安装

```bash
# 首次安装（自动拉取最新版）
bash <(curl -fsSL https://raw.githubusercontent.com/salt-lake/kd-vps-agent/main/deploy/install.sh)

# 安装指定版本
bash deploy/install.sh v1.0.3
```

安装完成后填写 `/etc/node-agent/env` 中的环境变量，然后 `systemctl restart node-agent`。
