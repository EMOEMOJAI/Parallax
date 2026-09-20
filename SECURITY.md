# Security

## Report a vulnerability privately

Use [GitHub private vulnerability reporting](https://github.com/EMOEMOJAI/Parallax/security/advisories/new).
Include the affected revision, impact and a minimal reproduction using synthetic
credentials and example addresses. Avoid public issues for unpatched vulnerabilities.
Never send private keys, production tokens or an inventory of your real nodes.

## Supported versions

Security fixes target the latest `main` revision and the newest release. Older
releases do not have a separate maintenance branch. There is no guaranteed
response time; reports are handled by the project maintainers.

## Deployment boundary

Parallax can run network probes and interactive shells on connected agents.
Configure server and agent authentication, use TLS, restrict network access and
only enable shells for trusted operators. See the [configuration reference](docs/reference.md)
and [deployment guide](docs/deployment.md). CI scans and passing tests do not make
an unauthenticated deployment safe.
