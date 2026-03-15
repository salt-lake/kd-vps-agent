# kd-vps-agent（Xray 模式）功能文档

测试参考手册：涵盖启动配置、指令、同步逻辑、指标采集、定时任务。

---

## 一、构建与启动

### 构建标签

| 构建方式 | 命令 | 说明 |
|---------|------|------|
| Xray 模式 | `go build -tags xray -o agent .` | 启用 xray 用户同步、gRPC 等功能 |
| IKEv2 模式 | `go build -o agent .` | 仅 StrongSwan 相关功能 |

### 环境变量

| 变量名 | 必需 | 默认值 | 说明 |
|--------|:----:|--------|------|
| `NODE_HOST` | ✓ | - | 节点主机名，用于生成 NATS 主题 |
| `NATS_URL` | | `nats://localhost:4222` | NATS 服务地址 |
| `NATS_AUTH_TOKEN` | | - | NATS 认证令牌 |
| `API_BASE` | ✓(xray) | - | 后端 API 基础地址，如 `http://api.example.com` |
| `SCRIPT_TOKEN` | | = `NATS_AUTH_TOKEN` | HTTP API 认证令牌（优先级高于 NATS Token） |
| `NODE_PROTOCOL` | | `ikev2` | 协议类型：`ikev2` 或 `xray` |
| `XRAY_CONTAINER` | | `xray` | Xray Docker 容器名 |
| `XRAY_API_ADDR` | | `127.0.0.1:10085` | Xray gRPC API 地址 |
| `XRAY_INBOUND_TAG` | | `vless` | Xray inbound 标签 |
| `XRAY_CONFIG_PATH` | | `/etc/xray/config.json` | Xray 配置文件路径 |
| `SWAN_CONTAINER` | | `strongswan` | StrongSwan 容器名（ikev2 模式） |
| `REPORT_INTERVAL` | | `2m` | 指标上报间隔（Go duration 格式） |

---

## 二、NATS 通信

### 订阅主题

| 主题 | 方向 | 说明 |
|------|------|------|
| `node.cmd.{HOST}` | 接收 | 主机特定指令 |
| `node.cmd.proto.{PROTOCOL}` | 接收 | 协议广播指令（如 `node.cmd.proto.xray`） |
| `node.report.{HOST}` | 发送 | 定期上报指标数据 |

### 指令消息格式

```json
{"cmd": "指令名", "data": {...}}
```

---

## 三、NATS 指令列表

### 3.1 `bootstrap`

执行远程 bootstrap 脚本，用于初始化节点环境。

**请求 data 字段：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `NodeID` | int | 节点 ID |
| `ScriptName` | string | 脚本名称 |

**行为：**
1. 从 `$API_BASE/api/scripts/bootstrap` 拉取 Shell 脚本（携带 `SCRIPT_TOKEN` 认证）
2. 后台执行，日志写入 `/tmp/bootstrap_{NodeID}.log`

---

### 3.2 `docker_restart`

拉取新镜像并重启指定容器。

**请求 data 字段：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `Image` | string | 镜像名，如 `teddysun/xray:latest` |
| `Container` | string | 容器名，如 `xray` |

**步骤：**`docker pull $Image` → `docker restart $Container`

**响应：**
```json
{"ok": true, "msg": ""}
```

---

### 3.3 `agent:self_update`

触发 agent 自我更新（从 GitHub Releases 拉取最新版本替换自身）。

**无请求参数。**

---

### 3.4 `xray_user_add`（仅 xray 构建）

通过 gRPC 动态向 Xray 添加单个用户。

**请求 data 字段：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `UUID` | string | 用户 UUID |
| `Email` | string | 用户邮箱（可选，用于标识） |

**注意：** 此操作只修改 Xray 内存，不写配置文件，重启后会丢失。持久化依赖定时同步。

---

### 3.5 `xray_user_remove`（仅 xray 构建）

通过 gRPC 动态从 Xray 移除单个用户。

**请求 data 字段：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `UUID` | string | 用户 UUID |
| `Email` | string | 用户邮箱（可选） |

---

## 四、Xray 用户同步（仅 xray 构建）

### 4.1 同步操作一览

| 操作 | 触发时机 | 持久化配置 | 重启容器 | 说明 |
|------|---------|:---------:|:-------:|------|
| **StartupSync** | 启动时 | ✓ | 仅 gRPC 失败时 | 全量拉取，写配置文件，尝试 gRPC 注入，失败则重启 |
| **DeltaSync** | 每小时 | ✗ | ✗ | 拉增量变更（added/removed），无状态文件时降级为 FullSync |
| **FullSync** | 每日 03:00 | ✗ | ✗ | 全量拉取并 diff，容错单用户失败 |
| **AddUser** | 按需（NATS 指令） | ✗ | ✗ | gRPC 动态添加单用户 |
| **RemoveUser** | 按需（NATS 指令） | ✗ | ✗ | gRPC 动态移除单用户 |

### 4.2 StartupSync 流程

