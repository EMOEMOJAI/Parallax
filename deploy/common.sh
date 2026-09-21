#!/usr/bin/env bash
# Shared helpers for the Debian/Ubuntu installers. Source; do not execute.

fail() { echo "ERROR: $*" >&2; exit 1; }
require_root() { [ "${EUID:-$(id -u)}" -eq 0 ] || fail 'Run this installer with sudo.'; }

# Quote a single systemd ExecStart argument. Reject control characters so a
# label can never add a second unit directive, and escape both expansions.
systemd_arg() {
  local value=$1
  if [[ "$value" = *$'\n'* ]] || printf '%s' "$value" | LC_ALL=C grep -q '[[:cntrl:]]'; then
    fail 'Service arguments must not contain control characters.'
  fi
  value=${value//\\/\\\\}
  value=${value//\"/\\\"}
  value=${value//%/%%}
  value=${value//\$/\$\$}
  printf '"%s"' "$value"
}

ensure_go() {
  export PATH="/usr/local/go/bin:/usr/local/bin:$PATH"
  local current arch archive temp expected
  current=$(go version 2>/dev/null | awk '{sub(/^go/, "", $3); print $3}') || current=0
  if [[ "$current" = 1.27.* ]] && [ "$(printf '%s\n' 1.27.1 "$current" | sort -V | head -1)" = 1.27.1 ]; then return; fi
  case "$(uname -m)" in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) fail 'Install Go 1.27.1+ for this architecture first.' ;; esac
  archive="go1.27.1.linux-${arch}.tar.gz"
  temp=$(mktemp -d)
  # The default feed drops older patches when a new release ships. Look up
  # the pinned archive in the complete history so this installer stays usable.
  curl -fsSL --retry 3 'https://go.dev/dl/?mode=json&include=all' -o "$temp/releases.json"
  expected=$(python3 - "$temp/releases.json" "$archive" <<'PY'
import json, sys
for release in json.load(open(sys.argv[1])):
    for artifact in release['files']:
        if artifact['filename'] == sys.argv[2]:
            print(artifact['sha256'])
            sys.exit(0)
raise SystemExit('Pinned Go release not found; update installer or install Go 1.27.1+ first')
PY
  )
  curl -fsSL --retry 3 "https://go.dev/dl/$archive" -o "$temp/$archive"
  (cd "$temp" && printf '%s  %s\n' "$expected" "$archive" | sha256sum -c -)
  local staged
  staged=$(mktemp -d /usr/local/.parallax-go.XXXXXX)
  tar -xzf "$temp/$archive" -C "$staged"
  if [ -e /usr/local/go ]; then mv /usr/local/go "/usr/local/go.previous.$(date +%s)"; fi
  mv "$staged/go" /usr/local/go
  rmdir "$staged"
  rm -rf "$temp"
}

ensure_node() {
  local current arch archive temp
  current=$(node -p 'process.versions.node' 2>/dev/null) || current=0
  if [[ "$current" = 24.* ]] && [ "$(printf '%s\n' 24.21.0 "$current" | sort -V | head -1)" = 24.21.0 ]; then return; fi
  case "$(uname -m)" in x86_64) arch=x64 ;; aarch64|arm64) arch=arm64 ;; *) fail 'Install Node.js 24.21+ for this architecture first.' ;; esac
  archive="node-v24.21.0-linux-${arch}.tar.xz"
  temp=$(mktemp -d)
  curl -fsSL --retry 3 "https://nodejs.org/dist/v24.21.0/$archive" -o "$temp/$archive"
  curl -fsSL --retry 3 https://nodejs.org/dist/v24.21.0/SHASUMS256.txt -o "$temp/SHASUMS256.txt"
  (cd "$temp" && grep "  $archive\$" SHASUMS256.txt | sha256sum -c -)
  tar -xJf "$temp/$archive" -C /usr/local --strip-components=1
  rm -rf "$temp"
  hash -r
}

