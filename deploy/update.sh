#!/usr/bin/env bash
# Parallax update for server, agent-only, or combined systemd installations.
# Run as root. Services briefly restart; failed restarts restore the old release.
set -Eeuo pipefail
umask 022

APP_DIR="${APP_DIR:-/opt/looking-glass}"
BRANCH="${BRANCH:-main}"
case "$APP_DIR" in /*) ;; *) echo "ERROR: APP_DIR must be an absolute path." >&2; exit 1 ;; esac
export PATH="/usr/local/go/bin:/usr/local/bin:$PATH"
# Refuse legacy service-writable code before invoking root Git hooks/builds.
# A trusted fresh clone is required for migration; chown alone cannot make
# source already writable by the service account trustworthy again.
TRUST_PATH=$APP_DIR
while :; do
  [ ! -L "$TRUST_PATH" ] || { echo "ERROR: Symlinked installation ancestor." >&2; exit 1; }
  UNSAFE=$(find "$TRUST_PATH" -maxdepth 0 \( ! -user root -o -perm /022 \) -print)
  [ -z "$UNSAFE" ] || { echo "ERROR: Installation ancestor is service-writable; see docs/deployment.md." >&2; exit 1; }
  [ "$TRUST_PATH" != / ] || break
  TRUST_PATH=$(dirname -- "$TRUST_PATH")
done
for name in repo frontend update.sh .env .deploy-key-path .update.lock looking-glass-server looking-glass-agent; do
  path="$APP_DIR/$name"
  [ ! -L "$path" ] || { echo "ERROR: Symlinked installation entry." >&2; exit 1; }
  [ -e "$path" ] || continue
  UNSAFE=$(find "$path" -xdev \( ! -user root -o \( ! -type l -a -perm /022 \) \) -print -quit)
  [ -z "$UNSAFE" ] || { echo "ERROR: Installation code is service-writable; see docs/deployment.md." >&2; exit 1; }
  while IFS= read -r -d '' link; do
    target=$(readlink -f -- "$link")
    case "$target" in "$path"/*) ;; *) echo "ERROR: Installation symlink escapes trusted code." >&2; exit 1 ;; esac
  done < <(find "$path" -type l -print0)
done
if [ ! -d "$APP_DIR/repo/.git" ] || [ -L "$APP_DIR/repo/.git" ]; then
  echo "ERROR: Updates require a standalone trusted clone, not external Git metadata." >&2
  exit 1
fi
# Serialize builds and swaps on a host. A second updater must not undo the first.
exec 9>"$APP_DIR/.update.lock"
flock -n 9 || { echo "ERROR: Another update is running." >&2; exit 1; }

# Detect installed units, including ones that are stopped or disabled.
SERVER=false
AGENT=false
if [ "$(systemctl show -p LoadState --value looking-glass-server.service)" = loaded ]; then
  SERVER=true
fi
if [ "$(systemctl show -p LoadState --value looking-glass-agent.service)" = loaded ]; then
  AGENT=true
fi
if ! "$SERVER" && ! "$AGENT"; then
  echo "ERROR: No Parallax systemd service is installed." >&2
  exit 1
fi

GO_CURRENT=$(go version | awk '{sub(/^go/, "", $3); print $3}')
if [[ "$GO_CURRENT" != 1.27.* ]] || [ "$(printf '%s\n' 1.27.1 "$GO_CURRENT" | sort -V | head -1)" != 1.27.1 ]; then
  echo "ERROR: Install the supported Go 1.27.1+ toolchain before updating." >&2
  exit 1
fi
if "$SERVER"; then
  NODE_CURRENT=$(node -p 'process.versions.node')
  if [[ "$NODE_CURRENT" != 24.* ]] || [ "$(printf '%s\n' 24.21.0 "$NODE_CURRENT" | sort -V | head -1)" != 24.21.0 ]; then
    echo "ERROR: Install supported Node.js 24.21+ before updating." >&2
    exit 1
  fi
fi

cd "$APP_DIR/repo"
# Trust this explicit installed clone, without changing global Git settings.
git() { command git -c "safe.directory=$APP_DIR/repo" "$@"; }
if [ -n "$(git status --porcelain)" ]; then
  echo "ERROR: Clone has local changes; preserve them before updating." >&2
  exit 1
fi
ORIGIN=${REPO_URL:-$(git remote get-url origin)}
# HTTPS clones do not need a key, even if a retired key-path file remains.
if [[ "$ORIGIN" = git@* || "$ORIGIN" = ssh://* ]]; then
  DEPLOY_KEY_PATH=${DEPLOY_KEY:-}
  if [ -z "$DEPLOY_KEY_PATH" ] && [ -f "$APP_DIR/.deploy-key-path" ]; then
    DEPLOY_KEY_PATH=$(cat "$APP_DIR/.deploy-key-path")
  fi
  if [ -n "$DEPLOY_KEY_PATH" ]; then
    [ -r "$DEPLOY_KEY_PATH" ] || { echo "ERROR: Saved deploy key is not readable." >&2; exit 1; }
    printf -v GIT_SSH_COMMAND 'ssh -i %q -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=accept-new' "$DEPLOY_KEY_PATH"
    export GIT_SSH_COMMAND
  fi
fi
export GIT_TERMINAL_PROMPT=0
git remote set-url origin "$ORIGIN"
git fetch origin "$BRANCH"
# Refuse divergence instead of silently deleting local commits/files.
if ! git merge-base --is-ancestor HEAD FETCH_HEAD; then
  echo "ERROR: Clone has commits absent from the requested remote branch; preserve them before updating." >&2
  exit 1
fi
git merge --ff-only FETCH_HEAD
VERSION=$(git describe --tags --always --dirty)

# Finish every build before touching running artifacts.
STAGE=$(mktemp -d "$APP_DIR/.update.XXXXXX")
trap 'rm -rf "$STAGE"' EXIT
if "$SERVER"; then
  (cd frontend && npm ci && npm run build)
  cp -a frontend/dist "$STAGE/dist"
  (cd backend && CGO_ENABLED=0 go build -trimpath -o "$STAGE/looking-glass-server" .)
fi
if "$AGENT"; then
  (cd agent && CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$VERSION" -o "$STAGE/looking-glass-agent" .)
fi

BACKUP=$(mktemp -d "$APP_DIR/rollback.XXXXXXXX")
chmod 700 "$BACKUP"
SERVICES=()
ACTIVE_SERVICES=()
if "$SERVER"; then SERVICES+=(looking-glass-server); fi
if "$AGENT"; then SERVICES+=(looking-glass-agent); fi
for service in "${SERVICES[@]}"; do
  if systemctl is-active --quiet "$service"; then ACTIVE_SERVICES+=("$service"); fi
  cp -a "$APP_DIR/$service" "$BACKUP/$service"
  chown root:root "$STAGE/$service"
  chmod 755 "$STAGE/$service"
done
if "$SERVER"; then
  cp -a "$APP_DIR/frontend/dist" "$BACKUP/dist"
  chown -R root:root "$STAGE/dist"
fi

rollback() {
  local result=${1:-$?}
  trap - ERR INT TERM HUP
  set +e
  echo "ERROR: Update failed; restoring artifacts from $BACKUP" >&2
  for service in "${SERVICES[@]}"; do
    cp -a "$BACKUP/$service" "$APP_DIR/$service.restore"
    mv -f "$APP_DIR/$service.restore" "$APP_DIR/$service"
  done
  if "$SERVER"; then
    rm -rf "$APP_DIR/frontend/dist"
    cp -a "$BACKUP/dist" "$APP_DIR/frontend/dist"
  fi
  for service in ${ACTIVE_SERVICES[@]+"${ACTIVE_SERVICES[@]}"}; do systemctl restart "$service" || true; done
  exit "$result"
}
trap 'rollback "$?"' ERR
trap 'rollback 130' INT
trap 'rollback 143' TERM
trap 'rollback 129' HUP
for service in "${SERVICES[@]}"; do
  mv -f "$STAGE/$service" "$APP_DIR/$service"
done
if "$SERVER"; then
  mv "$APP_DIR/frontend/dist" "$STAGE/previous-dist"
  mv "$STAGE/dist" "$APP_DIR/frontend/dist"
fi
for service in ${ACTIVE_SERVICES[@]+"${ACTIVE_SERVICES[@]}"}; do
  systemctl restart "$service"
  sleep 3
  systemctl is-active --quiet "$service"
done
# Refresh the standalone updater only after successful artifact activation.
install -m 755 "$APP_DIR/repo/deploy/update.sh" "$APP_DIR/update.sh.new"
mv -f "$APP_DIR/update.sh.new" "$APP_DIR/update.sh"
trap - ERR INT TERM HUP

echo "Updated to $VERSION. Previous artifacts: $BACKUP"
for service in "${SERVICES[@]}"; do
  systemctl show "$service" -p ActiveState -p SubState
done
