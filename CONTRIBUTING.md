# Contributing to Parallax

Start with [Getting started](docs/getting-started.md). The repository has two
independent Go modules and one frontend package; run commands in the appropriate
directory. [Architecture](docs/architecture.md) explains protocol and compatibility
constraints, and [AGENTS.md](AGENTS.md) provides shared guidance for coding tools.

## Check a change

Run the checks for the areas you touched:

```sh
(cd backend && go vet ./... && go test -race ./...)
(cd agent && go vet ./... && go test -race ./...)
(cd frontend && npm run lint && npm test && npm run build)
python3 -m unittest discover -s deploy/tests
shellcheck deploy/*.sh
git diff --check
```

Run `npm ci` inside `frontend/` first if dependencies are not installed. Use
`gofmt` on Go changes. Add regression coverage for behavior changes; include a
screenshot for visible UI changes when useful. Keep configuration and API changes
documented in [the reference](docs/reference.md).

## Describe the problem

For issues, include the expected behavior, what happened, reproduction steps,
OS and relevant server/agent versions. Redact API keys, private addresses and
deployment details from logs and screenshots.

For pull requests, explain the user-visible change and how you verified it.
Call out protocol, persisted-data or deployment migrations explicitly. Keep
unrelated cleanup separate from functional changes.

## Continuous integration

All workflows run on disposable GitHub-hosted runners. Pull requests run Go race
and vulnerability checks, frontend unit tests and production browser regressions,
accessibility checks, deployment tests, workflow lint, secret/privacy scanning,
real server/agent container integration, image vulnerability scanning, and native
amd64/arm64 release archive tests. CodeQL analyzes Go, JavaScript and
workflow code. CI and CodeQL also run weekly; Dependabot proposes grouped updates.
Security updates and dependency PRs still require review and passing checks.

To run the browser checks locally:

```sh
cd frontend
npm ci
npx playwright install chromium
npm run build
npm run test:e2e -- --project=production --project=source-auth
```

Visual regression checks cover login, desktop, mobile, the mobile tools menu,
mobile output, the share preview, mobile comparisons, unavailable links,
the investigations workspace and agent readiness.
Run them from the repository root in the same pinned Linux browser container as
GitHub CI (the anonymous dependency volume keeps Linux packages off your host):

```sh
docker run --rm --platform linux/amd64 --ipc=host -v "$PWD:/work" -v /work/frontend/node_modules -w /work/frontend \
  mcr.microsoft.com/playwright:v1.63.0-noble@sha256:eff16c30e6f3f4af0a03fa4b706120d5e9b0891c344a27d64559aff5900a4a27 \
  sh -c 'npm ci && npm run build && npm run test:e2e -- --project=visual'
```

For intentional visual changes, append `--update-snapshots` to the Playwright
command inside the container, inspect every changed image in
`frontend/tests/e2e/visual-snapshots/`, and commit the reviewed baselines. CI only
compares; it never updates baselines. Use synthetic data only, never a live
deployment or real credentials. Keep the container version aligned with the
locked Playwright dependency when updating it. Run host and container checks
sequentially because they share build output and browser reports.

The suite uses synthetic HTTP/WebSocket fixtures against the production bundle;
authentication race cases also use the Vite source server. Container tests
exercise the actual server and unprivileged agent, authentication, metrics access,
PTY sizing, UTF-8 output and request ID reuse:

```sh
docker build -f Dockerfile.server -t parallax-server:ci .
docker build -f Dockerfile.agent -t parallax-agent:ci .
python3 -m venv /tmp/parallax-tests
/tmp/parallax-tests/bin/pip install --require-hashes -r tests/integration/requirements.txt
/tmp/parallax-tests/bin/python tests/integration/containers.py
```

Tests create disposable credentials and containers, remove containers on exit,
and save redacted logs under ignored `test-results/`. Browser failure artifacts
are retained for seven days. No deployment hosts or repository secrets are needed. Trivy scans the actual
production images for high/critical OS and language-package vulnerabilities;
findings fail the container job. CycloneDX software bills of materials and JSON
scan reports are retained as GitHub artifacts for 14 days. Scan failures are not
silently ignored; investigate and update affected packages before merging.

Run `python3 scripts/check-privacy.py` before committing. It checks both staged blobs and tracked working files
for credential filenames, private key material, personal home paths, non-example
email addresses and private deployment addresses/domains. It complements secret
scanning and manual review; it cannot detect every personal detail. Network policy
tests may contain synthetic private addresses. Do not add real deployment markers
to a public denylist, since the denylist would disclose those markers itself.

