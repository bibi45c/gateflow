# GateFlow

FlowGate is a scalable API gateway for local AI coding tools. Developers point a
coding tool at the gateway with a gateway key, and FlowGate checks access and quota,
routes the request to an approved model channel, streams the output back and records
usage. This repository holds the SWE5001 Team 1 implementation. The current work is
the feasibility test in [docs/feasibility-plan.md](docs/feasibility-plan.md).

## Start here

- People: [.github/CONTRIBUTING.md](.github/CONTRIBUTING.md). The directory layout is under
  [Code layout](AGENTS.md#code-layout).
- Coding agents: [AGENTS.md](AGENTS.md). Claude Code loads it through
  [.claude/CLAUDE.md](.claude/CLAUDE.md).
- Checks and evidence: [docs/testing.md](docs/testing.md).

## Setup on macOS and Windows

1. Install Git, the GitHub CLI (`gh`), Python 3.13 or newer, Go (the version in
   `go.mod`) and Docker Desktop. On Windows, Docker Desktop needs WSL 2.
2. On Windows, enable long paths:

   ```text
   git config --global core.longpaths true
   ```

3. Clone the repository:

   ```text
   git clone https://github.com/bibi45c/gateflow.git
   ```

4. From the repository root, run the checks (on macOS, `python3` if `python` is not
   Python 3):

   ```text
   python -m unittest discover -s tools/repo/tests -v
   go vet ./...
   go test ./...
   go build ./...
   ```

5. Start the local stack: PostgreSQL, Redis, the three services and the mock
   upstream. Each process answers `GET /healthz`: the gateway on port 8080, access on
   8081, metering on 8082 and the mock upstream on 8090.

   ```text
   docker compose -f deploy/compose/compose.yaml up --build
   ```

   Stop it and remove its data with:

   ```text
   docker compose -f deploy/compose/compose.yaml down --volumes
   ```

   If a port is taken or reserved, put other ports in `deploy/compose/.env`, which
   Git ignores, for example `GATEWAY_PORT=18080`. The variables are `GATEWAY_PORT`,
   `ACCESS_PORT`, `METERING_PORT`, `MOCK_UPSTREAM_PORT`, `POSTGRES_PORT`,
   `REDIS_PORT`, `CONSOLE_PORT` and `PROMETHEUS_PORT`. To see the reserved ranges on
   Windows:

   ```text
   netsh interface ipv4 show excludedportrange protocol=tcp
   ```

   On macOS, `lsof -i :8080` shows what holds a port.

## Web console

The local stack also runs the console, on port 8070 (`CONSOLE_PORT`), and a
Prometheus for its dashboard, on 9090 (`PROMETHEUS_PORT`). Open
`http://localhost:8070` and sign in with a local-only account the seed creates:

| User | Password | Sees |
|---|---|---|
| `demo` | `demo-local-only` | Tenant `tenant-demo`: balance and today's usage, API keys (create, shown once, revoke), the usage log, models and prices, quick start for curl, Codex CLI and OpenCode, and a playground that streams a call through the gateway |
| `admin` | `admin-local-only` | Every tenant with its balance and a top-up or correction form, channels and prices (read-only), and a dashboard of live requests per second, active streams and gateway replicas from Prometheus |

A short tour: sign in as `demo`, create a key, send the playground's default
`mock-chat` (a readable answer streamed at about 28 tokens a second), and watch the
call appear under Usage. Then sign in as
`admin`, top up `tenant-demo`, and open the dashboard while a load test runs (see
[loadtest/README.md](loadtest/README.md)). The console keeps no data of its own; it
calls the internal APIs of access and metering ([api/](api)). Screenshots are in
[docs/console](docs/console). On Helm the console is off by default
(`console.enabled`); it is a ClusterIP Service, reached with `kubectl port-forward`.
