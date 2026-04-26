#!/usr/bin/env bash
#
# xray_audit.sh — xray 节点集群健康审计。
#
# ⚠️ 仅适用于 protocol=xray 节点。检查 12 项：agent/xray 服务存活、单 inbound +
# tag=proxy、proc env 实际加载的 tag 跟 config 一致、systemd drop-in 残留、
# reality 配置（shortIds/dest/port range）跟 reality.env 一致、port 在 iptables
# ACCEPT 范围、NODE_HOST 一致、2.2.0+ 节点 8080 在 listening、磁盘 > 200MB。
#
# ikev2 节点不要扫——它们没 xray、没 reality.env、没 8080，所有检查都会报
# CONFIG_PARSE_ERR。--db 模式已自动过滤 protocol=xray，手动传 IP 时自己注意。
#
# Usage:
#   scripts/xray_audit.sh                       # read IPs from stdin
#   scripts/xray_audit.sh -f nodes.txt          # read from file
#   scripts/xray_audit.sh --db                  # 从 backend DB 拉 active xray 节点
#   scripts/xray_audit.sh ip1 ip2 ...           # explicit IPs
#   scripts/xray_audit.sh --db -p 30 -o /tmp/a.csv
#
# Env (override defaults):
#   AUDIT_PARALLEL    - SSH parallelism (default 20)
#   AUDIT_TIMEOUT     - SSH connect timeout sec (default 15)
#   AUDIT_BACKEND     - SSH target for DB query (default ubuntu@54.46.43.200)
#   AUDIT_DB_*        - DB host/user/pass/name for --db mode

set -euo pipefail

PARALLEL="${AUDIT_PARALLEL:-20}"
TIMEOUT="${AUDIT_TIMEOUT:-15}"
BACKEND="${AUDIT_BACKEND:-ubuntu@54.46.43.200}"
DB_HOST="${AUDIT_DB_HOST:-db-mountain.ck0ibgs3yv3x.ap-east-1.rds.amazonaws.com}"
DB_USER="${AUDIT_DB_USER:-mountain}"
DB_PASS="${AUDIT_DB_PASS:-Mooc19882020}"
DB_NAME="${AUDIT_DB_NAME:-db_midnight_pro}"
OUTPUT="/tmp/audit_$(date +%Y%m%d_%H%M%S).csv"

INPUT_MODE=stdin
INPUT_FILE=""
declare -a INPUT_IPS=()

while [[ $# -gt 0 ]]; do
  case "$1" in
    -f)         INPUT_MODE=file; INPUT_FILE="$2"; shift 2 ;;
    --db)       INPUT_MODE=db; shift ;;
    -p|--parallel) PARALLEL="$2"; shift 2 ;;
    -o|--output)   OUTPUT="$2"; shift 2 ;;
    -h|--help)  sed -n '2,18p' "$0"; exit 0 ;;
    -*)         echo "unknown option: $1" >&2; exit 1 ;;
    *)          INPUT_MODE=args; INPUT_IPS+=("$1"); shift ;;
  esac
done

SSH_OPTS=(-o ConnectTimeout=$TIMEOUT -o UserKnownHostsFile=/dev/null
          -o StrictHostKeyChecking=no -o BatchMode=yes -o ServerAliveInterval=3)

get_nodes() {
  case "$INPUT_MODE" in
    file)  cat "$INPUT_FILE" ;;
    args)  printf '%s\n' "${INPUT_IPS[@]}" ;;
    stdin) cat ;;
    db)    ssh "${SSH_OPTS[@]}" "$BACKEND" "PGPASSWORD='$DB_PASS' psql -h '$DB_HOST' -U '$DB_USER' -d '$DB_NAME' -tAc \"SELECT host FROM tb_node WHERE protocol='xray' AND create_status='complete' AND enable_status>0 ORDER BY host\"" ;;
  esac
}

