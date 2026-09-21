# Configuration and API reference

[Overview](../README.md) · [Getting started](getting-started.md) · [Deployment](deployment.md)

## Probe types

These are the 12 command types a client can send over `/ws/client` (`allowedCommandTypes`,
`backend/main.go`, mirrored by `allowedCommands` in `agent/main.go`):

- **ping** — ICMP ping to a target
- **traceroute** — Trace the route to a target
- **mtr** — Combined traceroute + ping (My Traceroute)
- **nexttrace** — Visual route tracing
- **iperf3** — Network bandwidth testing
- **speedtest** — Speedtest.net CLI
- **dns** — DNS lookup
- **http** — HTTP request probe
- **tcp** — Native Go TCP connect probe (no external binary needed on the node)
- **tls** — Native Go TLS handshake probe (no external binary needed on the node)
- **dnsbench** — Native Go DNS benchmark probe (no external binary needed on the node)
- **download** — Native Go download-speed probe (no external binary needed on the node)

`tcp`, `tls`, `dnsbench` and `download` are Go-native: they need no external binary on the agent
node. `tcp`, `tls` and `download` enforce the agent's private-address policy on every target, which
`PROBE_ALLOW_PRIVATE` relaxes. **`dnsbench` is deliberately exempt for its system resolvers** — the
resolvers in `/etc/resolv.conf` are usually private, and benchmarking them is the point — so it will
query a private resolver whether or not that variable is set.
DNS benchmarks send queries directly to each selected resolver; `/etc/hosts` entries do not
substitute for a DNS response.

### Not command types, but related features

- **Interactive shell** — a PTY-backed shell session on the node, enabled by default and controlled by the agent's
  `-allow-shell` flag (`-allow-shell=false` disables it). `shell` is deliberately excluded from both the server's and the agent's
  command whitelists; it uses its own `shell_start` / `shell_input` action pair over the same
  WebSocket, not a `command`.
- **Browser speed test** — measures throughput directly between your browser and the server over
  `/ws/speedtest`, independent of any agent node.

### Schedules and recovery

Any node can run a schedule that repeats a command on an interval and keeps a history of results.
Only 8 of the 12 command types are schedulable (`scheduleAllowedCommands`,
`backend/scheduler.go`): **ping, traceroute, mtr, dns, http, tcp, tls, dnsbench**. `nexttrace`,
`iperf3`, `speedtest` and `download` are **not** schedulable — they are too expensive, too
interactive, or (for `download`) too much unreviewed traffic to run unattended on a timer.

Schedule results feed:

- **`/metrics`** — Prometheus-format gauges per schedule, for scraping into Grafana/Alertmanager.
  They are served **only to a scrape that presents `METRICS_TOKEN`**; with the token unset the
  endpoint stays open but emits process-level metrics only.
- **Webhook alerts** — `ALERT_WEBHOOK_URL` receives a POST when a scheduled probe's result crosses
  into an alerting state.

If the schedules file cannot be read or parsed at startup, the server preserves it
and returns HTTP 503 for authorized schedule requests. Repair or restore the file
and restart the server to resume scheduling; other diagnostic features remain
available. A missing file is treated as a new installation with no schedules.

## Agent flags

| flag | purpose |
|---|---|
| `-server` | Server WebSocket URL to register with |
| `-name` | Node display name |
| `-location` | Node location label |
| `-flag` | Flag emoji for the location |
| `-ipv4` | Node IPv4 address |
| `-ipv6` | Node IPv6 address |
| `-provider` | Hosting provider name |
| `-lat` | Node latitude |
| `-lon` | Node longitude |
| `-allow-shell` | Allow interactive shell sessions (default **true**); use `-allow-shell=false` to disable them |
| `-allow-tcp-traceroute` | Allow traceroute's TCP mode (`-T`, a port-scan primitive) on this node; default **false** |
| `-auto-ip` | Detect public IPv4/IPv6 at startup and refresh periodically, overriding `-ipv4`/`-ipv6`; default **true** |

## Configuration

### Backend

