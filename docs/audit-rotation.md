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
audit coverage for the chunks below. Baselines were therefore initialized to `none`.
This does not discard prior fixes or claim that no earlier reviews occurred.

All 12 chunks were initially due as of setup. The tables below track completed passes.
After squash-merging PRs #13–#24, each baseline was remapped to its merge
revision after verifying the complete tree matched the audited PR head. The
refresh in PR #28 subsequently recorded intermediate branch commits;
the reconciliation below restores main-history baselines for those rows.
Dates become meaningful after each recorded pass; do not use setup or CI run
dates as audit dates. There is no separate frozen or legacy source directory;
compatibility and rollback behavior belong to their active subsystem chunks.
A pre-existing untracked social-preview image was inspected separately and left
out of these PRs; it is not represented by the recorded commit baselines.

## Next up

**T1-01** — next regular review due **2026-12-21**, followed by the remaining
Tier 1 chunks. Tier 2 is next due **2027-03-21**. Tier 3 remains on touch;
any cadence override below can make a chunk due earlier.

All 12 chunks were reviewed again on 2026-09-21, starting at
`6f1cc5ff1a9098949854fa933be89faae54eac39`. Six findings were fixed, with no
unresolved findings in the reviewed scope. Each row points to its reviewed
source revision or the verified merge equivalent
described below. The 2026-09-23 reconciliation is bookkeeping, not another full
pass: all **Last pass** dates remain 2026-09-21. Later diff reviews are recorded
separately and do not restart the regular review clock.

## Reconciliation evidence — 2026-09-23

Nine refresh baselines were outside main history. Their replacement is PR #28's
merge, `0e01aa232e79ea4470f5761599b8cb3c084eb356`. Its complete tree equals the
reviewed final PR head, `f493757d27cbf315302a309888eeec9e3f4feb50`. Scoped Git
comparisons establish the following mapping; this is not a claim that every
intermediate commit had the same complete tree.

| Chunks | Original reviewed baseline | Evidence for replacement |
|---|---|---|
| T2-01, T2-02 | `2797cef956670cf40f79187a4f2567a46b28c743` | All listed scoped files identical at the merge. |
| T2-03 | `242ffa5983982335dcb5fab5239d6b29e7ba0208` | All listed scoped files identical at the merge. |
| T2-04, T2-05, T2-06 | `688ee35a2f9397aa882ceef89304c149a00ec07d` | All listed scoped files identical at the merge. |
| T3-01 | `987819e051ef146ffece25e7baef99bce3a8da56` | All listed scoped files identical at the merge. |
| T1-04 | `b812905479b6abd2303a4cc4295e01b313b66445` | Only `scripts/check-privacy.py` and its tests differ: the IPv6 punctuation fix covered by the final PR #28 diff audit logged below. |
| T3-02 | `987819e051ef146ffece25e7baef99bce3a8da56` | Only this audit record differs; those differences record scope, coverage and findings. Other listed documentation is identical. |

The three already reachable Tier 1 baselines remain unchanged. All 12 baseline
commits are now ancestors of main and available in a fresh full clone.

### Follow-up coverage after the full passes