audit_one() {
  local ip="$1"
  local out
  out=$(ssh "${SSH_OPTS[@]}" root@"$ip" bash -s 2>/dev/null <<'REMOTE'
agent_active=$(systemctl is-active node-agent 2>/dev/null)
xray_active=$(systemctl is-active xray 2>/dev/null)
pid=$(pgrep -x node-agent 2>/dev/null | head -1)
proc_tag=""; proc_host=""
if [ -n "$pid" ]; then
  proc_tag=$(tr '\0' '\n' < /proc/$pid/environ 2>/dev/null | grep ^XRAY_INBOUND_TAG= | cut -d= -f2)
  proc_host=$(tr '\0' '\n' < /proc/$pid/environ 2>/dev/null | grep ^NODE_HOST= | cut -d= -f2)
fi
env_tag=$(grep ^XRAY_INBOUND_TAG /etc/node-agent.service.env 2>/dev/null | cut -d= -f2)
dropin_tag=""
ls /etc/systemd/system/node-agent.service.d/*.conf 2>/dev/null >/dev/null && \
  dropin_tag=$(grep -h XRAY_INBOUND_TAG /etc/systemd/system/node-agent.service.d/*.conf 2>/dev/null | sed 's/.*XRAY_INBOUND_TAG=//' | tail -1)
agent_ver=$(/usr/local/bin/node-agent --version 2>/dev/null)
disk_mb=$(df -BM / 2>/dev/null | awk 'NR==2{gsub("M","",$4); print $4}')
http8080=$(ss -tln 2>/dev/null | awk '$4 ~ /:8080$/' | wc -l)
ufw_status=$(ufw status 2>/dev/null | head -1 | awk '{print $2}')
config=$(python3 - <<'PY' 2>/dev/null
import json
try:
  c = json.load(open('/etc/xray/config.json'))
  ibs = c.get('inbounds', [])
  tags = [ib.get('tag','') for ib in ibs]
  ports = [str(ib.get('port','')) for ib in ibs]
  rs = ibs[0].get('streamSettings',{}).get('realitySettings',{}) if ibs else {}
  si = rs.get('shortIds', []); de = rs.get('dest', '')
  cl = sum(len(ib.get('settings',{}).get('clients',[])) for ib in ibs)
  print(f"{len(ibs)}~{','.join(tags)}~{';'.join(ports)}~{','.join(si)}~{de}~{cl}")
except: print("0~ERR~ERR~ERR~ERR~0")
PY
)
re_si=$(grep ^REALITY_SHORT_IDS /etc/xray/reality.env 2>/dev/null | cut -d= -f2)
re_dest=$(grep ^REALITY_DEST /etc/xray/reality.env 2>/dev/null | cut -d= -f2)
re_port=$(grep ^REALITY_PORT_RANGE /etc/xray/reality.env 2>/dev/null | cut -d= -f2)
ipt=$(iptables -S INPUT 2>/dev/null | grep -- '-j ACCEPT' | grep -- '-p tcp' | grep -oE -- '--dports? [0-9:]+' | tr '\n' ';')
echo "${agent_active}|${xray_active}|${agent_ver}|${pid}|${proc_tag}|${env_tag}|${dropin_tag}|${proc_host}|${disk_mb}|${http8080}|${ufw_status}|${config}|${re_si}|${re_dest}|${re_port}|${ipt}"
REMOTE
)
  if [ -z "$out" ]; then echo "$ip|UNREACHABLE"; else echo "$ip|REACH|$out"; fi
}
export -f audit_one
export SSH_OPTS

NODES_FILE=$(mktemp)
trap 'rm -f "$NODES_FILE"' EXIT

echo ">> source: $INPUT_MODE"
get_nodes > "$NODES_FILE"
N=$(wc -l < "$NODES_FILE" | tr -d ' ')
echo ">> $N nodes  parallel=$PARALLEL  output=$OUTPUT"
[ "$N" -eq 0 ] && { echo "no nodes to audit"; exit 1; }

# xargs -I doesn't work directly with bash exported funcs on macOS; use bash -c
cat "$NODES_FILE" | xargs -P "$PARALLEL" -n1 bash -c 'audit_one "$0"' > "$OUTPUT" 2>&1
echo ">> done. classifying..."

python3 - "$OUTPUT" <<'PY'
import sys, re
from collections import Counter, defaultdict

def parse_port(s):
    s = s.strip()
    if not s: return None
    if '-' in s:
        try: a,b = s.split('-',1); return (int(a), int(b))
        except: return None
    try: n = int(s); return (n, n)
    except: return None

def parse_dports(s):
    return [(int(m.group(1)), int(m.group(2)) if m.group(2) else int(m.group(1)))
            for m in re.finditer(r'--dports?\s+(\d+)(?::(\d+))?', s)]

def covered(rng, ranges):
    if rng is None: return False
    if not ranges: return True   # no specific ACCEPT rules ~ INPUT default ACCEPT
    a, b = rng
    return any(ra <= a and b <= rb for ra, rb in ranges)

verdicts = Counter()
issues = defaultdict(list)

with open(sys.argv[1]) as fh:
    for line in fh:
        parts = line.rstrip('\n').split('|')
        ip = parts[0]
        if len(parts) < 3 or parts[1] == 'UNREACHABLE':
            verdicts['UNREACHABLE'] += 1
            issues['UNREACHABLE'].append(ip); continue
        if len(parts) < 18:
            verdicts['MALFORMED'] += 1
            issues['MALFORMED'].append((ip, line.rstrip())); continue
        (agent_a, xray_a, ver, pid, ptag, etag, dtag, phost,
         disk, http, ufw, config, re_si, re_dest, re_port, ipt) = parts[2:18]
        c = config.split('~')
        if len(c) < 6:
            verdicts['CONFIG_PARSE_ERR'] += 1; issues['CONFIG_PARSE_ERR'].append(ip); continue
        ic = int(c[0]) if c[0].isdigit() else 0
        ctag, cport, csi, cdest = c[1], c[2].split(';')[0], c[3], c[4]

        problems = []
        if agent_a != 'active': problems.append(f'agent_inactive:{agent_a}')
        if xray_a != 'active':  problems.append(f'xray_inactive:{xray_a}')
        if ic != 1:             problems.append(f'multi_inbound:{ic}')
        if ctag != 'proxy':     problems.append(f'non_proxy_tag:{ctag}')
        eff = ptag or 'proxy'
        if eff != ctag:         problems.append(f'tag_mismatch:eff={eff},cfg={ctag}')
        if dtag and dtag != ctag: problems.append(f'dropin_residue:{dtag}')
        if csi != re_si:        problems.append('reality_si_drift')
        if cdest != re_dest:    problems.append('reality_dest_drift')
        if cport != re_port:    problems.append(f'port_realityenv:{cport}!={re_port}')
        if not covered(parse_port(cport), parse_dports(ipt)):
            problems.append('port_not_in_iptables')
        if phost and phost != ip: problems.append(f'node_host:{phost}')
        if ver == '2.2.0-xray' and http != '1': problems.append('http8080_missing')
        try:
            if int(disk) < 200: problems.append(f'low_disk:{disk}MB')
        except: pass

        if not problems:
            verdicts[f'OK_{ver}'] += 1
        else:
            cats = ','.join(sorted({p.split(':')[0] for p in problems}))
            v = f'WARN:{cats}'
            verdicts[v] += 1
            issues[v].append((ip, problems))

print()
print('=== SUMMARY ===')
total = sum(verdicts.values())
for k, v in sorted(verdicts.items(), key=lambda x: -x[1]):
    print(f'{v:5d}  {k}')
print(f'{total:5d}  TOTAL')

if any(not k.startswith('OK_') for k in issues):
    print()
    print('=== DETAILS ===')
    for k, lst in sorted(issues.items()):
        print(f'\n--- {k} ({len(lst)}) ---')
        for entry in lst[:25]:
            if isinstance(entry, tuple):
                print(f'  {entry[0]}:')
                for p in entry[1]: print(f'      {p}')
            else:
                print(f'  {entry}')
        if len(lst) > 25:
            print(f'  ... +{len(lst)-25} more')
PY

echo
echo ">> raw CSV: $OUTPUT"
