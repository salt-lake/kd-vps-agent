# kd-vps-agent（IKEv2 模式）功能文档

测试参考手册：涵盖启动配置、指令、指标采集、定时任务。

---

## 一、构建与启动

### 构建方式

IKEv2 模式为**默认构建**，不需要额外 build tag：

```bash
go build -o node-agent-ikev2 .
```

构建产物 `node-agent-ikev2`（约 6.7MB），排除 xray 相关代码（xray/、command/xray_user.go）。

### 环境变量

| 变量名 | 必需 | 默认值 | 说明 |
|--------|:----:|--------|------|
| `NODE_HOST` | ✓ | - | 节点主机名或 IP，用于生成 NATS 主题 |
| `NATS_URL` | | `nats://127.0.0.1:4222` | NATS 服务地址 |
| `NATS_AUTH_TOKEN` | | - | NATS 认证令牌 |
| `NODE_PROTOCOL` | | `ikev2` | 协议类型（IKEv2 模式保持默认或显式设为 `ikev2`） |
| `SWAN_CONTAINER` | | `strongswan` | StrongSwan Docker 容器名 |
| `REPORT_INTERVAL` | | `2m` | 指标上报间隔（Go duration 格式，如 `1m`、`5m`） |

> `API_BASE`、`SCRIPT_TOKEN`、`XRAY_*` 等变量 IKEv2 模式下不使用。

---

## 二、NATS 通信

### 主题规则

`NODE_HOST` 中的 `.` 替换为 `-` 得到 `hostKey`，例如 `192.168.1.1` → `192-168-1-1`。

| 主题 | 方向 | 说明 |
|------|------|------|
| `node.report.{hostKey}` | 发送 | 定期上报指标数据 |
| `node.cmd.{hostKey}` | 接收 | 主机专属指令 |
| `node.cmd.proto.ikev2` | 接收 | 协议广播指令（发给所有 ikev2 节点） |

### 指令消息格式

```json
{"cmd": "指令名", "data": {...}}
```

若为 Request/Reply 模式（`msg.Reply` 非空），handler 返回的结果会回复给请求方。

---

## 三、NATS 指令列表

### 3.1 `bootstrap`

从后端拉取 Bootstrap 脚本并后台执行，用于初始化节点环境（安装 StrongSwan、配置证书等）。

**请求 data 字段：**

| 字段 | 类型 | 必填 | 说明 |
|------|------|:----:|------|
| `NodeID` | int | ✓ | 节点 ID |
| `ScriptName` | string | ✓ | 脚本名称 |

**执行命令：**

```bash
curl -fsSL \
  -H "Authorization: Bearer {SCRIPT_TOKEN}" \
  -H "X-Node-ID: {NodeID}" \
  "{API_BASE}/api/scripts/bootstrap" \
| bash -s -- \
  --node-id {NodeID} \
  --token {SCRIPT_TOKEN} \
  --script-name {ScriptName} \
  --api-base {API_BASE} \
  --nats-url {NATS_URL} \
  --nats-token {NATS_AUTH_TOKEN}
```

**后台执行，日志写入：** `/tmp/bootstrap_{NodeID}.log`

**无响应返回**（fire-and-forget）。

---

### 3.2 `docker_restart`

拉取新镜像并重启指定容器。IKEv2 场景下用于更新 StrongSwan 镜像。

**请求 data 字段：**

| 字段 | 类型 | 必填 | 说明 |
|------|------|:----:|------|
| `Image` | string | ✓ | 镜像名，如 `teddysun/strongswan:latest` |
| `Container` | string | ✓ | 容器名，如 `strongswan` |

**执行步骤：**

1. `docker pull {Image}`
2. `docker restart {Container}`

**响应：**

```json
{"ok": true, "msg": ""}
// 失败时
{"ok": false, "msg": "错误信息"}
```

---

### 3.3 `agent:self_update`

主动触发 agent 自我更新（从 GitHub Releases 拉取最新版本替换自身）。

**无请求参数。**

**行为：** 调用 `update.TryUpdate()`，若有新版本则下载、替换、重启进程。

---

## 四、指标采集与上报

每隔 `REPORT_INTERVAL`（默认 2 分钟）采集一次，通过 NATS 发送到 `node.report.{hostKey}`。

### 上报字段（Payload）