[PR #28](https://github.com/EMOEMOJAI/Parallax/pull/28)'s final diff review covers
the later scheduler changes shared with T1-03 (failed-exit classification and
blank-target rejection), as well as the privacy fix above. This does not imply
a second full T1-03 pass.

[PR #29](https://github.com/EMOEMOJAI/Parallax/pull/29)'s audited final head,
`354773970def41b4a334661b658207eb28a38e8b`, has the same complete tree as its merge,
`424e58720f0ff5c0cbb8696c087d818a2ebc7689`. Its 2026-09-22 diff audit covers:

- T1-01, T1-02, T2-02 and T2-04: changed hook dependencies, credential refresh,
  reconnect, map lookup and terminal lookup cleanup in their calling context.
- T1-04 and T2-06: artifact privacy inspection, workflow changes, ESLint and
  added dependencies; both artifact-inspection findings were fixed and retested.
- T2-01 and T2-04: new fuzz targets and the parsing/address-policy functions
  they exercise; this was not a new whole-module audit.
- T3-01 and T3-02: the changed dashboard components, browser fixtures/smoke
  cases, configuration and contributor/architecture/tool guidance.

The PR records 230 backend and 173 agent top-level tests, four bounded fuzz
runs, 34 frontend unit tests, 95 browser/visual tests and 23 privacy tests passing.
OSV checks covered the 114 added package entries; unchanged dependencies were
outside that diff audit. CI and CodeQL also passed on the merged revision.

Every tracked scoped delta from the full-pass baselines through `424e587` maps
to these recorded reviews. No uncovered tracked source delta was identified by
this reconciliation; it does not establish a new full-chunk clean pass. This
bookkeeping edit and the pre-existing untracked social-preview image are not
represented by those commits. The image remains excluded from publication.

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
| T1-01 | Authentication and browser credentials | `backend/main.go`, `backend/middleware.go`, `backend/client_ws.go`, `backend/agent_ws.go`, `agent/main.go`, `frontend/src/lib/api.js`, `frontend/src/components/KeyPrompt.jsx`, `frontend/src/App.jsx`, `frontend/src/hooks/useWebSocket.js` | Client/agent identity, route authorization, origin/proxy trust, credential transport/storage, authentication failures and reconnects. | 6f1cc5ff1a9098949854fa933be89faae54eac39 | 2026-09-21 | clean |
| T1-02 | Public endpoints and data exposure | `backend/public.go`, `backend/ops.go`, `backend/ratelimit.go`, `backend/nodes.go`, `backend/runs.go`, `backend/geoip.go`, `backend/metadata_client.go`, `backend/rdap.go`, `backend/speedtest.go`, `backend/alerts.go`, `backend/main.go`, `backend/client_ws.go`, `frontend/src/components/GeoMap.jsx` | Anonymous access, target/command restrictions, metrics and shared-run disclosure, outbound request boundaries, rate limits and HTML escaping. | 6f1cc5ff1a9098949854fa933be89faae54eac39 | 2026-09-21 | clean |
| T1-03 | Command execution and interactive shells | `agent/main.go`, `agent/commands.go`, `agent/shell.go`, `backend/main.go`, `backend/client_ws.go`, `backend/command_cleanup.go`, `backend/scheduler.go`, `frontend/src/components/ShellTerminal.jsx` | Allowlists, argv/options validation, shell authorization and opt-out, privileges, terminal input/output limits, cancellation and process cleanup. | 6f1cc5ff1a9098949854fa933be89faae54eac39 | 2026-09-21 | clean |
| T1-04 | Secrets and publication privacy | `scripts/check-privacy.py`, `scripts/check-artifact-privacy.py`, `scripts/check-commit-privacy.py`, `scripts/install-hooks.py`, `scripts/tests/`, `.githooks/`, `.gitignore`, `.dockerignore`, `.github/workflows/`, `backend/main.go`, `backend/middleware.go`, `backend/client_ws.go`, `agent/main.go`, `deploy/`, `tests/integration/` | Secret/config handling and log redaction; tracked files, commit metadata, package/image contents and CI artifacts must not disclose credentials or deployment identities. | 0e01aa232e79ea4470f5761599b8cb3c084eb356 | 2026-09-21 | clean |

## Tier 2 chunks

| ID | Chunk | Paths | Focus | Baseline | Last pass | Status |
|---|---|---|---|---|---|---|
| T2-01 | Native probes and network address policy | `agent/probes.go`, `agent/dnsbench.go`, `agent/main.go`, `backend/metadata_client.go` | DNS rebinding, IPv4/IPv6 and local-address policy, redirects, TLS diagnostic verification, DNS parsing, timeouts, byte limits and cancellation. | 0e01aa232e79ea4470f5761599b8cb3c084eb356 | 2026-09-21 | clean |
| T2-02 | WebSockets and request lifecycle | `backend/main.go`, `backend/agent_ws.go`, `backend/client_ws.go`, `backend/command_cleanup.go`, `backend/nodes.go`, `agent/main.go`, `agent/commands.go`, `agent/probes.go`, `agent/shell.go`, `frontend/src/hooks/useWebSocket.js`, `frontend/src/hooks/useNodes.js`, `frontend/src/lib/id.js`, `frontend/src/lib/nodes.js` | Serialized writes, lock order, backpressure, reconnect/shutdown, request ownership, stale cancellation/output, ID reuse and mixed-version compatibility. | 0e01aa232e79ea4470f5761599b8cb3c084eb356 | 2026-09-21 | clean |
| T2-03 | Schedules, saved runs and latency mesh | `backend/main.go`, `backend/scheduler.go`, `backend/runs.go`, `backend/mesh.go`, `backend/alerts.go`, `backend/ops.go`, `frontend/src/components/Schedules.jsx`, `frontend/src/components/LatencyMatrix.jsx`, `frontend/src/hooks/useInvestigations.js`, `frontend/src/lib/investigations.js`, `frontend/src/components/Investigations.jsx` | Persistence/migrations, restart recovery, retention, overlapping runs, mesh concurrency, alert delivery and sensitive state in metrics. | 0e01aa232e79ea4470f5761599b8cb3c084eb356 | 2026-09-21 | clean |
| T2-04 | Untrusted output and metadata parsing | `backend/summary.go`, `backend/util.go`, `backend/geoip.go`, `backend/metadata_client.go`, `backend/rdap.go`, `backend/speedtest.go`, `agent/summary.go`, `agent/testdata/`, `frontend/src/lib/`, `frontend/src/components/OutputTerminal.jsx`, `frontend/src/components/SummaryBadges.jsx`, `frontend/src/components/GeoMap.jsx` | Malformed/oversized external data, UTF-8, control characters, summary-before-done ordering, parser resource limits, text rendering and capability fallbacks. | 0e01aa232e79ea4470f5761599b8cb3c084eb356 | 2026-09-21 | clean |
| T2-05 | Installation, updates and runtime packaging | `deploy/`, `Dockerfile.server`, `Dockerfile.agent`, `docker-compose.yml`, `.dockerignore`, `scripts/package-release.py`, `tests/integration/`, `docs/deployment.md` | Verified downloads, clone/update trust, filesystem ownership, service permissions, rollback recovery, configuration preservation and release archive behavior. | 0e01aa232e79ea4470f5761599b8cb3c084eb356 | 2026-09-21 | clean |
| T2-06 | Dependencies, build and hosted CI | `.github/`, `backend/go.mod`, `backend/go.sum`, `agent/go.mod`, `agent/go.sum`, `frontend/package.json`, `frontend/package-lock.json`, `frontend/vite.config.js`, `frontend/playwright.config.js`, `frontend/eslint.config.js`, `tests/integration/requirements.txt`, `scripts/package-release.py`, `Dockerfile.server`, `Dockerfile.agent` | Vulnerabilities and upgrades, pinned actions/tools, workflow permissions and untrusted PR input, test coverage, artifact provenance and release gates. | 0e01aa232e79ea4470f5761599b8cb3c084eb356 | 2026-09-21 | clean |

## Tier 3 chunks

| ID | Chunk | Paths | Focus | Baseline | Last pass | Status |
|---|---|---|---|---|---|---|
| T3-01 | Dashboard behavior and accessibility | `frontend/src/`, `frontend/tests/`, `frontend/index.html`, `frontend/public/` | Remaining UI behavior, local history/kits/presets, subscriptions, accessibility, rendering bounds and error recovery. Trigger: change to UI behavior, browser storage, styles, assets or frontend tests; retain higher-tier security reviews where applicable. | 0e01aa232e79ea4470f5761599b8cb3c084eb356 | 2026-09-21 | clean |
| T3-02 | Documentation and tool guidance | `docs/`, `README.md`, `CONTRIBUTING.md`, `SECURITY.md`, `AGENTS.md`, `CLAUDE.md`, `GEMINI.md`, `llms.txt`, `LICENSE`, `.github/copilot-instructions.md` | Accurate defaults, setup/recovery instructions, safe examples, links and consistent coding-tool guidance. Trigger: documentation/branding changes or code changes affecting documented configuration, protocols, installation or recovery. | 0e01aa232e79ea4470f5761599b8cb3c084eb356 | 2026-09-21 | clean |

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
   changes. Consult the follow-up coverage above when determining whether a
   changed area already has a scoped review; a diff review does not reset the
   full-pass baseline or cadence date. With no valid baseline, review the full
   chunk. Skip only when unchanged
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
| 2026-09-21 | T2-01 | medium/low (fixed) | Reviewed native probes, DNS wire parsing, TLS verification and address policy. Fixed final DNS cancellation and bounded resolver reads; completed metadata IPv6/on-link checks and added that policy to scope. Both Go vet/race suites passed; DNS row test now uses local fixtures. | [#17](https://github.com/EMOEMOJAI/Parallax/pull/17) |
| 2026-09-21 | T2-02 | medium/low (fixed) | Reviewed ownership, serialized writes, cancellation, ID reuse, reconnects and shutdown. Queued node broadcasts now read current state and reconnects clear empty metadata; command tests ignore independent broadcasts. Backend race suite, repeated lifecycle regressions, frontend unit tests/build and privacy checks passed. | [#18](https://github.com/EMOEMOJAI/Parallax/pull/18) |
| 2026-09-21 | T2-03 | medium/low (fixed) | Reviewed persistence/migrations, run lifecycle, history and saved-run retention, mesh concurrency, alerts and UI. Failed schedule loads now preserve the file and reject requests; failed mesh results are not recorded; output separators respect the cap. Backend vet/race suite and privacy checks passed. Added server recovery state to scope. | [#19](https://github.com/EMOEMOJAI/Parallax/pull/19) |
| 2026-09-21 | T2-04 | medium/low (fixed) | Reviewed metadata, summaries, agent parsers and output rendering. Added provider shape/size/identity checks and sanitized-key collision rejection; numeric badges and hop links reject malformed values. Backend vet/race suite, frontend unit/build, 34 browser tests and privacy checks passed. | [#20](https://github.com/EMOEMOJAI/Parallax/pull/20) |
| 2026-09-21 | T2-05 | medium (fixed) | Reviewed installer/update trust, downloads, service permissions, config preservation, rollback and runtime/release packaging. Schedule migration now atomically replaces runtime destinations without following symlinks. All 28 deployment tests, ShellCheck and privacy checks passed. | [#21](https://github.com/EMOEMOJAI/Parallax/pull/21) |
| 2026-09-21 | T2-06 | low (fixed) | Reviewed dependencies, workflow trust/permissions, hosted runners, build configuration, container scans and release gates/provenance. Added Python vulnerability scanning and artifact hashes. Go/npm/Python scans and Dependabot alerts were clean; no dependency updates found. Hash-checked install, actionlint, guard tests and privacy checks passed. | [#22](https://github.com/EMOEMOJAI/Parallax/pull/22) |
| 2026-09-21 | T3-01 | low (fixed) | Reviewed dashboard flows, storage, subscriptions, focus, accessibility, rendering and recovery. Fixed disabled-fieldset focus wrapping, cross-tab clearing, dropdown semantics and offline comparison selections. Frontend unit/build, all 37 browser tests and privacy checks passed. | [#23](https://github.com/EMOEMOJAI/Parallax/pull/23) |
| 2026-09-21 | T3-02 | low (fixed) | Reviewed all documentation, tool guidance, examples, branding and links against current behavior. Corrected build/version guidance, auth/defaults, external-tool requirements and recovery placement. Reference tests, 51 local links and privacy checks passed; hosted protections and map policy verified. | [#24](https://github.com/EMOEMOJAI/Parallax/pull/24) |
| 2026-09-21 | T1-01 | none | Refresh: Reviewed route authorization, credential precedence, client/agent handshakes, origin/proxy trust, tab/browser storage, sign-out remount and stale rejection handling with authentication regressions. No new findings. | [#28](https://github.com/EMOEMOJAI/Parallax/pull/28) |
| 2026-09-21 | T1-02 | none | Refresh: Reviewed public-session allowlists and quotas, privileged route guards, permalink creation/read boundaries, metadata destination validation, provider request bounds, metrics authentication and Leaflet escaping. No new findings. | [#28](https://github.com/EMOEMOJAI/Parallax/pull/28) |
| 2026-09-21 | T1-03 | none | Refresh: Reviewed command dispatch/ownership, allowlist synchronization, option grammars and argv builders, duplicate-ID reservations, PTY bounds, cancellation and cleanup with lifecycle/argv tests. Shell defaults preserved; no new findings. | [#28](https://github.com/EMOEMOJAI/Parallax/pull/28) |
| 2026-09-21 | T1-04 | medium/low (fixed) | Refresh: Reviewed staged/worktree and commit guards, hooks, logging, build contexts and artifact contents. Fixed named environment-file omissions and private IPv6 inventory detection; 16 guard tests and privacy scan passed. Docker context exclusion verified separately. | [#28](https://github.com/EMOEMOJAI/Parallax/pull/28) |
| 2026-09-21 | T2-01 | medium (fixed) | Refresh: Reviewed native probe address admission and pinned dialing, TLS diagnostics, DNS wire parsing, response bounds and cancellation. Fixed fail-open interface enumeration and verified recovery plus explicit private-target override. Agent vet/race suite and reference gate passed. | [#28](https://github.com/EMOEMOJAI/Parallax/pull/28) |
| 2026-09-21 | T2-02 | none | Refresh: Reviewed serialized writers, connection retirement, ownership revalidation, ID reuse, exec/native/PTY cleanup, bounded output and node snapshot reconciliation. Lifecycle regression tests cover cancellation and reconnect interleavings; no new findings. | [#28](https://github.com/EMOEMOJAI/Parallax/pull/28) |
| 2026-09-21 | T2-03 | medium/low (fixed) | Refresh: Reviewed schedule persistence/recovery, history and permalink retention, mesh worker lifetimes, alert delivery, browser baselines and incident snapshots. Fixed failed exits being classified as degraded and rejected blank schedule targets. Backend vet/race, frontend unit/build and eight targeted browser checks passed. | [#28](https://github.com/EMOEMOJAI/Parallax/pull/28) |
| 2026-09-21 | T2-04 | none | Refresh: Reviewed summary grammars, bounded agent parsers, provider response validation, output/RDAP rendering, hop detection, literal redaction and CSV/export contracts with malformed-data and privacy regressions. No new findings. | [#28](https://github.com/EMOEMOJAI/Parallax/pull/28) |
| 2026-09-21 | T2-05 | none | Refresh: Reviewed install/update trust, quoting, checksum downloads, ownership, atomic replacement and rollback, release archive contents and disposable-container protocol checks. Deployment tests, ShellCheck and Compose validation passed. No new findings; full image runtime gates also run in hosted PR CI. | [#28](https://github.com/EMOEMOJAI/Parallax/pull/28) |
| 2026-09-21 | T2-06 | none | Refresh: Reviewed lockfiles and registry integrity, toolchain pins, CI permissions/events, native archive checks and release provenance gates. Go/npm/Python vulnerability scans and Dependabot alerts were clear; registry checks found no updates. Actionlint and current-branch secret/commit guards passed. Both production images built and passed runtime integration plus HIGH/CRITICAL vulnerability scans. | [#28](https://github.com/EMOEMOJAI/Parallax/pull/28) |
| 2026-09-21 | T3-01 | low (fixed) | Refresh: Reviewed dashboard state, bounded rendering, subscriptions, focus and dismissal, motion preferences, storage and recovery with browser/accessibility regressions. Fixed cross-tab command-history deletion so cleared entries cannot be restored by another tab after synchronization. All 34 unit tests, 76 browser tests, production build and 10 Linux visual comparisons passed. | [#28](https://github.com/EMOEMOJAI/Parallax/pull/28) |
| 2026-09-21 | T3-02 | none | Refresh: Reviewed all guides, configuration and protocol contracts, tool instructions, examples, local links and tracked branding/screenshots against source and hosted protections. No new findings. Checked 41 local link targets/anchors; private reporting is enabled and no repository self-hosted runners are registered. Banner metadata contains only dimensions/color space and empty Photoshop bookkeeping; screenshots use synthetic data. | [#28](https://github.com/EMOEMOJAI/Parallax/pull/28) |
| 2026-09-21 | T1-04 | medium (fixed) | PR diff review: fixed IPv6 sentence punctuation bypassing deployment privacy detection; synthetic prose regressions cover ULA, link-local and mapped addresses. All 17 privacy tests passed. This follow-up is included in the reconciled PR #28 merge baseline; the full-pass date is unchanged. | [#28](https://github.com/EMOEMOJAI/Parallax/pull/28) |
| 2026-09-22 | T1-04 | medium (fixed) | PR #29 diff audit: closed extensionless/config text and post-scan JPEG metadata inspection gaps; all 23 privacy tests passed. Diff coverage only; full-pass cadence unchanged. | [#29](https://github.com/EMOEMOJAI/Parallax/pull/29) |
| 2026-09-22 | affected chunks listed above | none | PR #29 follow-up coverage: reviewed changed hooks, browser tests, fuzz targets, dependencies, workflows and guidance in context; no remaining diff findings. Full-pass baselines/dates are not advanced to this PR. | [#29](https://github.com/EMOEMOJAI/Parallax/pull/29) |
| 2026-09-23 | audit record | low (fixed) | Reconciled nine branch-only baselines to verified PR #28 merge coverage and recorded PR #29's diff review. Preserved all full-pass dates/statuses and the regular Next up pointer; no new whole-chunk audit claimed. | — |
