# kd-vps-agent 通用功能文档

两种协议模式（IKEv2 / Xray）共用的功能。测试参考手册。

---

## 一、启动流程

```
LoadConfig()               读取环境变量，构建 Config
  ↓
日志初始化                  输出到 /var/log/node-agent.log，lumberjack 滚动
  ↓
newNATSConn()              连接 NATS，最多重试 10 次，每次间隔 10 秒
  ↓
信号注册                   监听 SIGINT / SIGTERM，触发 ctx.cancel()
  ↓
Dispatcher 注册            注册所有 Handler（3 个通用 + 协议专属）
  ↓
setupXray()                xray 构建才有实现，ikev2 返回 nil
  ↓
buildProviders()           按协议选择采集器
  ↓
NATS 订阅                  订阅 node.cmd.{hostKey} 和 node.cmd.proto.{protocol}
  ↓
startDailyJobs()           启动每日定时任务（goroutine）
  ↓
首次上报                   立即采集并发布到 node.report.{hostKey}
  ↓
主循环                     ticker 上报 + updateTicker 检查更新 + ctx.Done 退出
```

**NODE_HOST 为空时直接 `log.Fatal`，进程退出。**

---

## 二、环境变量（通用）

| 变量名 | 必需 | 默认值 | 说明 |
|--------|:----:|--------|------|
| `NODE_HOST` | ✓ | - | 节点主机名或 IP，`.` 替换为 `-` 后构成 NATS 主题 key |
| `NATS_URL` | | `nats://127.0.0.1:4222` | NATS 服务地址 |
| `NATS_AUTH_TOKEN` | | - | NATS 认证令牌，也作为 `SCRIPT_TOKEN` 的回退值 |
| `SCRIPT_TOKEN` | | = `NATS_AUTH_TOKEN` | HTTP API 认证令牌（优先级高于 NATS Token） |
| `API_BASE` | | - | 后端 API 基础地址，`bootstrap` 指令必须有效 |
| `NODE_PROTOCOL` | | `ikev2` | 协议类型：`ikev2` 或 `xray`，决定采集器和广播主题 |
| `REPORT_INTERVAL` | | `2m` | 指标上报间隔，Go duration 格式（如 `1m`、`30s`） |

> `REPORT_INTERVAL` 解析失败或 ≤ 0 时自动回退到 `2m`。

---

## 三、NATS 连接

- **连接名：** `kd-node-agent`
- **断线重连间隔：** 5 秒
- **最大重连次数：** 无限（`-1`）
- **启动重试：** 最多 10 次，每次间隔 10 秒，全部失败则 `log.Fatalf`
- **关闭方式：** `nc.Drain()`（等待消息处理完毕后关闭）

---

## 四、NATS 主题

`NODE_HOST` 中的 `.` 替换为 `-` 得到 `hostKey`，例如 `192.168.1.1` → `192-168-1-1`。

| 主题 | 方向 | 说明 |
|------|------|------|
| `node.report.{hostKey}` | 发送 | 定期上报指标，JSON 格式 Payload |
| `node.cmd.{hostKey}` | 接收 | 主机专属指令 |
| `node.cmd.proto.{protocol}` | 接收 | 协议广播指令（如 `node.cmd.proto.ikev2`） |

---

## 五、指令系统

### 消息格式

```json
{"cmd": "指令名", "data": {...}}
```

- `cmd`：Handler 名称
- `data`：各指令独立的 JSON payload，传给对应 Handler

### Request / Reply

若 NATS 消息的 `Reply` 字段非空（即调用方使用了 `nc.Request()`），Handler 的返回值会通过 `msg.Respond()` 回复给调用方。

### 通用响应格式

```json
{"ok": true,  "msg": "ok"}
{"ok": false, "msg": "错误描述"}
```

### 5.1 `bootstrap`

从后端拉取 Bootstrap 脚本并后台执行，用于初始化节点环境。

**请求 data：**

| 字段 | 类型 | 必填 | 说明 |
|------|------|:----:|------|
| `node_id` | int | ✓ | 节点 ID（不能为 0） |
| `script_name` | string | ✓ | 脚本名称 |

**实际执行命令（后台 nohup）：**

```bash
curl -fsSL \
  -H "Authorization: Bearer {SCRIPT_TOKEN}" \
  -H "X-Node-ID: {node_id}" \
  "{API_BASE}/api/scripts/bootstrap" \
| bash -s -- \
  --node-id {node_id} \
  --token {SCRIPT_TOKEN} \
  --script-name {script_name} \
  --api-base {API_BASE} \
  --nats-url {NATS_URL} \
  --nats-token {NATS_AUTH_TOKEN}
```

- 日志写入：`/tmp/bootstrap_{node_id}.log`
- `API_BASE` 或 `SCRIPT_TOKEN` 为空时返回错误响应，不执行

**响应：** `{"ok": true, "msg": "ok"}` 或 `{"ok": false, "msg": "..."}`

---

### 5.2 `docker_restart`

拉取新镜像并重启指定容器。

**请求 data：**

| 字段 | 类型 | 必填 | 说明 |
|------|------|:----:|------|
| `image` | string | ✓ | 镜像名，如 `teddysun/xray:latest` |
| `container` | string | ✓ | 容器名，如 `xray` |

**执行步骤（顺序）：**

