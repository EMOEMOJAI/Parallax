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
and real server/agent container integration. CodeQL analyzes Go, JavaScript and
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
are retained for seven days. No deployment hosts or repository secrets are needed.

Run `python3 scripts/check-privacy.py` before committing. It checks tracked files
for credential filenames, private key material, personal home paths, non-example
email addresses and private deployment addresses/domains. It complements secret
scanning and manual review; it cannot detect every personal detail. Network policy
tests may contain synthetic private addresses. Do not add real deployment markers
to a public denylist, since the denylist would disclose those markers itself.

## Releases

Run the **Release** workflow manually to test packaging without publishing. It
runs CI, then builds Linux amd64 and arm64 agent/server archives. Server archives
include the dashboard; run the server from the extracted directory. Agents still
need the external diagnostic tools described in the deployment guide.

To publish, tag a tested commit on `main` with `vMAJOR.MINOR.PATCH` and push that
tag. CI must pass again before packaging. Tags with a suffix, such as
`v1.2.3-rc.1`, create prereleases. Published releases include `SHA256SUMS` and GitHub
build provenance. Verify downloaded archives with `sha256sum -c SHA256SUMS` and
`gh attestation verify <archive.tar.gz> --repo EMOEMOJAI/Parallax`.

`main` requires the CI and CodeQL checks and a pull request, without a mandatory
independent approval. Force pushes and deletion are blocked. Repository admins
retain an emergency bypass; normal changes should use pull requests.
