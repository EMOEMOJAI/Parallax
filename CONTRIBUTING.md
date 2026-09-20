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