```
启动
  └─ 全量拉取用户列表（GET /api/agent/xray/users）
       └─ 写入 config.json（持久化）
            └─ 尝试 gRPC 动态注入（injectUsers）
                 ├─ 成功 → 跳过重启，保存 sync state
                 └─ 失败 → docker restart $XRAY_CONTAINER → 保存 sync state
```

如果 StartupSync 失败，每 30 秒重试一次，直到成功。

### 4.3 DeltaSync 流程

```
读取 sync_state.json 获取 last_sync_time
  ├─ 无状态文件 → 降级执行 FullSync
  └─ 有状态 → GET /api/agent/xray/users/delta?since={last_sync_time}
                  └─ 对 added 逐一调用 AddUser（gRPC）
                  └─ 对 removed 逐一调用 RemoveUser（gRPC）
                  └─ 更新 sync state
```

### 4.4 FullSync 流程

```
GET /api/agent/xray/users（全量）
  └─ diff(current, remote)
       ├─ 多余用户 → RemoveUser（gRPC，单个失败不中断）
       └─ 缺失用户 → AddUser（gRPC，单个失败不中断）
```

### 4.5 保护用户

UUID `a1b2c3d4-0000-0000-0000-000000000001` 为测试用户，**永不被移除**。

### 4.6 后端 API 接口

| 端点 | 方法 | 认证 | 参数 | 响应 |
|------|------|------|------|------|
| `/api/agent/xray/users` | GET | `Bearer $SCRIPT_TOKEN` | - | `{"code":200,"data":[{"uuid":"..."}]}` |
| `/api/agent/xray/users/delta` | GET | `Bearer $SCRIPT_TOKEN` | `?since=<unix_seconds>` | `{"code":200,"data":{"added":[...],"removed":[...]}}` |

### 4.7 状态文件

| 文件 | 内容 | 用途 |
|------|------|------|
| `/var/lib/node-agent/sync_state.json` | `{"last_sync_time": 1234567890}` | DeltaSync 时间基准 |

---

## 五、Reality Dest 检测（仅 xray 构建）

每 5 分钟检测当前 Reality dest 是否 TCP 可达（超时 3 秒）。

**候选 dest 列表：**
- `www.apple.com:443`
- `www.microsoft.com:443`
- `www.cloudflare.com:443`
- `www.amazon.com:443`

**不可达时：** 从候选列表中轮选新 dest → 更新 `config.json` → `docker restart $XRAY_CONTAINER`

---

## 六、指标采集与上报

每隔 `REPORT_INTERVAL`（默认 2 分钟）采集一次，通过 NATS 发送到 `node.report.{HOST}`。

### 上报字段

| 字段 | 含义 | 采集来源 |
|------|------|---------|
| `C` | 类型标识（固定 `"tr"`） | - |
| `M` | 月累计流量（GB，如 `12.3G`） | `/proc/net/dev` TX |
| `D` | 日累计流量（GB） | `/proc/net/dev` TX |
| `Conn` | 当前活跃连接数 | Xray gRPC stats / ipsec statusall |
| `Mem` | 内存占用百分比（如 `72%`） | `/proc/meminfo` |
| `CPU` | CPU 占用百分比（如 `15%`） | `/proc/stat` 增量计算 |
| `Disk` | 磁盘占用百分比（如 `45%`） | syscall.Statfs("/") |
| `SV` | 协议软件版本（如 `1.8.4`） | docker exec xray version |
| `AV` | agent 版本 | version.txt |

### Xray 连接数统计

```bash
docker exec xray xray api statsquery --server=127.0.0.1:10085 --pattern 'user>>>' --reset=true
```
统计 downlink 不为 0 的用户数（在线活跃用户）。

### 流量状态文件

`/var/lib/node-agent/traffic.json` — 持久化日/月流量计数，重启不丢失。

---

## 七、定时任务

| 任务 | 触发时间 | 说明 |
|------|---------|------|
| FullSync（全量同步） | 每日 03:00 CST | Xray 用户全量拉取 + diff |
| ClearLog（清日志） | 每日 04:00 CST | docker exec 清空 `/var/log/charon.log` |
| DeltaSync（增量同步） | 每小时 | 拉增量用户变更并应用 |
| CheckDest（dest 检测） | 每 5 分钟 | Reality dest 可达性检测 |
| VersionCheck（版本检查） | 每小时 | 从 GitHub Releases 检查更新 |
| Startup 重试 | 启动失败时，每 30 秒 | 重试 StartupSync 直到成功 |
| 指标上报 | 每 `REPORT_INTERVAL` | 采集并发送到 NATS |

---

## 八、日志

- **位置：** `/var/log/node-agent.log`
- **轮转：** 单文件 20MB，最多 3 个备份，启用压缩
- **格式：** `2006/01/02 15:04:05 <message>`

---

## 九、自更新

agent 每小时检查 GitHub Releases 最新版本。也可通过 NATS 指令 `agent:self_update` 主动触发。

更新流程：下载新二进制 → 替换自身 → 重启进程。

---

## 十、gRPC 连接管理

- 长连接懒加载：首次使用时创建，后续复用
- 连接异常时自动清空，下次重建
- 目标：`XRAY_API_ADDR`（默认 `127.0.0.1:10085`）
- 协议：gRPC，无 TLS
