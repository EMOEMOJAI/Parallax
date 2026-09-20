<p align="center">
  <img src="docs/brand/parallax-lockup.jpg" alt="Parallax — network diagnostics from every vantage point" width="960">
</p>

# Parallax

A self-hosted **network looking glass**. Compare connectivity across servers,
regions and providers from one live dashboard.

[![CI](https://github.com/EMOEMOJAI/Parallax/actions/workflows/ci.yml/badge.svg)](https://github.com/EMOEMOJAI/Parallax/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

[Get started](docs/getting-started.md) · [Deploy](docs/deployment.md) · [Reference](docs/reference.md) · [Contribute](CONTRIBUTING.md)

- **Find the problem:** ping, traceroute, MTR, DNS, TLS and bandwidth tests across remote agents.
- **Watch your network:** scheduled checks, a latency mesh, webhook alerts and Prometheus metrics.
- **Explore together:** route maps, shared results, reusable diagnostic kits and interactive terminals.

## Try it locally

With Docker and Docker Compose installed:

```sh
git clone https://github.com/EMOEMOJAI/Parallax.git
cd Parallax
docker compose up --build
```

Open **http://localhost:8080**. The server and an example agent are ready to explore.
Add remote agents with the [deployment guide](docs/deployment.md).

Before exposing an installation, configure client and agent keys and allowed
origins in the [deployment guide](docs/deployment.md). Interactive shells are
enabled by default; agents can disable them with `-allow-shell=false`.

[Architecture](docs/architecture.md) · [AI tools](AGENTS.md) · [Docs index](llms.txt) · [MIT license](LICENSE)
