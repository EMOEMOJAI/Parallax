#!/usr/bin/env bash
# Install or update an agent on Debian/Ubuntu. Public HTTPS by default;
# private forks may set REPO_URL and DEPLOY_KEY (a read-only deploy key).
set -Eeuo pipefail
umask 022
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/common.sh
source "$SCRIPT_DIR/common.sh"
require_root
APP_DIR=/opt/looking-glass
BRANCH=${BRANCH:-main}
SERVER_URL=${1:?Usage: $0 <server-ws-url> <node-name> <location> [flag] [provider]}
NODE_NAME=${2:?Missing node name}
LOCATION=${3:?Missing location}
FLAG=${4:-🏳️}
PROVIDER=${5:-Unknown}
case "$SERVER_URL" in ws://*|wss://*) ;; *) fail 'Server URL must use ws:// or wss://.' ;; esac
# Validate before making any host changes; values are quoted again below.
for value in "$SERVER_URL" "$NODE_NAME" "$LOCATION" "$FLAG" "$PROVIDER"; do systemd_arg "$value" >/dev/null; done
apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y iputils-ping traceroute mtr-tiny iperf3 dnsutils curl ca-certificates git openssh-client python3
lock_install
ensure_go
prepare_user
prepare_repo
(cd "$APP_DIR/repo/agent" && CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$(git -c "safe.directory=$APP_DIR/repo" -C "$APP_DIR/repo" describe --tags --always --dirty)" -o "$APP_DIR/looking-glass-agent.new" .)
begin_activation looking-glass-agent
chown root:root "$APP_DIR/looking-glass-agent.new"
chmod 755 "$APP_DIR/looking-glass-agent.new"
mv -f "$APP_DIR/looking-glass-agent.new" "$APP_DIR/looking-glass-agent"
if [ ! -f "$APP_DIR/.env" ]; then
  cat > "$APP_DIR/.env" <<'ENVEOF'
# AGENT_API_KEY=changeme
# PROBE_ALLOW_PRIVATE=1 (only for intentionally probing private addresses)
ENVEOF
fi
chown root:lookingglass "$APP_DIR/.env"
chmod 640 "$APP_DIR/.env"
cat > /etc/systemd/system/looking-glass-agent.service <<UNIT
[Unit]
Description=Parallax Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=lookingglass
WorkingDirectory=$APP_DIR
ExecStart=$APP_DIR/looking-glass-agent -server $(systemd_arg "$SERVER_URL") -name $(systemd_arg "$NODE_NAME") -location $(systemd_arg "$LOCATION") -flag $(systemd_arg "$FLAG") -provider $(systemd_arg "$PROVIDER")
AmbientCapabilities=CAP_NET_RAW
NoNewPrivileges=true
Restart=always
RestartSec=5
EnvironmentFile=-$APP_DIR/.env

[Install]
WantedBy=multi-user.target
UNIT
install -o root -g root -m 755 "$APP_DIR/repo/deploy/update.sh" "$APP_DIR/update.sh"
systemctl daemon-reload
systemctl enable looking-glass-agent
systemctl restart looking-glass-agent
sleep 3
systemctl is-active --quiet looking-glass-agent
systemctl --no-pager --lines=0 status looking-glass-agent
finish_activation