| variable | purpose |
|---|---|
| `PORT` | HTTP listen port |
| `CLIENT_API_KEY` | Browser ↔ server auth key |
| `AGENT_API_KEY` | Agent ↔ server auth key |
| `ALLOWED_ORIGINS` | Comma-separated allowed WebSocket origins |
| `STRICT_ORIGIN` | Reject a WebSocket whose `Origin` does not match the request `Host`. A request with **no** `Origin` is still accepted — curl and CLI clients never send one. Only consulted when `ALLOWED_ORIGINS` is empty; an allowlist takes precedence |
| `PUBLIC_MODE` | Set to `1` to enable restricted unauthenticated public sessions; disabled by default |
| `PUBLIC_COMMANDS` | Comma-separated command allowlist for public sessions; defaults to `ping,traceroute,mtr,dns` |
| `PUBLIC_TARGETS` | Comma-separated, exact-match target allowlist for public sessions; empty by default (no public probes) |
| `TRUST_PROXY` | Set to `1` to trust forwarded client-address headers; enable only behind a trusted proxy that sanitizes them and prevents direct client access |
| `TRUSTED_PROXY_HOPS` | Number of trusted proxy hops counted from the right across all `X-Forwarded-For` fields; malformed or shorter chains fall back to the transport peer |
| `METRICS_TOKEN` | Bearer token for `/metrics`. When set it is required for the whole endpoint and the output is complete. When unset the endpoint stays open but the per-schedule gauges are withheld from **every** scrape — `CLIENT_API_KEY` does not unlock them |
| `ALERT_WEBHOOK_URL` | Webhook URL that receives schedule alert POSTs |
| `MESH_INTERVAL_SEC` | Latency mesh interval: default 300 seconds, minimum 30; `0` disables it |
| `SCHEDULES_FILE` | Schedules persistence path; defaults to `schedules.json` in the working directory. Installers and containers configure their writable data directory |
| `HSTS` | Set to `1` to enable the `Strict-Transport-Security` response header |
| `LOG_FORMAT` | Set to `json` for JSON request logs; plain text by default |

### Agent

| variable | purpose |
|---|---|
| `AGENT_API_KEY` | Agent ↔ server auth key |
| `PROBE_ALLOW_PRIVATE` | Set to `1` to allow `tcp`, `tls` and `download` to target private/loopback addresses. `dnsbench` always queries the system resolvers from `/etc/resolv.conf`, which are exempt by design |

## API routes

### Authentication and public sessions

Client authentication is enforced only when `CLIENT_API_KEY` is configured.
With public mode off and that key unset, every visitor has full client access,
including shells on agents that allow them. `AGENT_API_KEY` separately controls
agent registration; leaving it unset allows uncredentialed agents to connect.

HTTP clients and agents use `Authorization: Bearer <key>`. Browser WebSockets
use the `lg.bearer` subprotocol with the key as the next entry. Precedence for
browser WebSockets is Authorization header, then subprotocol, then the legacy
`?key=` fallback. Prefer headers or the subprotocol to avoid credentials in URLs.

With `PUBLIC_MODE=1`, visitors without a valid client key receive a restricted
session. They may read the public inventory and run only allowed commands against
allowed targets; they cannot open shells or mutate schedules and saved runs.
When `CLIENT_API_KEY` is unset in public mode, every client session is restricted.

### Route access

| route | auth |
|---|---|
| `/` | none — static SPA |
| `/api/nodes` | client auth required **unless `PUBLIC_MODE=1`**; `DELETE` also refused for public sessions |
| `/api/nodes/health` | client auth required **unless `PUBLIC_MODE=1`** |
| `/api/latency-matrix` | `GET`: client auth required **unless `PUBLIC_MODE=1`**. `POST` (the legacy browser-measured write): refused for public sessions, then client auth |
| `/api/latency-matrix/measure` | refused for public sessions, then client auth required |
| `/api/geoip/` | client auth required **unless `PUBLIC_MODE=1`** |
| `/api/rdap/` | client auth required **unless `PUBLIC_MODE=1`** |
| `/api/runs` | `POST`: client auth required, and refused for public sessions. A bare `GET` is not authenticated — it falls through to the permalink reader and returns `400 invalid id` |
| `/api/runs/<id>` | **none — open by design**: a permalink read is unauthenticated, the URL itself is the access token |
| `/api/schedules`, `/api/schedules/` | client auth required and refused for public sessions |
| `/api/version` | none — open |
| `/healthz` | none — open |
| `/metrics` | `METRICS_TOKEN` if set; **otherwise open**, but process-level metrics only. The per-schedule gauges — the schedule inventory (ids, node names, commands, probe targets) — are served **only when `METRICS_TOKEN` is set and matched**; with the token unset they are withheld from every request, credentialed or not, and `CLIENT_API_KEY` is not an alternative credential for them. The server warns at startup when `METRICS_TOKEN` and `CLIENT_API_KEY` are both unset outside public mode |
| `/api/public-config` | none — open by design, so it can advertise whether public mode is on |
| `/ws/agent` | `AGENT_API_KEY` |
| `/ws/client` | client auth required **unless the session is public**, via Authorization, the `lg.bearer` subprotocol or legacy `?key=` |
| `/ws/speedtest` | client auth required; refused for public sessions |

### Metadata lookup transport

GeoIP and RDAP lookups connect directly to public provider addresses; environment
HTTP proxies are not used for these requests. Private, on-link (including global IPv6), and special-use
addresses are rejected after DNS resolution and on redirect connections. Redirects
require HTTPS and are limited to five hops. TLS certificate verification remains
enabled. Lookups stop when the requesting client disconnects. The free GeoIP
provider uses HTTP for its initial request, so its location data is advisory and
not a trusted identity or authorization input.
