<p align="center">
  <img src="docs/brand/parallax-lockup.jpg" alt="Parallax — network diagnostics from every vantage point" width="960">
</p>

# Parallax

**Your network. Every vantage point.**

Find slow routes, compare providers, and troubleshoot from the locations that
matter. Parallax is a **self-hosted network looking glass** that brings remote
agents into one live dashboard.

[![CI](https://github.com/EMOEMOJAI/Parallax/actions/workflows/ci.yml/badge.svg)](https://github.com/EMOEMOJAI/Parallax/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

[**Get started →**](#try-it-locally) · [Download](https://github.com/EMOEMOJAI/Parallax/releases) · [Documentation](docs/getting-started.md) · [Contribute](CONTRIBUTING.md)

![Parallax dashboard showing ping results, latency summaries, and node details](docs/screenshots/dashboard.jpg)

*The real dashboard, with simulated nodes and diagnostic results.*

## One dashboard, more answers

- **Compare connections.** Run ping, traceroute, MTR, DNS, TLS and bandwidth tests across locations. See results side by side.
- **Catch recurring problems.** Schedule probes, track latency between nodes, and connect Prometheus metrics and webhook alerts.
- **Follow the evidence.** Run guided checks, compare saved baselines, and export incident reports with a redaction preview. Share results or open a terminal for a closer look.

<details>
<summary>See a side-by-side comparison across three locations</summary>

![Parallax comparing simulated ping results from Tokyo, Singapore, and Frankfurt](docs/screenshots/comparison.jpg)

*Run the same diagnostic from multiple nodes and compare the results in one view.*

</details>

## Try it locally

With Docker and Docker Compose:

```sh
git clone https://github.com/EMOEMOJAI/Parallax.git
cd Parallax
docker compose up --build
```

Open **[localhost:8080](http://localhost:8080)**. An example agent connects
automatically. Choose a node, pick a diagnostic, and run it.

Ready for more vantage points? [Deploy remote agents →](docs/deployment.md)

> Before exposing your instance, configure API keys and allowed origins using the
> [deployment guide](docs/deployment.md). Interactive shells default to enabled;
> disable them per agent with `-allow-shell=false`.

[Configuration & API](docs/reference.md) · [Architecture](docs/architecture.md) · [AI tools](AGENTS.md) · [Docs index](llms.txt) · [MIT license](LICENSE)
