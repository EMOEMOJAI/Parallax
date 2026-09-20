# Getting started

Parallax runs a server, a browser dashboard and one agent per probe location.

## Docker Compose

Install Docker with Compose support, then run:

```sh
git clone https://github.com/EMOEMOJAI/Parallax.git
cd Parallax
docker compose up --build
```

Open <http://localhost:8080>. The example agent registers automatically. Schedule
data lives in the `schedules` named volume; `docker compose down` keeps it, while
`docker compose down --volumes` deletes it. Cloning a private repository requires
GitHub access; a public checkout does not require a deploy key.

This example has no API keys configured. Set `CLIENT_API_KEY`, `AGENT_API_KEY`
and `ALLOWED_ORIGINS` for an installation reachable by untrusted users.
See [Deployment](deployment.md) for production setup and updates.

## Local development

Use **Go 1.27.1** and **Node.js 24**, matching [CI](../.github/workflows/ci.yml).
From a checkout, run each command in a separate terminal:

```sh
# Terminal 1: server, http://localhost:8080
cd backend
go run .
```

```sh
# Terminal 2: dashboard; open the local URL printed by Vite
cd frontend
npm ci
npm run dev
```

```sh
# Terminal 3: local probe agent
cd agent
go run . -server ws://localhost:8080/ws/agent -name Local -auto-ip=false
```

Vite forwards `/api` and `/ws` to the server. Native `tcp`, `tls`, `dnsbench` and
`download` probes need no external diagnostic tools; other commands depend on
binaries installed on the agent. The UI displays each node's available tools.
Linux is the deployment target; local command availability varies by OS.

## Build from source

From the repository root:

```sh
(cd frontend && npm ci && npm run build)
(cd backend && CGO_ENABLED=0 go build -o parallax-server .)
(cd agent && CGO_ENABLED=0 go build -o parallax-agent .)
```

The server serves `frontend/dist` from the repository root, or `../frontend/dist`
when started inside `backend/`. Keep that directory with your server deployment.

Connect a remote agent to a server with TLS:

```sh
./parallax-agent -server wss://example.com/ws/agent -name Tokyo-1 -location "Tokyo, JP"
```

Set the agent's `AGENT_API_KEY` to match the server. Agents reconnect automatically
and detect their public IP by default. Interactive shells default to enabled;
pass `-allow-shell=false` to disable them on a node.

See [Reference](reference.md) for every flag and configuration variable, or
[Contributing](../CONTRIBUTING.md) for checks before submitting a change.
