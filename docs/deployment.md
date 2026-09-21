# Deployment

Parallax runs a Go server, a built frontend, and one agent per probe location.
The scripts support Debian/Ubuntu with systemd. Container examples are in
`docker-compose.yml`.

## Install from source

Review the scripts, then run them from a trusted checkout:

```sh
sudo ./deploy/install.sh
sudo ./deploy/agent-only-install.sh wss://example.com/ws/agent Node-1 "Example location" "🌐" "Example provider"
```

Public repositories use HTTPS and require no GitHub account or deploy key.
`REPO_URL` and `BRANCH` select a fork or branch. For a private repository, create
a read-only deploy key on the host, register only its public key with GitHub,
and pass its private-key path:

```sh
sudo env REPO_URL=git@github.com:OWNER/REPO.git \
  DEPLOY_KEY=/root/.ssh/parallax-deploy ./deploy/install.sh
```

Private keys belong outside the repository. Never commit `.env`, deploy keys,
node inventories, runtime state, or deployment bundles. Git and Docker ignore
rules exclude common local artifacts, but they do not remove files already
committed or present in history.

Installers provision supported Go/Node toolchains when needed, verifying vendor
archive checksums. Application code and binaries are root-owned; only the
server's `/opt/looking-glass/data` directory is writable by its service user.
Configure `/opt/looking-glass/.env` for authentication and allowed browser
origins before exposing the service beyond a trusted network. Agent shell
sessions remain enabled by default; use `-allow-shell=false` in the agent unit
if they are not required.

### Agent diagnostic tools

The agent installer and example agent image include `ping`, `traceroute`,
`mtr`, `iperf3`, `dig` (the `dns` command) and `curl` (the `http` command).
`nexttrace` and the `speedtest` executable are optional and must be installed
separately on the agent's `PATH`. Release archives contain the Parallax binaries,
so archive users must provide these external tools themselves. Native `tcp`,
`tls`, `dnsbench` and `download` probes need no external diagnostic binary.
The dashboard reports detected tools; raw-socket probes also need the relevant
OS permissions, supplied by the installer or example container configuration.

## Update and rollback

```sh
sudo /opt/looking-glass/update.sh
```

The updater locks the installation, fetches the configured origin, refuses
local changes or commits absent from the requested remote branch, and builds
only installed service roles. It finishes builds before replacing artifacts,
restarts services that were already running, and restores prior artifacts if
activation fails or receives an interrupt. Root-only `rollback.*` directories
retain previous binaries and frontend files. Back up persistent schedules
separately before upgrades that change the data format.

To switch an existing clone to public HTTPS, set `REPO_URL` for one update:

```sh
sudo env REPO_URL=https://github.com/OWNER/REPO.git /opt/looking-glass/update.sh
```

A saved deploy-key path is ignored for HTTPS origins. After verifying HTTPS
access, revoke an unused deploy key in GitHub and remove its private file from
the host.

### Legacy installations

Older installers made the whole application directory writable by the service
account. Updated scripts refuse to run root Git/build commands against that
code. Changing ownership alone does not establish that the source is intact.

For migration, use a trusted checkout outside the old application directory:
stop the affected services; preserve the existing environment, schedules,
service units, and binaries in a root-only backup; move the old application
directory aside; then run the appropriate installer with a fresh root-owned
clone. Restore reviewed configuration and schedule data into the new writable
data directory, preserving node names and connection settings. Restart and
verify the server and each agent before retiring the backup. Never copy the
old clone's hooks or Git configuration into the new clone.

## Map tiles

The network map loads standard OpenStreetMap tiles directly in the browser;
no API key is required. Keep the visible attribution and the server's
`strict-origin-when-cross-origin` referrer policy. Tile requests disclose the
visitor's IP address and site origin to the provider. Normal interactive use
must follow the [tile usage policy](https://operations.osmfoundation.org/policies/tiles/);
bulk downloads, offline prefetching and cache bypass are not supported.
For high-volume or offline deployments, use an appropriate tile provider or
self-hosted tiles, updating both `GeoMap.jsx` and the server's `img-src` CSP.

## Containers

```sh
docker compose up --build
```

The example server persists schedules in the `schedules` named volume. Removing
containers preserves it; `docker compose down --volumes` deletes that data.
The agent runs without root and receives only its probe-related network
capability from Compose. Set matching agent keys and a client key for an
installation reachable by untrusted users.

## Before making a repository public

Review the staged tree and Git history separately for credentials, personal
metadata, and generated artifacts. A clean current tree does not erase old
commits. No history rewrite or force-push is performed by these scripts.

CI uses disposable GitHub-hosted runners for every job, including pull requests.
It checks both Go modules, the frontend, deployment scripts and both container
images. **Remove any previously registered self-hosted runners before making the
repository public**: a contributor can submit a different workflow requesting
one, regardless of the conditions in the committed CI workflow. Do not run
untrusted pull-request code on a machine with access to your private network.
