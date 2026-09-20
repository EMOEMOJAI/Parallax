#!/usr/bin/env bash
# Install or update the Parallax server on Debian/Ubuntu.
# Public HTTPS needs no key; private forks may set REPO_URL and DEPLOY_KEY.
set -Eeuo pipefail
umask 022
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/common.sh
source "$SCRIPT_DIR/common.sh"
require_root
APP_DIR=/opt/looking-glass
BRANCH=${BRANCH:-main}
apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y git curl openssh-client ca-certificates python3 xz-utils
lock_install
ensure_go
ensure_node
prepare_user
prepare_repo
(cd "$APP_DIR/repo/frontend" && npm ci && npm run build)
(cd "$APP_DIR/repo/backend" && CGO_ENABLED=0 go build -trimpath -o "$APP_DIR/looking-glass-server.new" .)
begin_activation looking-glass-server
chown root:root "$APP_DIR/looking-glass-server.new"
chmod 755 "$APP_DIR/looking-glass-server.new"
install -d -o lookingglass -g lookingglass -m 750 "$APP_DIR/data"
install -d -o root -g root -m 755 "$APP_DIR/frontend"
# Preserve the latest legacy data, including an explicit old .env override.
if ! ENV_MIGRATED=$(migrate_legacy_schedule_env); then rollback_install 1; fi
if [ -f "$APP_DIR/schedules.json" ] && { [ ! -f "$APP_DIR/data/schedules.json" ] || [ "$ENV_MIGRATED" = migrated ]; }; then
  if [ "$(systemctl show -p LoadState --value looking-glass-server.service)" = loaded ]; then
    systemctl stop looking-glass-server
  fi
  cp -a "$APP_DIR/schedules.json" "$APP_DIR/data/schedules.json"
  chown lookingglass:lookingglass "$APP_DIR/data/schedules.json"
  chmod 600 "$APP_DIR/data/schedules.json"
fi
mv -f "$APP_DIR/looking-glass-server.new" "$APP_DIR/looking-glass-server"
# Retain the prior frontend for manual rollback on an existing installation.
if [ -d "$APP_DIR/frontend/dist" ]; then
  mv "$APP_DIR/frontend/dist" "$APP_DIR/frontend/dist.previous.$(date +%s)"
fi
cp -a "$APP_DIR/repo/frontend/dist" "$APP_DIR/frontend/dist"
chown -R root:root "$APP_DIR/frontend/dist"
if [ ! -f "$APP_DIR/.env" ]; then
  cat > "$APP_DIR/.env" << 'ENVEOF'
# Looking Glass environment variables
# AGENT_API_KEY=changeme
# CLIENT_API_KEY=changeme
# ALLOWED_ORIGINS=https://yourdomain.com
# Accept only browser origins whose host matches the request Host. Use this
# when you don't want to hardcode ALLOWED_ORIGINS; it closes cross-site
# WebSocket access. Leave both unset only on a trusted LAN.
# STRICT_ORIGIN=1
# Set when a reverse proxy (traefik, nginx, Caddy) sits in front, so per-IP
# rate limits key on the real visitor instead of the proxy address. Only set
# it if the proxy always overwrites X-Forwarded-For. TRUSTED_PROXY_HOPS=1.
# TRUST_PROXY=1
#
# Require a token on /metrics. When set, scrapers must send it as
# Authorization: Bearer <token> (or ?key=) and the output is complete. When
# left unset /metrics stays open, but the per-schedule probe gauges — schedule
# ids, node names, commands and probe targets — are withheld from every scrape,
# credentialed or not: CLIENT_API_KEY does not unlock them. Process-level
# metrics are always served.
# METRICS_TOKEN=changeme
#
# Optional webhook for scheduled-probe status changes. Treat it as a secret:
# it is never logged beyond scheme+host and never stored in schedules.json.
# Alerts fire after two consecutive runs at the new status, at most once every
# 5 minutes per schedule, and never for the first run after a restart.
# ALERT_WEBHOOK_URL=https://hooks.example.com/services/XXXX
#
# Reverse proxy logging: browsers send the client key as the "lg.bearer"
# WebSocket subprotocol, but CLI clients still use ?key=<key> on /ws/*.
# Configure the proxy to strip query strings from /ws/* access-log lines
# (nginx: log $uri instead of $request; traefik: disable accesslog fields
# RequestPath or filter it) so client keys never land in the log files.
ENVEOF
fi
chown root:lookingglass "$APP_DIR/.env"
chmod 640 "$APP_DIR/.env"
cat > /etc/systemd/system/looking-glass-server.service <<'UNIT'
[Unit]
Description=Parallax Server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=lookingglass
WorkingDirectory=/opt/looking-glass
ExecStart=/opt/looking-glass/looking-glass-server
Environment=PORT=8080
Environment=SCHEDULES_FILE=/opt/looking-glass/data/schedules.json
EnvironmentFile=-/opt/looking-glass/.env
NoNewPrivileges=true
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT
install -o root -g root -m 755 "$APP_DIR/repo/deploy/update.sh" "$APP_DIR/update.sh"
systemctl daemon-reload
systemctl enable looking-glass-server
systemctl restart looking-glass-server
sleep 3
systemctl is-active --quiet looking-glass-server
systemctl --no-pager --lines=0 status looking-glass-server
finish_activation
printf '%s\n' 'Server installed. Configure .env before exposing the service beyond a trusted network.'