prepare_repo() {
  local key=${DEPLOY_KEY:-} origin=${REPO_URL:-}
  if [ -z "$origin" ] && [ -d "$APP_DIR/repo/.git" ]; then
    origin=$(git -c "safe.directory=$APP_DIR/repo" -C "$APP_DIR/repo" remote get-url origin)
  fi
  origin=${origin:-https://github.com/EMOEMOJAI/Parallax.git}
  # A key is optional. Public repositories clone over HTTPS with no account.
  if [ -z "$key" ] && [[ "$origin" = git@* || "$origin" = ssh://* ]] && [ -f "$APP_DIR/.deploy-key-path" ]; then
    key=$(cat "$APP_DIR/.deploy-key-path")
  fi
  if [ -n "$key" ]; then
    [ -r "$key" ] || fail "Deploy key is not readable: $key"
    case "$origin" in https://github.com/*) origin="git@github.com:${origin#https://github.com/}" ;; esac
    printf -v GIT_SSH_COMMAND 'ssh -i %q -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=accept-new' "$key"
    export GIT_SSH_COMMAND
  fi
  export GIT_TERMINAL_PROMPT=0
  if [ -d "$APP_DIR/repo/.git" ]; then
    [ -z "$(git -c "safe.directory=$APP_DIR/repo" -C "$APP_DIR/repo" status --porcelain)" ] || fail 'Clone has local changes; preserve them before reinstalling.'
    git -c "safe.directory=$APP_DIR/repo" -C "$APP_DIR/repo" remote set-url origin "$origin"
    git -c "safe.directory=$APP_DIR/repo" -C "$APP_DIR/repo" fetch origin "$BRANCH"
    git -c "safe.directory=$APP_DIR/repo" -C "$APP_DIR/repo" merge-base --is-ancestor HEAD FETCH_HEAD || fail 'Clone has commits outside the requested remote branch; preserve them before reinstalling.'
    git -c "safe.directory=$APP_DIR/repo" -C "$APP_DIR/repo" merge --ff-only FETCH_HEAD
  else
    git clone --branch "$BRANCH" -- "$origin" "$APP_DIR/repo"
  fi
  if [ -n "$key" ]; then
    printf '%s\n' "$key" > "$APP_DIR/.deploy-key-path"
    chmod 600 "$APP_DIR/.deploy-key-path"
  fi
  # Service accounts must not be able to change code later executed by root.
  chown -R root:root "$APP_DIR/repo"
  chmod -R go-w "$APP_DIR/repo"
}

prepare_user() {
  if ! id lookingglass >/dev/null 2>&1; then
    useradd -r -s /usr/sbin/nologin -d "$APP_DIR" lookingglass
  fi
  install -d -o root -g root -m 755 "$APP_DIR"
}

# Validate before invoking root-owned Git/npm operations. Chowning an untrusted
# clone is insufficient: its hooks/config or package scripts may already have
# been replaced by the service account. Keep legacy files for explicit recovery.
check_install_trust() {
  python3 - "$APP_DIR" <<'PY'
import os, pathlib, stat, sys
root = pathlib.Path(sys.argv[1]).absolute()
def check(path):
    info = path.lstat()
    if info.st_uid != 0 or (not stat.S_ISLNK(info.st_mode) and info.st_mode & 0o022):
        raise SystemExit(f'Unsafe installation path: {path}. Preserve this installation and reinstall from a fresh trusted checkout; do not merely chown its code.')
for path in [*root.parents, root]:
    if path.exists() or path.is_symlink():
        check(path)
        if path.is_symlink():
            raise SystemExit(f'Installation ancestor must not be a symlink: {path}')
for name in ('repo', 'frontend', 'update.sh', '.env', '.deploy-key-path', '.update.lock', 'looking-glass-server', 'looking-glass-agent'):
    path = root / name
    if not (path.exists() or path.is_symlink()):
        continue
    check(path)
    if path.is_symlink():
        raise SystemExit(f'Installation entry must not be a symlink: {path}')
    if path.is_dir():
        for directory, dirs, files in os.walk(path, followlinks=False):
            for name in dirs + files:
                child = pathlib.Path(directory) / name
                check(child)
                if child.is_symlink() and os.path.commonpath((str(path), str(child.resolve()))) != str(path):
                    raise SystemExit(f'Installation symlink escapes trusted tree: {child}')
PY
}

lock_install() {
  check_install_trust
  install -d -o root -g root -m 755 "$APP_DIR"
  exec 9>"$APP_DIR/.update.lock"
  flock -n 9 || fail 'Another install or update is running.'
}

# Keep enough state to undo a failed reinstall, including its service unit and
# secrets/config. Backups remain root-only for manual recovery after success.
begin_activation() {
  INSTALL_BACKUP=$(mktemp -d "$APP_DIR/rollback.install.XXXXXXXX")
  chmod 700 "$INSTALL_BACKUP"
  SYSTEMD_DIR=${SYSTEMD_DIR:-/etc/systemd/system}
  INSTALL_SERVICES=("$@")
  INSTALL_ACTIVE=()
  INSTALL_ENABLED=()
  INSTALL_PATHS=()
  INSTALL_LABELS=()
  for service in "${INSTALL_SERVICES[@]}"; do
    if systemctl is-active --quiet "$service"; then INSTALL_ACTIVE+=("$service"); fi
    if systemctl is-enabled --quiet "$service"; then INSTALL_ENABLED+=("$service"); fi
    backup_install_path "$APP_DIR/$service" "$service.binary"
    backup_install_path "$SYSTEMD_DIR/$service.service" "$service.unit"
    if [ "$service" = looking-glass-server ]; then
      backup_install_path "$APP_DIR/frontend/dist" frontend
    fi
  done
  backup_install_path "$APP_DIR/.env" environment
  backup_install_path "$APP_DIR/update.sh" updater
  trap 'rollback_install "$?"' ERR
  trap 'rollback_install 130' INT
  trap 'rollback_install 143' TERM
  trap 'rollback_install 129' HUP
}

backup_install_path() {
  INSTALL_PATHS+=("$1")
  INSTALL_LABELS+=("$2")
  if [ -e "$1" ] || [ -L "$1" ]; then cp -a "$1" "$INSTALL_BACKUP/$2"; fi
}

rollback_install() {
  local result=${1:-1} index service active enabled candidate
  trap - ERR INT TERM HUP
  set +e
  echo "ERROR: Installation failed; restoring $INSTALL_BACKUP" >&2
  for service in "${INSTALL_SERVICES[@]}"; do systemctl stop "$service" || true; done
  for index in "${!INSTALL_PATHS[@]}"; do
    rm -rf "${INSTALL_PATHS[$index]}"
    if [ -e "$INSTALL_BACKUP/${INSTALL_LABELS[$index]}" ] || [ -L "$INSTALL_BACKUP/${INSTALL_LABELS[$index]}" ]; then
      cp -a "$INSTALL_BACKUP/${INSTALL_LABELS[$index]}" "${INSTALL_PATHS[$index]}"
    fi
  done
  systemctl daemon-reload
  for service in "${INSTALL_SERVICES[@]}"; do
    enabled=false
    for candidate in ${INSTALL_ENABLED[@]+"${INSTALL_ENABLED[@]}"}; do if [ "$candidate" = "$service" ]; then enabled=true; fi; done
    if "$enabled"; then systemctl enable "$service"; else systemctl disable "$service"; fi
    active=false
    for candidate in ${INSTALL_ACTIVE[@]+"${INSTALL_ACTIVE[@]}"}; do if [ "$candidate" = "$service" ]; then active=true; fi; done
    if "$active"; then systemctl restart "$service"; fi
  done
  exit "$result"
}

finish_activation() {
  trap - ERR INT TERM HUP
  printf 'Previous installation retained at %s\n' "$INSTALL_BACKUP"
}

# EnvironmentFile overrides Environment=. Rewrite only the exact old default,
# retaining unrelated custom paths and every other configuration line.
migrate_legacy_schedule_env() {
  python3 - "$APP_DIR" <<'PY'
import os, pathlib, re, stat, sys, tempfile
root = pathlib.Path(sys.argv[1]); env = root / '.env'
if not env.exists():
    raise SystemExit(0)
text = env.read_text()
legacy = re.escape(str(root / 'schedules.json'))
pattern = re.compile(r'(?m)^(\s*SCHEDULES_FILE\s*=\s*)(?:"' + legacy + r'"|\x27' + legacy + r'\x27|' + legacy + r')[ \t]*$')
updated, count = pattern.subn(lambda m: m[1] + str(root / 'data/schedules.json'), text)
if count:
    info = env.stat()
    fd, path = tempfile.mkstemp(prefix='.env.migrate.', dir=root)
    try:
        with os.fdopen(fd, 'w') as out:
            out.write(updated)
            os.fchmod(out.fileno(), stat.S_IMODE(info.st_mode))
            os.fchown(out.fileno(), info.st_uid, info.st_gid)
        os.replace(path, env)
    finally:
        if os.path.exists(path): os.unlink(path)
    print('migrated')
PY
}

# Replace a runtime destination without following service-created symlinks.
# Stage in the root-owned application directory, set ownership on its open FD,
# and rename through a pinned directory FD; never chown the destination path.
migrate_legacy_schedule_data() {
  python3 - "$APP_DIR" "$1" "$2" <<'PYDATA'
import os, pathlib, shutil, stat, sys, tempfile
root = pathlib.Path(sys.argv[1])
uid, gid = map(int, sys.argv[2:])
directory = os.open(root / 'data', os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
try:
    source = os.open(root / 'schedules.json', os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(source, 'rb') as incoming:
        if not stat.S_ISREG(os.fstat(incoming.fileno()).st_mode):
            raise SystemExit('Legacy schedules must be a regular file')
        fd, staged = tempfile.mkstemp(prefix='.schedules.migrate.', dir=root)
        try:
            with os.fdopen(fd, 'wb') as outgoing:
                shutil.copyfileobj(incoming, outgoing)
                outgoing.flush()
                os.fchmod(outgoing.fileno(), 0o600)
                os.fchown(outgoing.fileno(), uid, gid)
                os.fsync(outgoing.fileno())
            os.replace(staged, 'schedules.json', dst_dir_fd=directory)
        finally:
            if os.path.exists(staged): os.unlink(staged)
finally:
    os.close(directory)
PYDATA
}
