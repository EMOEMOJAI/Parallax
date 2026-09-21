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
(cd frontend && npm test && npm run build)
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
npm run test:e2e
```

The suite uses synthetic HTTP/WebSocket fixtures; 20 cases exercise the production
bundle and two authentication cases use the Vite source server. Container tests
exercise the actual server and unprivileged agent, authentication, metrics access,
PTY sizing, UTF-8 output and request ID reuse:

```sh
docker build -f Dockerfile.server -t parallax-server:ci .
docker build -f Dockerfile.agent -t parallax-agent:ci .
python3 -m venv /tmp/parallax-tests
/tmp/parallax-tests/bin/pip install -r tests/integration/requirements.txt
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