1. `docker pull {image}` — 失败则返回错误，不继续
2. `docker restart {container}` — 失败则返回错误

**响应：** `{"ok": true, "msg": "ok"}` 或 `{"ok": false, "msg": "docker pull failed: ..."}`

---

### 5.3 `agent:self_update`

主动触发 agent 自我更新。

**无请求参数。**

调用 `update.TryUpdate(Version)`，若有新版本则下载替换并重启。

---

## 六、指标采集

每次 `ticker` 触发时采集一次，结果发布到 `node.report.{hostKey}`。

### Payload 结构（JSON）

| 字段 | JSON Key | 含义 | 采集来源 |
|------|----------|------|---------|
| `C` | `c` | 固定值 `"tr"` | Collector |
| `M` | `m` | 月累计流量，如 `"12.3G"` | TrafficProvider |
| `D` | `d` | 日累计流量，如 `"1.5G"` | TrafficProvider |
| `Conn` | `conn` | 当前连接数（十进制字符串） | 协议专属 Provider |
| `Mem` | `mem` | 内存占用百分比（整数字符串，如 `"72"`） | SysProvider |
| `CPU` | `cpu` | CPU 占用百分比（整数字符串） | SysProvider |
| `Disk` | `disk` | 磁盘占用百分比（整数字符串） | SysProvider |
| `SV` | `s_v` | 协议软件版本，如 `"6.0.4"` | 协议专属 Provider |
| `AV` | `a_v` | agent 版本，如 `"1.0.5"` | main（version.txt） |

> `CPU` 第一次采集时为空字符串（无历史数据无法计算增量）。

### 系统指标（SysProvider）

| 指标 | 数据源 | 计算方式 |
|------|--------|---------|
| 内存 | `/proc/meminfo` | `(MemTotal - MemAvailable) / MemTotal × 100`，取整 |
| CPU | `/proc/stat` 第一行 | 两次采样的 `(deltaTotal - deltaIdle) / deltaTotal × 100` |
| 磁盘 | `syscall.Statfs("/")` | `(Blocks - Bfree) × Bsize / (Blocks × Bsize) × 100` |

### 流量采集（TrafficProvider）

- **数据源：** `/proc/net/dev` 网卡 TX（出站字节，与云平台计费一致）
- **网卡自动探测：** 选 TX 最高的物理网卡，排除 `lo`、`docker*`、`veth*`、`br-*`
- **累积方式：** 每次取 delta（`tx - prevTx`），负值过滤为 0（防计数器回绕）
- **日重置：** `now.Day() != lastDay` 时 `dayBytes = 0`
- **月重置：** `now.Month() != lastMonth` 时 `monthBytes = 0`
- **格式：** `%.1fG`，如 `"1.5G"`、`"0.0G"`
- **持久化：** 每次采集后写入 `/var/lib/node-agent/traffic.json`，重启后恢复当天/当月累计

**持久化文件格式：**

```json
{
  "day_bytes": 1610612736,
  "month_bytes": 12884901888,
  "last_day": 14,
  "last_month": 3
}
```

---

## 七、自动更新

### 定期检查

`dailyScheduler` 每天北京时间 02:00（含随机 jitter）触发 `update.CheckAndUpdate(Version)`，也可通过 `agent:self_update` 指令立即触发。

### 更新流程

```
请求 GitHub API
https://api.github.com/repos/salt-lake/kd-vps-agent/releases/latest
  ↓
取 tag_name，去掉 v 前缀后与当前版本比较
  ↓ 版本不同
下载二进制
https://github.com/salt-lake/kd-vps-agent/releases/download/{tag}/node-agent-ikev2  # 或 node-agent-xray
  ↓
写到 {self}.new（权限 0755）
  ↓
os.Rename({self}.new, {self})  原子替换
  ↓
systemctl restart node-agent
```

- **HTTP 超时：** 60 秒
- **版本比较：** 去掉 `v` 前缀，`v1.0.5` == `1.0.5`
- **已是最新版：** 静默返回，无日志

---

## 八、日志

- **位置：** `/var/log/node-agent.log`
- **格式：** `2006/01/02 15:04:05 <message>`（`log.LstdFlags`）
- **轮转（lumberjack）：**

| 参数 | 值 |
|------|-----|
| 单文件最大 | 20 MB |
| 保留备份数 | 3 个 |
| 压缩备份 | 是 |

---

## 九、持久化文件汇总

| 文件 | 内容 | 说明 |
|------|------|------|
| `/var/log/node-agent.log` | 运行日志 | lumberjack 滚动 |
| `/var/lib/node-agent/traffic.json` | 日/月流量计数 | 两种模式均写入 |
| `/var/lib/node-agent/sync_state.json` | xray 同步时间戳 | **仅 xray 模式** |

---

## 十、版本管理

- **版本来源：** 编译时嵌入 `version.txt`（`//go:embed version.txt`），无 `v` 前缀，如 `1.0.5`
- **启动日志：** `node-agent version=1.0.5 host=... protocol=...`
- **上报字段：** `Payload.AV`（`a_v`）

---

## 十一、平滑关闭

捕获 `SIGINT` / `SIGTERM` 后：

1. `ctx.cancel()` — 通知所有 goroutine 退出
2. `nc.Drain()` — 等待 NATS 消息处理完毕后关闭连接
3. 主循环 `ctx.Done()` case 触发，打印 `shutting down` 后退出
