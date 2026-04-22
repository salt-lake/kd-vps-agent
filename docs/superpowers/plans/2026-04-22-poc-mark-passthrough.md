# mark 透传 PoC 结论

**日期**：2026-04-22
**测试机**：`149.104.78.146` (Ubuntu 22.04.1 LTS, kernel 5.15.0-60-generic)
**iproute2**：iptables-legacy 1.8.7, tc from iproute2 5.15
**结论**：**早期设计用 xray outbound sockopt.mark 方向错误**；改用 **iptables mangle 按源端口打 mark** 后机制完全生效

---

## 测试概览

在真实 Linux 环境里走了 3 组对比：

| # | 打 mark 方式 | 产生的流量方向 | tc class 1:10 计数 | overlimits |
|---|---|---|---|---|
| A | xray outbound `sockopt.mark=1` | 通过 xray 代理下载 10MB（ingress-dominated）| 49KB（仅 ACK）| 0 |
| B | `iptables -t mangle -A OUTPUT -p tcp --dport 443 -j MARK --set-mark 1` | curl PUT 30MB 到 https://target/（egress-dominated）| **34.4MB** | **11155** ✅ |
| C | `iptables -t mangle -A OUTPUT -p tcp --sport 8080 -j MARK --set-mark 1` | 远端 mac 客户端下载 server:8080 上的 7MB 文件（egress to user）| **7.1MB** | **2420** ✅ |

## 关键发现

1. **sockopt.mark 方向问题**（测试 A）：
   - xray 的 outbound 配置里 `sockopt.mark` 只作用在 xray 发起的到 target 的那个 socket 上
   - 用户代理下载的大流量走的路径是：`target → VPS eth1 ingress → xray → VPS eth1 egress → user`
   - egress-to-user 那一段走的是 **xray 接收 user 时 accept 出来的 socket**（inbound socket），那个 socket 没有被 outbound 配置影响
   - 结果：tc 只看到 outbound socket 的 egress（发给 target 的 TCP ACK，49KB）

2. **iptables 源端口打 mark 是正确方案**（测试 C）：
   - xray inbound 监听在 port X（用户连进来的端口）
   - 用户下行流量就是 `VPS egress` 方向、`src_port = X`
   - `iptables -t mangle -A OUTPUT -p tcp --sport X -j MARK --set-mark N` 精确捕获
   - kernel `skb->mark = N`，tc fw filter 读到，分类到对应 HTB class，fq_codel 公平 + HTB 限速

3. **限速实际生效证据**（测试 B/C 的 `overlimits`）：
   - HTB 每发现超过配额的包就递增 overlimits
   - 测试 B 下 30MB 上传在 10Mbit 限速下耗时 23.4s（≈10Mbps），匹配理论
   - 测试 C 下 7MB 下载 overlimits=2420 说明内核在持续整形

## 方案修正

原 spec §3.2 里设计的 outbound 带 sockopt.mark、routing 按 tier 路由，**全部废弃**。

**新方案**（已实现，commit `5bbd5e7`）：

- xray 配置改造**只涉及 inbounds**：每个 tier 一个 inbound（对应一个端口范围），outbounds 和 routing 保持原样
- `ratelimit/` 包通过 iptables 生成打标规则：
  ```
  iptables -t mangle -A OUTPUT -p tcp --sport <tier-port-range> -j MARK --set-mark <markId>
  ```
- Manager 的 Apply 协调 tc + iptables 下发；Disable 两者一起拆
- `-C/-D` 循环保证 iptables 规则幂等（应对 agent 重启后的残留）

## 副作用 / 意外好处

1. **xray 配置改造面积更小**：原设计要动 inbounds + outbounds + routing 三处，现在只动 inbounds
2. **打标逻辑与 xray 解耦**：iptables 工作在内核包层，将来换 v2ray / sing-box / 其它代理也能直接复用
3. **运维更直观**：`iptables -t mangle -L OUTPUT -v -n` 直接能看到每 tier 的打标计数，无需深入 xray 内部

## 风险 / 未验证事项

- **集成场景下源端口被 NAT 改写**：如果 VPS 外面还有一层 NAT（云商 IP 转换），用户看到的源端口和 iptables OUTPUT 看到的源端口可能不一致。目前假设内核的 OUTPUT 链观察到的就是 xray 进程发出时的本机源端口，不受后续 NAT 影响（因为 OUTPUT chain 在 NAT POSTROUTING 之前）。✅ 实际 Linux netfilter 流水线确实如此，无风险。
- **`iptables-legacy` vs `iptables-nft`**：Ubuntu 22.04 默认 legacy，本 PoC 在此环境验证通过。现网节点如使用 nft 需二次验证（通常接口兼容）。
- **agent 重启时的残留清理**：Manager.ensureIptablesMark 用 `-C/-D` 循环清干净后再 `-A`，保证 exactly-one。`TestManager_EnsureIptables_CleansLeftover` 单测覆盖。

## 原始测试命令

测试 C 的完整复现（在测试机上）：

```bash
# 建 tc 分类树
IFACE=eth1
tc qdisc del dev $IFACE root 2>/dev/null
tc qdisc add dev $IFACE root handle 1: htb default 999
tc class add dev $IFACE parent 1: classid 1:10 htb rate 10mbit ceil 10mbit
tc class add dev $IFACE parent 1: classid 1:999 htb rate 10gbit
tc qdisc add dev $IFACE parent 1:10 handle 10: fq_codel
tc filter add dev $IFACE protocol ip parent 1: prio 1 handle 1 fw flowid 1:10

# iptables 按源端口打 mark
iptables -t mangle -A OUTPUT -p tcp --sport 8080 -j MARK --set-mark 1

# 起测试文件服务器
mkdir -p /tmp/srv && dd if=/dev/zero bs=1M count=30 > /tmp/srv/big.bin
cd /tmp/srv && python3 -m http.server 8080 --bind 0.0.0.0 &

# 从远端客户端（Mac）下载
# mac$ curl -o /dev/null http://149.104.78.146:8080/big.bin

# 观察结果
tc -s class show dev $IFACE classid 1:10
iptables -t mangle -L OUTPUT -v -n -x
```

观察到 tc 1:10 `Sent 7146836 bytes 4755 pkt (overlimits 2420)`，iptables rule 计数 `2450 pkts 6960406 bytes`。
