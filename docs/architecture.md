# Architecture and compatibility

Maintainer reference for Parallax. Start with [AGENTS.md](../AGENTS.md) for
setup, checks and contribution conventions. Paths below are relative to the repository root.

## Naming

The project is **Parallax** (renamed from "Looking Glass", which is the generic term for this class
of tool). The rename was deliberately partial — these identifiers still carry the old name and must
not be changed casually:

- `deploy/*.sh`: `/opt/looking-glass`, `looking-glass-server.service`, `looking-glass-agent.service`,
  the `/root/.ssh/looking-glass-deploy` key path and the `github.com-looking-glass` SSH alias.
  Changing these means reinstalling on the server and every agent node. The repo URL itself has
  already moved to `EMOEMOJAI/Parallax`; existing installs
  need their clone's origin migrated. Public HTTPS needs no key; private SSH
  access needs a read-only deploy key authorized for the new repo.
  `update.sh` preserves the clone origin (or uses `REPO_URL`) and builds installed services.
- `lookingglass_*` — the Prometheus metric names in `backend/ops.go`. Renaming breaks existing
  Grafana dashboards.
- `lg-client-key`, `lg-cmd-history`, `lg-kit` and `lookingGlass.presets` — browser localStorage keys.
  Renaming signs users out and discards their saved history and presets.
- `lg.bearer` — the WebSocket auth subprotocol. Frontend and server must agree, so it can only change
  in a single coordinated deploy.

Move any of these only as a deliberate migration, not as part of unrelated work.

## Repository layout

Three-tier network diagnostic tool. Each tier lives in its own top-level directory:

- `backend/` — Go HTTP + WebSocket server, split across `main.go` (setup, routing, shared `Server` struct) and per-concern files: `agent_ws.go`, `client_ws.go`, `nodes.go`, `mesh.go`, `scheduler.go`, `runs.go`, `speedtest.go`, `geoip.go`, `rdap.go`, `metadata_client.go`, `public.go`, `alerts.go`, `ops.go`, `ratelimit.go`, `middleware.go`, `summary.go`, `util.go`. Also serves the built frontend.
- `agent/` — Go binary deployed to remote nodes. `main.go` and `commands.go` run whitelisted network commands; `shell.go` runs PTY-backed interactive shells; `probes.go` and `summary.go` implement the native Go probes and their output summaries.
- `frontend/` — React 19 + Vite 8 + Tailwind 4 SPA. Exact versions live in `frontend/package.json`; components in `src/components/`, hooks in `src/hooks/`.
- `deploy/` — Debian/Ubuntu install + update scripts (systemd-based; public HTTPS or optional SSH deploy key).

## Common commands

Local dev (three terminals):
```bash
cd backend && go run .                 # server on :8080
cd frontend && npm ci && npm run dev             # Vite dev server, proxies /api and /ws to :8080
cd agent && go run . -server ws://localhost:8080/ws/agent -name Local -auto-ip=false
```

Build from the repository root:
```bash
(cd frontend && npm ci && npm run build) # → frontend/dist (consumed by server)
(cd backend && CGO_ENABLED=0 go build -o parallax-server .)
(cd agent && CGO_ENABLED=0 go build -o parallax-agent .)
```

Containers: `docker compose up --build` (server + one example agent).

Run `go test -race ./...` and `go vet ./...` in each Go module. Frontend checks are
`npm test` and `npm run build`; there is no frontend lint script. Deployment checks
are `python3 -m unittest discover -s deploy/tests` and `shellcheck deploy/*.sh`.

## Architecture

```
Browser ──/ws/client──▶ Server ──/ws/agent──▶ Agent (one per location)
```

