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
