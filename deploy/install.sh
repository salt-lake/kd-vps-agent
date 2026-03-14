#!/usr/bin/env bash
# install.sh — 在节点上安装或更新 node-agent
# 用法：bash install.sh [version]
# 若不指定 version，自动拉取 GitHub 最新 Release
set -euo pipefail

REPO="salt-lake/kd-vps-agent"
INSTALL_PATH="/usr/local/bin/node-agent"
SERVICE_NAME="node-agent"
ENV_DIR="/etc/node-agent"

log(){ echo "[$(date '+%F %T')] $*"; }

# 获取最新版本 tag
latest_version(){
  curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    -H "Accept: application/vnd.github+json" \
    | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/'
}

VERSION="${1:-$(latest_version)}"
log "installing node-agent ${VERSION}"

# 下载二进制
TMP=$(mktemp)
curl -fsSL "https://github.com/${REPO}/releases/download/${VERSION}/node-agent" -o "$TMP"
chmod 755 "$TMP"
mv "$TMP" "$INSTALL_PATH"
log "binary installed to $INSTALL_PATH"

# 安装 systemd service（仅首次）
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ ! -f "/etc/systemd/system/${SERVICE_NAME}.service" ]; then
  cp "$SCRIPT_DIR/node-agent.service" "/etc/systemd/system/${SERVICE_NAME}.service"
  systemctl daemon-reload
  log "systemd service installed"
fi

# 创建 env 文件模板（仅首次）
if [ ! -f "${ENV_DIR}/env" ]; then
  mkdir -p "$ENV_DIR"
  cat > "${ENV_DIR}/env" <<'EOF'
NODE_HOST=
NATS_URL=nats://127.0.0.1:4222
NATS_AUTH_TOKEN=
NODE_PROTOCOL=ikev2
API_BASE=
SCRIPT_TOKEN=
EOF
  chmod 600 "${ENV_DIR}/env"
  log "env template created at ${ENV_DIR}/env — fill in required values"
fi

systemctl enable "$SERVICE_NAME"
systemctl restart "$SERVICE_NAME"
log "node-agent ${VERSION} started"