- **Both WebSocket directions are message-multiplexed** with an `{action, payload}` envelope (`AgentMessage` in both `backend/main.go` and `agent/main.go`). Add a new feature by adding a new `action` in both sides, not a new endpoint.
- **The server holds persistent schedule state**. Nodes register themselves over `/ws/agent`; their UUID is assigned by the server and reused if an agent with the same `name` reconnects after its previous connection dies (duplicate names with a live conn are rejected).
- **The server also serves the SPA**. It looks for `frontend/dist` in the CWD, falling back to `../frontend/dist`, so running `go run .` from `backend/` works for local dev.
- **Commands flow**: client sends `{node_id, command:{id, type, target, options}}` → server routes to the right agent via `node.conn` → agent runs the command and streams `output`/`error`/`done` messages back → server forwards each to the originating client (tracked via `cmdOwners`/`cmdNodes` maps keyed by command id).
- **Protocol message types have grown past `output`/`error`/`done`**: agent `register` and `health` messages now carry `version` and `tools` fields so the server knows an agent's build and which binaries it has; a **separate `summary` message** — its own `output.Type`, sent before `done`, not a field on it — carries a structured, tool-specific digest of the run; the native probes (`tcp`, `tls`, `dnsbench`, `download`) stream through the same `output`/`error`/`done` envelope as the exec-based command types.
- **Shell startup** accepts optional `cols` and `rows` on `shell_start`. The server forwards them to the agent, which sizes the PTY before starting the process; absent or zero values retain the legacy 120×40 default. Shell output carries complete UTF-8 sequences in JSON strings, even when a PTY read splits a character.

### Concurrency model (read this before touching the server)

- Every WebSocket connection has its own write mutex. **All data writes to a connection must hold its mutex** — there are dedicated ping goroutines that also write. `Node.conn` and `Node.mu` are immutable for a connection; reconnect replaces the node object while preserving its ID. Shutdown may use Gorilla's concurrency-safe `WriteControl` and `Close` without that mutex to stay bounded. For agents this is `node.mu` (shared via `connMu` between the ping goroutine and the read loop). For clients it's `clientMu`.
- Per-concern RWMutexes/mutexes: `nodesMu`, `cmdOwnersMu`, `cmdNodesMu`, `clientsMu`, `publicClientsMu`, `geoCacheMu`, `geoRateMu`, `rdapCacheMu`, `runStoreMu`, `schedulesMu`, `scheduleRunsMu`, `saveMu`, `saveWriteMu`, `summarySeenMu`, `latencyMatrixMu`, `meshRunsMu`, `httpLatencyMu`, and `rateMu` (the rate-limit lock), plus per-schedule lifecycle locks. **Lock ordering: release `nodesMu` before acquiring any other** (see the `DELETE /api/nodes` and agent-disconnect paths for examples). Violating this will deadlock. Command retirement holds `cmdOwnersMu` before `cmdNodesMu` or `summarySeenMu`; never acquire `cmdOwnersMu` while holding either subordinate mutex. The WebSocket shutdown registry has its own `webSocketsMu` and gates new tracking before waiting for handlers.
- WebSocket-level ping/pong every 30s with a 90s read deadline on both ends. The deadline is reset on any successful read so streaming commands don't get killed.
- `http.Server.ReadTimeout` / `WriteTimeout` are intentionally **not** set — they apply to the whole connection lifetime and would kill long-lived WebSockets. `ReadHeaderTimeout` bounds the initial HTTP headers and `IdleTimeout` bounds idle HTTP keep-alive connections.

### Security-sensitive invariants