| 字段 | JSON Key | 含义 | 采集来源 |
|------|----------|------|---------|
| `C` | `c` | 固定值 `"tr"` | - |
| `M` | `m` | 月累计流量（如 `12.3G`） | `/proc/net/dev` TX |
| `D` | `d` | 日累计流量（如 `1.5G`） | `/proc/net/dev` TX |
| `Conn` | `conn` | 当前 IKEv2 活跃连接数 | `docker exec ipsec statusall` |
| `Mem` | `mem` | 内存占用百分比（如 `72%`） | `/proc/meminfo` |
| `CPU` | `cpu` | CPU 占用百分比（如 `15%`） | `/proc/stat` 增量计算 |
| `Disk` | `disk` | 磁盘占用百分比（如 `45%`） | `syscall.Statfs("/")` |
| `SV` | `s_v` | StrongSwan 版本（如 `6.0.4`） | `docker exec ipsec version` |
| `AV` | `a_v` | agent 版本（如 `1.0.5`） | `version.txt` |

### IKEv2 连接数采集

```bash
docker exec strongswan ipsec statusall
```

- 查找输出中含 `ESTABLISHED` 的行
- 正则提取对端 IP：`ESTABLISHED.*\.\.\.\s*([^\s\[]+)`
- 仅统计 IPv4 对端（过滤非 IPv4 地址）
- 返回十进制数字字符串，如 `"5"`

### StrongSwan 版本采集

```bash
docker exec strongswan ipsec version
```

- 正则提取：`U(\S+?)/`
- 示例输出 `Linux strongSwan U6.0.4/K5.15.0` → 提取结果 `6.0.4`

### 系统指标采集

| 指标 | 数据源 | 计算方式 |
|------|--------|---------|
| 内存 | `/proc/meminfo` | `(MemTotal - MemAvailable) / MemTotal × 100` |
| CPU | `/proc/stat` | 两次采样增量计算，首次返回空 |
| 磁盘 | `syscall.Statfs("/")` | `(Blocks - Bfree) × Bsize / (Blocks × Bsize) × 100` |

### 流量采集

- 数据源：`/proc/net/dev` 网卡 TX（出站）
- 自动选择 TX 最高的物理网卡（排除 lo、docker\*、veth\*、br-\*）
- 仅累加 delta（`tx - prevTx`），过滤负值（防计数器回绕）
- 持久化文件：`/var/lib/node-agent/traffic.json`
- 日重置：每天 00:00
- 月重置：每月 1 日 00:00

---

## 五、定时任务

| 任务 | 触发时间 | 说明 |
|------|---------|------|
| ClearCharonLog（清日志） | 每日 04:00 CST | 清空 StrongSwan charon 日志 |
| VersionCheck（版本检查） | 每小时 | 检查 GitHub Releases 是否有新版本 |
| 指标上报 | 每 `REPORT_INTERVAL` | 采集并发送到 NATS |

> FullSync（03:00 CST）是 xray 模式专属，IKEv2 模式下该任务传入 `nil` 直接跳过。

### 清空 charon.log 详情

每日北京时间 04:00 执行：

```bash
docker exec strongswan sh -c "test -f /var/log/charon.log && truncate -s 0 /var/log/charon.log || true"
```

- `test -f` 检查文件存在性
- `truncate -s 0` 清空内容但保留文件
- `|| true` 确保无论成功失败不报错

---

## 六、自更新机制

agent 每小时检查 GitHub Releases 最新版本，也可通过 `agent:self_update` 指令主动触发。

**版本比较：** 去掉 `v` 前缀后比较（兼容 GitHub tag `v1.0.5` 与 `version.txt` 中的 `1.0.5`）。

**更新流程：**
1. 请求 GitHub API 获取最新 Release tag
2. 比较当前版本，有新版本则下载对应二进制
3. 原子替换自身可执行文件
4. `systemctl restart node-agent` 重启进程

---

## 七、日志

- **位置：** `/var/log/node-agent.log`
- **轮转：** 单文件 20MB，最多 3 个备份，启用压缩
- **格式：** `2006/01/02 15:04:05 <message>`

---

## 八、持久化文件

| 文件 | 内容 | 说明 |
|------|------|------|
| `/var/lib/node-agent/traffic.json` | `{"day_bytes":...,"month_bytes":...,"last_day":...,"last_month":...}` | 日/月流量计数，重启不丢失 |

---

## 九、与 Xray 模式的差异

| 功能点 | IKEv2 | Xray |
|--------|--------|------|
| 构建 tag | 无（默认） | `-tags xray` |
| 连接数采集 | `docker exec ipsec statusall` | Xray gRPC stats API |
| 软件版本采集 | `docker exec ipsec version` | `docker exec xray version` |
| 用户管理 | 无（脚本部署时配置） | gRPC 动态注入 |
| 同步模块 | 无 | StartupSync + DeltaSync + FullSync |
| 专属定时任务 | 04:00 清 charon.log | 03:00 全量用户同步 |
| 专属指令 | 无额外指令 | `xray_user_add`、`xray_user_remove` |
| gRPC 依赖 | 无 | 有（Xray API） |
| 配置文件管理 | 无 | 动态写 `config.json` |
