# Working on Parallax

Parallax is a self-hosted network looking glass: Go server and remote agents,
with a React/Vite dashboard. Start with [README.md](README.md) for product context.

## Find the right code

| Directory | Responsibility |
|---|---|
| `backend/` | HTTP/WebSocket routing, authentication, schedules, metrics and latency mesh |
| `agent/` | Diagnostic commands, native probes, capabilities and PTY sessions |
| `frontend/` | React components, hooks, browser authentication and streamed output |
| `deploy/` | Debian/Ubuntu installation, systemd updates and rollback |
| `docs/` | Setup, deployment, configuration and architecture |

Read [docs/architecture.md](docs/architecture.md) before changing protocol,
concurrency, authentication, persistence or compatibility behavior.
[docs/reference.md](docs/reference.md) is the configuration/API reference.

For recurring audits, read [docs/audit-rotation.md](docs/audit-rotation.md).
It tracks chunk scope, cadence, reviewed revisions, findings and the next review.
Keep that record current when running an audit; ordinary changes still use the
checks below and the contribution workflow.

## Setup and checks

Use the Go and Node versions pinned in `.github/workflows/ci.yml`.
See [docs/getting-started.md](docs/getting-started.md) for the three-terminal setup.
Run the relevant checks for the area changed:

```sh
(cd backend && go vet ./... && go test -race ./...)
(cd agent && go vet ./... && go test -race ./...)
(cd frontend && npm ci && npm run lint && npm test && npm run build)
python3 -m unittest discover -s deploy/tests
shellcheck deploy/*.sh
git diff --check
```

Format Go with `gofmt`. Frontend code uses functional React components, hooks,
single quotes and no semicolons. Frontend lint checks JavaScript correctness and
React hook dependencies; there is no typecheck script.
Keep documentation-only changes focused; run the backend documentation gate
with `cd backend && go test -run 'TestS11' ./...` when changing the reference.

## Preserve these contracts

- Keep existing `looking-glass` service/install names, `lookingglass_*` metrics,
  browser storage keys and the `lg.bearer` protocol unless implementing a migration.
- Server and agent command allowlists must agree. Native probes, external command
  builders and the smaller schedule allowlist have different responsibilities.
- A summary precedes `done`; release completed request IDs before publishing
  terminal output. Old cancellation/output must never affect a reused ID.
- Serialize WebSocket data writes per connection and follow documented lock order.
- Render untrusted strings as text. Preserve Leaflet escaping and probe address
  validation; do not introduce `dangerouslySetInnerHTML`.
- Document configuration changes in `docs/reference.md`; the backend checks
  environment-variable coverage there.
- Keep credentials, `.env`, node inventories, private keys, runtime state and
  generated binaries out of commits. Do not invent a license or change deployment
  defaults as a side effect of documentation work.