- Auth: `AGENT_API_KEY` (agent ↔ server), `CLIENT_API_KEY` (browser ↔ server). Both prefer `Authorization: Bearer <key>` and accept legacy `?key=` credentials. Browser WebSockets also accept `Sec-WebSocket-Protocol: lg.bearer, <key>` between header and query precedence; only `lg.bearer` is echoed. Key checks use `subtle.ConstantTimeCompare`. The server logs warnings at startup if either key is unset. See [authentication and public sessions](reference.md#authentication-and-public-sessions) for the open-mode boundary.
- WebSocket origin: `ALLOWED_ORIGINS` (comma-separated). Permissive default with a startup warning — must be set in production.
- All agent-supplied strings (registration fields, health info) go through `stripControlChars` → rune-truncate (`sanitizeString`, `backend/util.go`) before being stored. Don't bypass this when adding new agent-supplied fields. **Storage does not HTML-escape** — `sanitizeString` used to, which is why provider names rendered as `AT&amp;T`; escaping is now purely a render-layer concern, so **every output sink must protect itself**:
  - React escapes JSX text children, which is every node/hop/summary string the SPA renders. Keep them text children — never build markup from them.
  - `GeoMap.createNodeIcon` (`frontend/src/components/GeoMap.jsx`) is the **only** HTML-building sink in the frontend (a Leaflet `divIcon`), and its `escapeHtml(flag)` call is now the *sole* protection for it. Don't remove it, and don't interpolate any other agent-controlled value into that template.
  - `dangerouslySetInnerHTML` appears nowhere in `frontend/src` and must stay that way.
  - `xterm`'s `term.write` (ShellTerminal) takes raw PTY bytes by design; error text written there is stripped of `\x00-\x1f\x7f` first.
  - Server-side sinks with their own encoding keep it: `/metrics` label values go through `metricsLabelValue` (`backend/ops.go`), and JSON responses are encoded by `encoding/json`.
- Schedules carry a per-schedule `schema_version` marker (`backend/scheduler.go`). Absent means "written when `sanitizeString` still escaped"; `loadSchedules` then applies `html.UnescapeString` to `NodeName` **once**, re-strips/re-truncates it, stamps the marker and persists synchronously after releasing `schedulesMu`. It is per-schedule and not top-level because `schedules.json` is a top-level JSON array — a top-level object would make an older binary log "starting empty" and rewrite the file, losing every schedule.
- Command whitelisting lives in four lists that are **deliberately not identical** — adding a type means deciding about each, not copying it everywhere: `allowedCommandTypes` (`backend/main.go`, what a client may request) and `allowedCommands` (`agent/main.go`, what the agent will run) do match today; `commandBuilders` (`agent/main.go`) holds only the 8 exec-based types, because the native probes dispatch through `nativeProbes` instead; and `scheduleAllowedCommands` (`backend/scheduler.go`) deliberately omits `nexttrace`, `iperf3`, `speedtest` and `download`. The string `"shell"` is intentionally excluded from all of them — interactive shells use the separate `shell_start` / `shell_input` action pair.
- `iperf3` flags are individually whitelisted with parameter caps (port 1024-65535, duration ≤60s, parallel ≤10, bandwidth ≤100 Mbit/s, bytes ≤1 GB). Adding new flags requires updating `allowedIperf3Flags` and the per-flag validator branch in `buildIperf3`.
- Agent caps: 10-minute command timeout, 10 MB total output, 1 MB max line, 5 concurrent shell sessions, 16 KB max shell input chunk. The server caps concurrent commands per client at 20.

### Frontend wiring

- Single WebSocket per browser tab via the `useWebSocket('/ws/client')` hook, fanned out by `subscribe(channelName, handler)`. Each major component (`App`, `LatencyMatrix`, `MultiNodeCompare`, `ShellTerminal`) registers its own channel; don't open extra sockets.
- Vite proxies `/api` and `/ws` to `http://localhost:8080` in dev (see `vite.config.js`).
- Modal state in `App.jsx` is a single string (`'health' | 'matrix' | 'compare' | 'map' | 'shell' | 'schedules' | null`), not six booleans — keep it that way.
- Output buffer is hard-capped at 10000 lines (`MAX_OUTPUT_LINES`) to prevent unbounded memory growth on long-running commands.

## Deployment notes

- `deploy/install.sh` provisions a Debian/Ubuntu host, builds the frontend and server from a public HTTPS or optional SSH clone, and installs `looking-glass-server.service` (systemd). Env vars live in `/opt/looking-glass/.env`.
- `deploy/agent-only-install.sh` does the same for an agent-only node.
- `deploy/update.sh` fetches the configured branch, fast-forwards a clean clone, builds installed roles and restarts services that were running. Failed activation restores prior artifacts; see [update and rollback](deployment.md#update-and-rollback).
- Installers default to `https://github.com/EMOEMOJAI/Parallax.git`; override `REPO_URL` for forks and optionally `DEPLOY_KEY` for private SSH access.

### Environment variables

Backend (16): `PORT`, `CLIENT_API_KEY`, `AGENT_API_KEY`, `ALLOWED_ORIGINS`, `STRICT_ORIGIN`, `PUBLIC_MODE`, `PUBLIC_COMMANDS`, `PUBLIC_TARGETS`, `TRUST_PROXY`, `TRUSTED_PROXY_HOPS`, `METRICS_TOKEN`, `ALERT_WEBHOOK_URL`, `MESH_INTERVAL_SEC`, `SCHEDULES_FILE`, `HSTS`, `LOG_FORMAT`.

Agent (2): `AGENT_API_KEY`, `PROBE_ALLOW_PRIVATE`.
