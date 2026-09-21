# Audit rotation

This file tracks recurring, scoped reviews of Parallax. It supplements PR review,
CI, CodeQL and dependency scanning. One session reviews one chunk by default.
The rotation is maintainer-run; this file does not schedule an automated audit.

## Tier cadence

| Tier | Cadence | Scope |
|---|---|---|
| 1 | Every 3 calendar months | Authentication, public access, remote execution and secrets |
| 2 | Every 6 calendar months | Input parsing, concurrency, persistence, deployment and supply chain |
| 3 | On touch after the first pass | Remaining UI and documentation; explicit triggers below |

Calculate the next due date from **Last pass**, adding the tier's calendar months
(clamp to the last day of the destination month when needed). A missing or
unresolvable baseline is immediately due for a full first pass, including Tier 3.
Unchanged code that is due still needs review. Skipping an unchanged chunk must
not reset its baseline, audit date or unresolved findings.

## Coverage evidence

Setup date: 2026-09-20. Public history and merged PRs were inspected.
[PR #1](https://github.com/EMOEMOJAI/Parallax/pull/1) and
[PR #7](https://github.com/EMOEMOJAI/Parallax/pull/7) document regression tests,
privacy checks and release validation, but do not establish complete, scoped
audit coverage for the chunks below. All baselines therefore start at `none`.
This does not discard prior fixes or claim that no earlier reviews occurred.

All 12 chunks were initially due as of setup. The tables below track completed passes.
Dates become meaningful after each recorded pass; do not use setup or CI run
dates as audit dates. There is no separate frozen or legacy source directory;
compatibility and rollback behavior belong to their active subsystem chunks.

## Next up

**T2-01 — Native probes and network address policy.** Baseline: `none`; due now
for a full first pass. Then prioritize the remaining Tier 1 chunks, followed by
Tier 2 and the initial Tier 3 passes. Within a tier, choose open findings and
then the most overdue chunk; break ties by chunk ID. Cadence overrides take
precedence. Update this pointer after each review.

## Scope conventions

Paths are repository-relative; directory entries include descendants. Include
the tests and fixtures exercising the selected behavior, even when a shared
regression file spans several chunks. A shared file can appear in several rows:
review the responsibilities described in **Focus**, not unrelated behavior.
Security-sensitive UI belongs to Tier 1 or 2 even when it is also part of the
broader frontend tree. Read [architecture.md](architecture.md) for contracts and
lock order. Assign new files to a chunk before recording coverage.

## Tier 1 chunks

| ID | Chunk | Paths | Focus | Baseline | Last pass | Status |
|---|---|---|---|---|---|---|
| T1-01 | Authentication and browser credentials | `backend/main.go`, `backend/middleware.go`, `backend/client_ws.go`, `backend/agent_ws.go`, `agent/main.go`, `frontend/src/lib/api.js`, `frontend/src/components/KeyPrompt.jsx`, `frontend/src/App.jsx`, `frontend/src/hooks/useWebSocket.js` | Client/agent identity, route authorization, origin/proxy trust, credential transport/storage, authentication failures and reconnects. | 3186a777877a6e5055936f29031c31b775806c78 | 2026-09-21 | clean |
| T1-02 | Public endpoints and data exposure | `backend/public.go`, `backend/ops.go`, `backend/ratelimit.go`, `backend/nodes.go`, `backend/runs.go`, `backend/geoip.go`, `backend/metadata_client.go`, `backend/rdap.go`, `backend/speedtest.go`, `backend/alerts.go`, `backend/main.go`, `backend/client_ws.go`, `frontend/src/components/GeoMap.jsx` | Anonymous access, target/command restrictions, metrics and shared-run disclosure, outbound request boundaries, rate limits and HTML escaping. | 68a6b8160cd6ea5c802cb361f51d510dd9834dd8 | 2026-09-21 | clean |
| T1-03 | Command execution and interactive shells | `agent/main.go`, `agent/commands.go`, `agent/shell.go`, `backend/main.go`, `backend/client_ws.go`, `backend/command_cleanup.go`, `backend/scheduler.go`, `frontend/src/components/ShellTerminal.jsx` | Allowlists, argv/options validation, shell authorization and opt-out, privileges, terminal input/output limits, cancellation and process cleanup. | 3f53fc2f314095530e9dfad7cfd767d5d5907134 | 2026-09-21 | clean |
| T1-04 | Secrets and publication privacy | `scripts/check-privacy.py`, `scripts/check-commit-privacy.py`, `scripts/install-hooks.py`, `scripts/tests/`, `.githooks/`, `.gitignore`, `.dockerignore`, `.github/workflows/`, `backend/main.go`, `backend/middleware.go`, `backend/client_ws.go`, `agent/main.go`, `deploy/`, `tests/integration/` | Secret/config handling and log redaction; tracked files, commit metadata, package/image contents and CI artifacts must not disclose credentials or deployment identities. | 66975f6351d85027a45c024a3adceb1e84776447 | 2026-09-21 | clean |

## Tier 2 chunks

| ID | Chunk | Paths | Focus | Baseline | Last pass | Status |
|---|---|---|---|---|---|---|
| T2-01 | Native probes and network address policy | `agent/probes.go`, `agent/dnsbench.go`, `agent/main.go` | DNS rebinding, IPv4/IPv6 and local-address policy, redirects, TLS diagnostic verification, DNS parsing, timeouts, byte limits and cancellation. | none | none | due — first pass |
| T2-02 | WebSockets and request lifecycle | `backend/main.go`, `backend/agent_ws.go`, `backend/client_ws.go`, `backend/command_cleanup.go`, `backend/nodes.go`, `agent/main.go`, `agent/commands.go`, `agent/probes.go`, `agent/shell.go`, `frontend/src/hooks/useWebSocket.js`, `frontend/src/hooks/useNodes.js`, `frontend/src/lib/id.js`, `frontend/src/lib/nodes.js` | Serialized writes, lock order, backpressure, reconnect/shutdown, request ownership, stale cancellation/output, ID reuse and mixed-version compatibility. | none | none | due — first pass |
| T2-03 | Schedules, saved runs and latency mesh | `backend/scheduler.go`, `backend/runs.go`, `backend/mesh.go`, `backend/alerts.go`, `backend/ops.go`, `frontend/src/components/Schedules.jsx`, `frontend/src/components/LatencyMatrix.jsx` | Persistence/migrations, restart recovery, retention, overlapping runs, mesh concurrency, alert delivery and sensitive state in metrics. | none | none | due — first pass |
| T2-04 | Untrusted output and metadata parsing | `backend/summary.go`, `backend/util.go`, `backend/geoip.go`, `backend/metadata_client.go`, `backend/rdap.go`, `backend/speedtest.go`, `agent/summary.go`, `agent/testdata/`, `frontend/src/lib/`, `frontend/src/components/OutputTerminal.jsx`, `frontend/src/components/SummaryBadges.jsx`, `frontend/src/components/GeoMap.jsx` | Malformed/oversized external data, UTF-8, control characters, summary-before-done ordering, parser resource limits, text rendering and capability fallbacks. | none | none | due — first pass |
| T2-05 | Installation, updates and runtime packaging | `deploy/`, `Dockerfile.server`, `Dockerfile.agent`, `docker-compose.yml`, `.dockerignore`, `scripts/package-release.py`, `tests/integration/`, `docs/deployment.md` | Verified downloads, clone/update trust, filesystem ownership, service permissions, rollback recovery, configuration preservation and release archive behavior. | none | none | due — first pass |
| T2-06 | Dependencies, build and hosted CI | `.github/`, `backend/go.mod`, `backend/go.sum`, `agent/go.mod`, `agent/go.sum`, `frontend/package.json`, `frontend/package-lock.json`, `frontend/vite.config.js`, `frontend/playwright.config.js`, `tests/integration/requirements.txt`, `scripts/package-release.py`, `Dockerfile.server`, `Dockerfile.agent` | Vulnerabilities and upgrades, pinned actions/tools, workflow permissions and untrusted PR input, test coverage, artifact provenance and release gates. | none | none | due — first pass |

## Tier 3 chunks

| ID | Chunk | Paths | Focus | Baseline | Last pass | Status |
|---|---|---|---|---|---|---|
| T3-01 | Dashboard behavior and accessibility | `frontend/src/`, `frontend/tests/`, `frontend/index.html`, `frontend/public/` | Remaining UI behavior, local history/kits/presets, subscriptions, accessibility, rendering bounds and error recovery. Trigger: change to UI behavior, browser storage, styles, assets or frontend tests; retain higher-tier security reviews where applicable. | none | none | due — first pass |
| T3-02 | Documentation and tool guidance | `docs/`, `README.md`, `CONTRIBUTING.md`, `SECURITY.md`, `AGENTS.md`, `CLAUDE.md`, `GEMINI.md`, `llms.txt`, `LICENSE`, `.github/copilot-instructions.md` | Accurate defaults, setup/recovery instructions, safe examples, links and consistent coding-tool guidance. Trigger: documentation/branding changes or code changes affecting documented configuration, protocols, installation or recovery. | none | none | due — first pass |

## Cadence overrides

- Authentication, credential storage, origin/proxy trust or new routes: review
  T1-01 and affected public-access behavior in T1-02 before merge.
- New commands/options, PTY changes or permission changes: T1-03; address-policy,
  resolver, redirect or TLS changes also trigger T2-01 immediately.
- New output sinks, parsers or external services: T1-02 and T2-04 as applicable.
- Protocol, cancellation, connection ownership or locking changes: T2-02;
  scheduling, persisted formats or retention changes: T2-03 before release.
- Installer, service, container or rollback changes: T2-05 before deployment;
  review changed recovery code before using it to restore a service.
- Major dependency/toolchain upgrades, new dependencies, workflow permission or
  release changes, and relevant security advisories: T2-06 immediately and any
  affected runtime chunk. Existing automated scans continue independently.
- Changes to credentials, logs, privacy guards or published artifacts: T1-04
  before publication. Keep real inventories, private evidence and exploitable
  unpatched vulnerability details out of this public file; follow [SECURITY.md](../SECURITY.md).
- An incident or credible vulnerability report triggers the affected chunk
  immediately, regardless of its normal cadence.

## How to run one chunk

1. Read this file and applicable repository instructions. Use `$audit-rotation`
   for the next due chunk, `$audit-rotation run T1-01` for an explicit chunk, or
   `$audit-rotation status` for a read-only status check. These are coding-agent
   skill invocations, not shell commands or GitHub Actions. Without that skill,
   follow these steps manually and update the same record.
2. Check the selected baseline resolves to a commit. With a valid baseline,
   inspect `git log <baseline>..HEAD -- <paths>` and staged, unstaged and untracked
   changes. With no valid baseline, review the full chunk. Skip only when unchanged
   and not due, recording "re-verified unchanged" without changing its audit date
   or baseline. Examine each row at most once per invocation.
3. Review correctness, security, resource bounds, error handling and the row's
   focus. Include relevant tests and run checks appropriate to the review from
   [CONTRIBUTING.md](../CONTRIBUTING.md). Inspect relevant advisories when reviewing
   dependencies. Passing tests alone is not an audit.
4. Report findings by severity before fixing; honor any fixes already authorized.
   Use the normal branch/PR/review flow. Record pending findings even when fixes
   await a decision; handle sensitive security reports privately.
5. Record the actual reviewed commit in **Baseline**, review date in **Last pass**,
   and `clean` or `open-findings` in **Status**. Disclose uncommitted coverage
   separately; do not imply it is represented by the commit. Never mark an
   unreviewed chunk clean. Log notable results and advance **Next up**.

## Findings log

Completed results are recorded below. A clean pass means no unresolved findings within the reviewed scope; it is not a whole-repository security guarantee.
Use `Date | Chunk | Severity | Summary | PR` rows for subsequent results. Record
clean passes as such with severity `none`; reference private reports only in a
way that does not disclose sensitive details.

| Date | Chunk | Severity | Summary | PR |
|---|---|---|---|---|
| 2026-09-20 | all | none | Rotation configured; first passes pending. This is setup, not a clean audit result. | — |
| 2026-09-21 | T1-01 | medium (fixed) | Reviewed client/agent authentication, origin/proxy trust, credential transports/storage and reconnect failures. Corrected forwarded-chain client selection; backend vet/race suite, 15 frontend unit tests and 34 browser tests passed. | [#13](https://github.com/EMOEMOJAI/Parallax/pull/13) |
| 2026-09-21 | T1-02 | medium (fixed) | Reviewed anonymous route policy, shared runs, metrics, rate limits, metadata and map escaping. Restricted metadata connections and redirects, propagated cancellation, and disabled metrics caching. Backend vet/race suite and synthetic regressions passed. Added metadata client to the scope. | [#14](https://github.com/EMOEMOJAI/Parallax/pull/14) |
| 2026-09-21 | T1-03 | medium (fixed) | Reviewed command builders, allowlists, option bounds, subprocess/PTY cleanup, shell ownership and UI. Duplicate agent requests now preserve active streams, including disabled-shell refusals. Agent vet/race suite passed with real-WebSocket regression. | [#15](https://github.com/EMOEMOJAI/Parallax/pull/15) |
| 2026-09-21 | T1-04 | medium (fixed) | Reviewed guards/hooks, credentials, logs and artifact privacy. Guard now checks staged blobs and broken symlinks; command logs omit targets; credential-container exclusions added. 14 guard tests, both Go vet/race suites, 26 deployment tests and secret/identity scans passed. Added command logging to scope. | [#16](https://github.com/EMOEMOJAI/Parallax/pull/16) |