## Additional CI checks

Frontend lint runs `npm run lint` with JavaScript correctness, hook-order and
hook-dependency rules. Fix dependencies without discarding deliberate credential
refresh/reconnect triggers. Node 24 is the CI toolchain; development also supports
Node 22.13 or later in the 22.x line.

Firefox and WebKit run a focused smoke suite for login, native WebSocket output,
PTY rendering, dialog focus and cross-tab storage. Chromium also runs those
cases alongside the full existing suite. All browser data is synthetic. Run:

```sh
cd frontend
npx playwright install --with-deps firefox webkit
npm run build
npm run test:e2e -- --project=firefox --project=webkit
```

Both Go modules fuzz pure parsing/validation functions without network calls.
Every fuzz target runs for 10 seconds with two workers on PRs and ordinary CI;
the weekly run uses 120 seconds per target. Seed cases also run in the normal
race suite. Failures retain synthetic reproducers for seven days; review and
commit useful minimized cases. For example:

```sh
(cd backend && go test -run '^$' -fuzz '^FuzzValidateSummary$' -fuzztime 10s -parallel 2 .)
(cd agent && go test -run '^$' -fuzz '^FuzzDNSAnswers$' -fuzztime 10s -parallel 2 .)
```

Artifact privacy checks inspect complete release archives, `/app` payloads and
configuration of production images (including earlier application layers and
build history), and archive owner/extended metadata. They
reject credential filenames, personal build paths, private keys, identifying
text and unapproved PNG/JPEG metadata. OS packages outside `/app` remain covered
by the image vulnerability scan. A scratch Docker build verifies real context
exclusion using synthetic credential files; it never reads production secrets.

```sh
python3 scripts/check-artifact-privacy.py --archive release-dist/example.tar.gz
python3 scripts/check-artifact-privacy.py --check-context --image parallax-server:ci --image parallax-agent:ci
```

These are bounded checks, not proof that every possible identifying detail is
absent. All jobs use GitHub-hosted runners and need no homelab credentials.

## Recurring audits

[Audit rotation](docs/audit-rotation.md) records review scope, cadence, findings
and the next area to inspect. Review one chunk per session: authentication,
public access, command execution and secrets quarterly; parsers, runtime state,
deployment and dependencies every six months; remaining UI/docs when changed.
Missing baselines require a first pass. Relevant changes and security advisories
trigger earlier review. This is a maintainer-run process alongside existing CI;
record the reviewed revision and evidence without publishing private deployment
details or sensitive vulnerability reports.

## Commit privacy before pushing

Install the local guard in every clone before your first push:

```sh
python3 scripts/install-hooks.py
python3 scripts/check-commit-privacy.py
```

The installer covers all linked worktrees, preserves unrelated existing hooks,
and keeps its checker outside the source tree so older worktrees cannot silently
skip it. Re-run the installer after updating the hook source. Configure Git's
`user.name` to your public handle and `user.email` to your GitHub-provided noreply
address. Keep GitHub's **Keep my email addresses private** and **Block command line
pushes that expose my email** settings enabled.

The pre-push hook checks author and committer emails in every commit reachable
from the pushed refs, plus annotated tagger emails. It allows GitHub noreply
identities, handles new branches and deletions, and rejects incomplete shallow
history. Failures identify the object and field without printing the email.
CI repeats the check, but only the local hook/account protections run before
publication. Hooks are local and can be bypassed; they do not detect personal
names, text inside commit messages, or every other identifying detail.

## Releases

Run the **Release** workflow manually to test packaging without publishing. It
runs CI, including Linux amd64 and arm64 archive builds on their native GitHub
runners. Both PRs and releases extract and run the exact archives before uploading
them, checking the dashboard assets, authentication, agent version and shell
protocol. The publication job uses those tested archives without rebuilding.
Server archives include the dashboard; run the server from the extracted directory. Agents still
need the external diagnostic tools described in the deployment guide.

To publish, tag a tested commit on `main` with `vMAJOR.MINOR.PATCH` and push that
tag. All CI checks and native archive tests must pass before publication. Tags
with a suffix, such as
`v1.2.3-rc.1`, create prereleases. Published releases include `SHA256SUMS` and GitHub
build provenance. Verify downloaded archives with `sha256sum -c SHA256SUMS` and
`gh attestation verify <archive.tar.gz> --repo EMOEMOJAI/Parallax`.

`main` requires the CI and CodeQL checks and a pull request, without a mandatory
independent approval. Force pushes and deletion are blocked. Repository admins
retain an emergency bypass; normal changes should use pull requests.
