# GateFlow

FlowGate is a scalable API gateway for local AI coding tools. Developers point a
coding tool at the gateway with a gateway key, and FlowGate checks access and quota,
routes the request to an approved model channel, streams the output back and records
usage. This repository holds the SWE5001 Team 1 implementation. The delivery plan is
[docs/feasibility-plan.md](docs/feasibility-plan.md).

Milestones S0–S3 are delivered: the local stack, services and console; Helm on kind;
Terraform for EKS, RDS and networking in ap-southeast-1; and the S3 cloud experiments,
run on EKS on 17–18 October 2026 against the mock upstream, with results on
[issue #77](https://github.com/bibi45c/gateflow/issues/77). Milestone "S4 Real-model
validation" (due 9 November 2026) tests against a real model, vLLM serving Qwen3-0.6B
on a g4dn GPU host; its kit is merged, and running it is future work that waits for an
AWS G-instance quota.

## Start here

- People: [.github/CONTRIBUTING.md](.github/CONTRIBUTING.md). The directory layout is under
  [Code layout](AGENTS.md#code-layout).
- Coding agents: [AGENTS.md](AGENTS.md). Claude Code loads it through
  [.claude/CLAUDE.md](.claude/CLAUDE.md).
- Checks and evidence: [docs/testing.md](docs/testing.md).

## Repository map

| Path | What it holds | Read |
|---|---|---|
| `services/` | The Go services `gateway`, `access`, `metering` and `console` | [Code layout](AGENTS.md#code-layout) |
| `test/mock-upstream/` | Controllable fake model provider for Messages and Responses, used for load and failure tests | [docs/scripts.md](docs/scripts.md#testmock-upstream) |
| `api/` | Internal API contracts as OpenAPI files, one per service, and the error codes | [api/errors.md](api/errors.md) |
| `deploy/compose/` | The local stack: services, console, PostgreSQL, Redis, Prometheus and the mock upstream | [deploy/compose/README.md](deploy/compose/README.md) |
| `deploy/helm/` | The Helm chart, the kind rehearsal, autoscaling, single-service deploy and rollback, and the AWS handoff | [deploy/helm/README.md](deploy/helm/README.md) |
| `deploy/terraform/` | Terraform for networking, EKS, controller IAM and private RDS, with an optional GPU model host | [deploy/terraform/README.md](deploy/terraform/README.md) |
| `deploy/aws/` | Operator scripts for a cloud day: Secrets, database reset, live checks, runs, evidence and leftover checks | [deploy/aws/README.md](deploy/aws/README.md) |
| `deploy/aws/real-model.md` | The runbook for tests T0–T5 against vLLM on a GPU host; future work | [deploy/aws/real-model.md](deploy/aws/real-model.md) |
| `loadtest/` | The k6 scenario, the in-cluster runner, results tables, Prometheus export, and real-model prompts and checks | [loadtest/README.md](loadtest/README.md) |
| `docs/scripts.md` | An index of every script: who runs it, its inputs and its side effects | [docs/scripts.md](docs/scripts.md) |
| `docs/diagrams/as-built.md` | Architecture views as built on `main`, and the deviations from the proposal | [docs/diagrams/as-built.md](docs/diagrams/as-built.md) |
| `docs/observability.md` | Logging and metrics design, the metric dictionary and the dashboard | [docs/observability.md](docs/observability.md) |
| `docs/clients/` | Guides for OpenCode (Messages) and Codex CLI (Responses), direct and through the gateway | [opencode.md](docs/clients/opencode.md), [codex.md](docs/clients/codex.md) |
| `docs/proposal/` | The submitted project proposal, revision 1.1 | [proposal-v1.1.md](docs/proposal/proposal-v1.1.md) |

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

## Cloud deployment

AWS plan, apply and destroy are operator-only: only bibi45c runs them, and every
cloud run costs money. The local Terraform checks need no AWS credentials. This
README does not repeat the commands; follow these documents:

- [deploy/terraform/README.md](deploy/terraform/README.md): inputs, budget, state,
  plan and apply, and the daily teardown.
- [deploy/helm/README.md](deploy/helm/README.md#aws-handoff-operator-only): the AWS
  handoff, with the Secrets, RDS and the chart on EKS.
- [deploy/aws/README.md](deploy/aws/README.md): the cloud-day flow and the leftover
  checks after teardown.
- [deploy/aws/real-model.md](deploy/aws/real-model.md): the real-model session, which
  extends a cloud day and waits for the GPU quota.

## Load tests

[loadtest/README.md](loadtest/README.md) describes the k6 scenario on the local stack
and in the cluster. On EKS, `loadtest/kube_run.py` runs k6 in a Pod on the
load-generator node. Real-model runs must set `CONNECTION_REUSE=0`; see
[Real-model runs](loadtest/README.md#real-model-runs). The results of the S3 cloud
runs are on [issue #77](https://github.com/bibi45c/gateflow/issues/77).
