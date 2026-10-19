# Scripts

An index of every script in the repository, one section per directory. Each row
rests on the script's docstring, its `--help` text or its code. "unclear: …" marks a
point the code does not settle.

Scope: the tracked `*.py` and `*.js` files, the Go command-line tools under `test/`,
and the workflows under `.github/workflows/`. The repository has no tracked `*.sh`,
`*.ps1` or Makefile. The Go services under `services/` are applications, not scripts.
Test files are not listed one by one; each section ends with one line on what its
tests cover.

Columns:

- **Run by**: developer, CI, operator, or test (started by another script or a test
  run).
- **Side effects**, always in the same four parts:
  - Cluster: whether it reads or changes a Kubernetes cluster.
  - AWS: whether it calls the `aws` CLI.
  - Paid: whether it creates anything billable itself. Anything run against EKS
    still runs on nodes, RDS and an ALB that are billed while the environment exists.
  - Deletes: whether it deletes data or objects.

## tools/repo

| Script | Purpose | Run by | Inputs → outputs | Side effects |
|---|---|---|---|---|
| [check_issue.py](../tools/repo/check_issue.py) | Checks an issue's title and sections against the issue forms in `.github/ISSUE_TEMPLATE/`. | CI ([issue-check.yml](../.github/workflows/issue-check.yml)); developer, with `--title` and `--body-file` | Local mode: `--title`, `--body-file`, and the repository's `area:` labels or `--areas` → a JSON result on stdout; exit 1 when a field is missing. CI mode: the event in `GITHUB_EVENT_PATH` → one format reminder comment per open issue, JSON lines on stdout. | Cluster: no. AWS: no. Paid: no. Deletes: no. Calls the GitHub API, with `GH_TOKEN` when set. CI mode creates and edits bot comments; later duplicates are edited to point at the first one, not deleted. Local mode only reads labels, and with `--areas` makes no network call. |
| [image_metadata.py](../tools/repo/image_metadata.py) | Writes the image tags of one service for the images workflow. | CI ([images.yml](../.github/workflows/images.yml)) | `--service`, `GITHUB_SHA`, `GITHUB_REF` → `tags` in `GITHUB_OUTPUT`: `ghcr.io/bibi45c/gateflow-<service>:<sha>`, plus `:<tag>` on a `v*` tag. With `--digest`: a summary entry in `GITHUB_STEP_SUMMARY` instead. | Cluster: no. AWS: no. Paid: no. Deletes: no. Writes only the two GitHub Actions files; no network. The workflow, not this script, pushes images. |

Tests (`tools/repo/tests/`): the issue format check and its comment updates, without
network access; the image tags; relative links, heading anchors and skill routing in
Markdown files; the error codes of `api/errors.md` against the OpenAPI contracts.

## deploy/helm

| Script | Purpose | Run by | Inputs → outputs | Side effects |
|---|---|---|---|---|
| [toolchain.py](../deploy/helm/toolchain.py) | Installs pinned Helm 3.16.3, kind 0.33.0 and kubectl 1.35.0 into a local folder. | CI ([helm.yml](../.github/workflows/helm.yml)); developer | `--directory` → `helm`, `kind` and `kubectl` (`.exe` on Windows) in that folder, each checked against its published SHA-256. | Cluster: no. AWS: no. Paid: no. Deletes: no; it overwrites binaries of the same name. Downloads from github.com, dl.k8s.io and get.helm.sh. |
| [rehearse.py](../deploy/helm/rehearse.py) | Prepares a disposable kind cluster, installs the chart and runs the smoke suite through ingress. | CI ([helm.yml](../.github/workflows/helm.yml)); developer | `--cluster` (must start with `gateflow`), `--kubeconfig`, `--namespace` → kind cluster `kind-<cluster>`, a credentials JSON next to the kubeconfig, release `gf`, smoke output. `--existing-cluster` reuses the cluster and upgrades. `--prepare-only` stops before Helm. `--deploy-check`, `--load-check` and `--autoscale-check` run one extra check instead of the smoke suite. | Cluster: kind only, never the default kubeconfig. Creates the cluster (not with `--existing-cluster`), Secrets with random values, a PostgreSQL fixture and ingress-nginx from raw.githubusercontent.com. `--deploy-check` moves the gateway image with `deploy.py` and rolls it back; records go to `deployments/` next to the kubeconfig. `--load-check` runs `loadtest/kube_run.py` into `loadtest/rehearsal/` next to the kubeconfig. AWS: no. Paid: no. Deletes: earlier outputs of `--deploy-check` and `--load-check` in those folders; never the cluster. Smoke requests add ledger entries to the fixture database. |
| [autoscale.py](../deploy/helm/autoscale.py) | Rehearses KEDA autoscaling of the gateway on kind: scale-up, scale-down, and no scale-down while Prometheus is down. | developer, through `rehearse.py --existing-cluster --autoscale-check`; not in CI | Module without its own command line. Settings from `rehearse.py` and `monitoring/versions.json` → `.local/autoscale/autoscale-<UTC>.json` with replica samples and phase results; exit 1 when a required phase fails. | Cluster: kind only (context from `rehearse.py`). Installs the pinned Prometheus and KEDA charts, upgrades `gf` with `autoscaling.enabled=true`, opens `mock-load` streams through ingress, scales Prometheus to 0 and back to 1. AWS: no. Paid: no. Deletes: no data. The upgrade from `values-kind.yaml` turns `dashboard.enabled` off, which removes the dashboard ConfigMap ([Helm README](../deploy/helm/README.md#load-and-scaling-dashboard-74)). |
| [deploy.py](../deploy/helm/deploy.py) | Moves one service of an installed release to an image digest, checks that only that service changed, runs the smoke suite and writes a record. | operator; CI on kind, through `rehearse.py --deploy-check` | `--kubeconfig`, `--kube-context`, `--service` with `--digest`, or `--rollback-from RECORD`; `-f` values files; `--smoke-port` or `--skip-smoke` → `helm upgrade` of the release, then a JSON record in `.local/deployments/<UTC>-<service>.json` or `--record`; exit 1 on any failure. | Cluster: yes, the `--kube-context` cluster (EKS or kind), never the default kubeconfig. Runs `helm upgrade --wait --wait-for-jobs`; never `helm install` or `helm rollback`. AWS: no. Paid: no new resource. Deletes: no; it replaces one service's Pods. Smoke requests add ledger entries; with `--smoke-db-secret` the smoke suite creates and deletes a query Pod. |
| [dashboard.py](../deploy/helm/dashboard.py) | Installs the pinned Grafana release that shows the FlowGate dashboard. | operator; developer on kind | `--kubeconfig`, `--kube-context`, `--namespace`, `--release` → Secret `gateflow-grafana-admin` (random password, only when missing) and Helm release `grafana`. Stops unless the `<release>-dashboard` ConfigMap matches this checkout and release `prometheus` in `monitoring` is deployed with available replicas. | Cluster: yes; checks Prometheus but never changes it. AWS: no. Paid: no new resource; Grafana gets a ClusterIP Service and no Ingress. Deletes: no; an existing admin Secret is kept. Never prints the credentials. |

Tests (`deploy/helm/tests/`): chart rendering with `helm template` (needs helm on
PATH), dashboard provisioning and its panel semantics, the ScaledObject and the
monitoring pins, the scrape jobs, and the logic of `rehearse.py`, `autoscale.py` and
`deploy.py` without a cluster. CI runs them in
[helm.yml](../.github/workflows/helm.yml).

## deploy/aws

Operator scripts for the cloud day. Each runs external commands through an
injectable runner, so the tests need neither aws nor kubectl.

| Script | Purpose | Run by | Inputs → outputs | Side effects |
|---|---|---|---|---|
| [common.py](../deploy/aws/common.py) | Shared helpers: Terraform output parsing, secret redaction, private file writes, the git-ignore check and command running. | imported by the other three scripts; not run directly | None of its own. | Cluster: no. AWS: no. Paid: no. Deletes: no. Its `require_ignored` runs `git check-ignore` and refuses an output path inside the repository that Git does not ignore. |
| [secrets.py](../deploy/aws/secrets.py) | Creates or updates the `gateflow-bootstrap` and `gateflow-runtime` Secrets on EKS. | operator (bibi45c only) | `--terraform-output` (a saved `terraform output -json` file), `--kubeconfig`, `--kube-context`, `--state` (default `.local/cloud-day/secrets-state.json`) → both Secrets applied with `kubectl apply -f -`; the state file, when values are generated. `--refresh-master-only` replaces only `PGPASSWORD` in `gateflow-bootstrap`. | Cluster: yes, applies Secrets; never the default kubeconfig. AWS: yes, `aws secretsmanager get-secret-value` on the RDS master secret (read only). Paid: no new resource. Deletes: no; it overwrites Secret values. It refuses to generate new values while either Secret exists without a matching state file. |
| [reset_db.py](../deploy/aws/reset_db.py) | Exports, empties and reseeds the accounting tables on EKS between load-test conditions. | operator (bibi45c only) | `--kubeconfig`, `--kube-context`, `--balance`, `--tenants`, `--evidence-dir` → without `--yes`, the plan on stdout. With `--yes`: counts and CSV of `accounting.wallets`, `accounting.reservations` and `accounting.ledger_entries` in `.local/cloud-day/db-reset/<UTC>/`, then reseeded and verified wallets. | Cluster: only with `--yes`. It scales the gateway to 0 or pauses its KEDA ScaledObject, runs a query Pod and a seed Pod, deletes both and restores the gateway. AWS: no. Paid: no new resource. Deletes: **yes**; with `--yes` it truncates the three accounting tables after exporting them. It never touches the access schema. |
| [leftovers.py](../deploy/aws/leftovers.py) | Records the environment's AWS resources before teardown (`inventory`) and lists what is left after it (`check`). | operator (bibi45c only) | `inventory`: `--terraform-output` → `.local/cloud-day/teardown-inventory.json` (`--out`); exit 2 when a query failed. `check`: `--inventory`, `--state-bucket`, `--wait`, `--json` → a table or JSON of leftovers; exit 0 none, 1 leftovers found, 2 a query failed. | Cluster: no. AWS: yes, describe and list calls only. Paid: no. Deletes: no; it only reports. |

Tests (`deploy/aws/tests/`): all three operator scripts with fake runners, and their
`--help` text; a loader imports them without letting `secrets.py` shadow the standard
library module.

## loadtest

| Script | Purpose | Run by | Inputs → outputs | Side effects |
|---|---|---|---|---|
| [messages.js](../loadtest/messages.js) | k6 script: scheduled `POST /v1/messages` streams at fixed arrival rates. | developer, with k6 against compose; operator, through `kube_run.py`; not in CI | Environment settings such as `GATEWAY_URL`, `LEVELS`, `TENANTS`, `PAIRED` and `PROMPTS` ([Settings](../loadtest/README.md#settings)) → k6 JSON output (`--out json=…`) with the plan and per-request tags that `summarize.py` reads. | Cluster: no access of its own; it only sends HTTP. AWS: no. Paid: no. Deletes: no. Requests the gateway admits reserve and settle in the seeded wallets, adding ledger entries. `PAIRED=1` also sends every request to the mock, or to `DIRECT_URL`. k6 sends usage statistics unless run with `--no-usage-report`. |
| [summarize.py](../loadtest/summarize.py) | Builds the results tables of a `messages.js` run from k6's JSON output. | developer; operator, through `kube_run.py` | k6 JSON output → Markdown tables on stdout and `<output>.results.json` or `--out`; exit 2 when a check (empty wallet, bad key, dropped or unclassified requests) is non-zero. | Cluster: no. AWS: no. Paid: no. Deletes: no; it overwrites its results file. Offline. |
| [kube_run.py](../loadtest/kube_run.py) | Runs `messages.js` in a k6 Pod inside the cluster and summarizes it locally. | operator; developer on kind, through `rehearse.py --load-check` | `--kubeconfig`, `--kube-context`, `--name`, `--env KEY=VALUE`, `--prompts`, `--gateway-url`, `--k6-cpu`, `--k6-memory` → `.local/loadtest/<name>/` or `--out-dir`, with `run.json`, `exit`, `k6.log`, `summary.md`, `run.results.json` and `run-record.json` (with the run's UTC timeline). | Cluster: yes, never the default kubeconfig. Creates a ConfigMap and Pod `gateflow-k6-<name>` on the load-generator node, copies the output back, then deletes both, also after an error or Ctrl-C. AWS: no. Paid: no new resource. Deletes: only the objects it created; it refuses an output folder that already holds outputs. The load adds ledger entries. |
| [prom_export.py](../loadtest/prom_export.py) | Exports the Prometheus series of a run window for the results report. | operator, after `kube_run.py` | `--record` (a `run-record.json`) or `--start` and `--end`; `--step`, `--tail-seconds`, `--skip` → `prom/<query>.json` per query and `prom/index.json` next to the record, or `--out-dir`; exit 2 when a query returned no series. | Cluster: yes, read only, through `kubectl port-forward` to the Prometheus Service; never the default kubeconfig. AWS: no. Paid: no. Deletes: no; `--overwrite` replaces an earlier export. It waits until the window's end plus the tail has passed. |
| [realmodel_eval.py](../loadtest/realmodel_eval.py) | Checks that answers through the gateway are byte-identical to direct answers from a model server, and scores them (T5). | operator, in the Pod its `pod` command writes; developer against compose | `run`: `--direct-url`, `--gateway-url`, `--model`, `--out-dir`, `--prompts`, `--protocol`, `--concurrency`, `--passes`, `--control`, `--record-deltas`, `--exec-humaneval` → `answers-<pass>.jsonl`, `summary.json`, `summary.md`; exit 1 on a mismatch or an incomplete call, 2 on unusable input. `reconcile`: optional `--answers`, `--ledger`, `--reservations` (the CSVs of `reset_db.py`) → PASS or the mismatches; exit 1 on any. `pod` → a Pod manifest. | Cluster: no access of its own; `pod` only writes a file. AWS: no. Paid: no. Deletes: no; it overwrites its output files. Gateway calls add ledger entries. `--exec-humaneval` runs model-written code in a subprocess, which is not a sandbox. |
| [prompts/build_prompts.py](../loadtest/prompts/build_prompts.py) | Rebuilds `prompts/realmodel.jsonl` from pinned GSM8K, MMLU and HumanEval sources. | developer, only to reproduce or change the prompt set | None → `prompts/realmodel.jsonl` or `--out`; exits on a source whose SHA-256 or revision moved. | Cluster: no. AWS: no. Paid: no. Deletes: no; it overwrites its output. Downloads from GitHub and Hugging Face. |
| [derive.py](../loadtest/derive.py) | Derives L, C, D and T from the single-Pod calibration repeats. | unclear: the docstring names no role; it runs offline on saved run folders | Run folders, each with `run-record.json`, `run.results.json` or `results.json`, and `prom/`; `--level` → Markdown on stdout and JSON at `--out`; exit 2 when no run is valid or no level passes. | Cluster: no. AWS: no. Paid: no. Deletes: no. Offline. |

Tests (`test_*.py` in `loadtest/`): `summarize.py`, `kube_run.py` with a fake runner,
`prom_export.py` with a fake port-forward and Prometheus, `derive.py` on run
folders built by the tests, and `realmodel_eval.py` against fake streaming upstreams. They need neither k6, kubectl nor a cluster. `messages.js`
has no automated test.

## test/e2e

| Script | Purpose | Run by | Inputs → outputs | Side effects |
|---|---|---|---|---|
| [smoke.py](../test/e2e/smoke.py) | End-to-end smoke test through the gateway: Messages and Responses requests, then ledger and reservation checks. | CI ([tests.yml](../.github/workflows/tests.yml) compose job, [helm.yml](../.github/workflows/helm.yml) through `rehearse.py`); developer; test, started by `rehearse.py` and `deploy.py` | `--project` for compose, or `--kube-context` and `--namespace` with `--postgres-pod` or `--db-secret`; `--host`, `--port` (default `GATEWAY_PORT` or 8080); `ACCESS_SEED_SECRET` → one PASS or FAIL line per check; exit 0 only when every check passed. | Cluster: only in Kubernetes mode; `kubectl exec` psql in the PostgreSQL Pod, or with `--db-secret` a query Pod it creates and deletes. Compose mode runs psql with `docker compose exec`. AWS: no. Paid: no new resource. Deletes: no. Its requests use mock models and add reservations and ledger entries to the target database. |

Tests (`test/e2e/`): the smoke test's helpers, such as key derivation, SSE parsing,
request shapes, SQL quoting and query Pod commands. They need no network and no
Docker.

## test/mock-upstream

| Script | Purpose | Run by | Inputs → outputs | Side effects |
|---|---|---|---|---|
| [main.go](../test/mock-upstream/main.go) | Controllable fake model provider: `POST /v1/messages` (Anthropic Messages) and `POST /v1/responses` (OpenAI Responses), with a fixed reply per mock model name. | test: runs as a container in the compose stack and the Helm chart; [images.yml](../.github/workflows/images.yml) builds its image | `MOCK_*` environment variables that pace streams and inject failures, such as `MOCK_FIRST_EVENT_DELAY_MS` and `MOCK_LOAD_EVENTS` → an HTTP server on port 8090. | Cluster: runs inside one when deployed. AWS: no. Paid: no. Deletes: no. It makes no outbound calls. |

Tests (Go tests in `internal/mock`): every scenario over both protocols, stream
pacing and the `MOCK_*` settings, failure injection and tool calls.

## test/observability

| Script | Purpose | Run by | Inputs → outputs | Side effects |
|---|---|---|---|---|
| [main.go](../test/observability/main.go) and [probe.go](../test/observability/probe.go) | Loopback probe that measures the instrumentation prototype of `pkg/observability`. It is not a load-capacity test. | developer, with `go run ./test/observability` ([Run the prototype](observability.md#run-the-prototype)) | `-duration`, `-warmup`, `-rps`, `-repeats`, `-stream-delay`, `-scrape-interval`, `-output` → a JSON report at `-output` or on stdout; progress on stderr; exit 2 on settings out of bounds. | Cluster: no. AWS: no. Paid: no. Deletes: no; it overwrites the `-output` file. All traffic stays inside one process on 127.0.0.1. |

Tests (Go tests in `test/observability/`): first-output timing, admission release when
a client disconnects, that bodies and headers are never logged, and the generator's
handling of the terminal event.

## .github/workflows

| Script | Purpose | Run by | Inputs → outputs | Side effects |
|---|---|---|---|---|
| [tests.yml](../.github/workflows/tests.yml) | Repository, load test and AWS script tests on three OSes; Go checks; Go integration tests; the compose smoke test. | CI: pull requests, push to `main` | Runs the unit tests of `tools/repo/tests`, `loadtest` and `deploy/aws/tests`; `gofmt`, `go vet`, `go test`, `go build`; `docker compose up`, an inline health-check loop on ports 8080, 8081, 8082 and 8090, the `test/e2e` unit tests and `test/e2e/smoke.py`, then `docker compose down --volumes` → job results. | Cluster: no. AWS: no. Paid: no AWS cost. Deletes: only the runner's compose volumes and the per-test databases of the integration tests. |
| [helm.yml](../.github/workflows/helm.yml) | Helm kind smoke. | CI: pull requests and pushes to `main` that touch `deploy/helm/**`, `test/e2e/**` or the workflow | Runs `deploy/helm/toolchain.py`, `helm lint` with the default and AWS values, the `deploy/helm/tests` and `test/e2e` unit tests, then `deploy/helm/rehearse.py` three times: fresh, `--existing-cluster`, and `--existing-cluster --deploy-check`; kubectl diagnostics on failure → job result. | Cluster: kind cluster `gateflow71ci` inside the runner, deleted at the end with `kind delete cluster`. AWS: no. Paid: no. Deletes: only that cluster. |
| [images.yml](../.github/workflows/images.yml) | Builds the four service images; publishes them from `main` and `v*` tags. | CI: pull requests (build only); push to `main` or a `v*` tag (publish) | Runs `tools/repo/image_metadata.py` for the tags, and again with `--digest` after publishing → `ghcr.io/bibi45c/gateflow-<service>` images and their digests in the job summary. | Cluster: no. AWS: no. Paid: unclear: it publishes to GHCR, and the repository does not state GHCR's billing. Deletes: no. |
| [issue-check.yml](../.github/workflows/issue-check.yml) | Issue format check. | CI: issue opened or edited; label created, edited or deleted; manual dispatch | Runs `tools/repo/check_issue.py` with `GH_TOKEN` → format reminder comments on issues. | Cluster: no. AWS: no. Paid: no. Deletes: no; it creates and edits comments (`issues: write`). |
| [terraform.yml](../.github/workflows/terraform.yml) | Terraform format, init, validate and mocked tests. | CI: pull requests, push to `main` | Runs no repository script: `terraform fmt -check`, `init -backend=false`, `validate` and `test` in `deploy/terraform` → job result. | Cluster: no. AWS: no; mock providers only, with `AWS_EC2_METADATA_DISABLED`. Paid: no. Deletes: no. |

## Typical flows

### Local compose smoke

1. `docker compose up` with `deploy/compose/compose.yaml` (starts the services and
   [mock-upstream](../test/mock-upstream/main.go)).
2. [smoke.py](../test/e2e/smoke.py).
3. Optional load: [messages.js](../loadtest/messages.js) with k6, then
   [summarize.py](../loadtest/summarize.py).
4. `docker compose down --volumes`.

### kind rehearsal

1. [toolchain.py](../deploy/helm/toolchain.py).
2. [rehearse.py](../deploy/helm/rehearse.py) (creates the cluster, installs, runs
   [smoke.py](../test/e2e/smoke.py)).
3. `rehearse.py --existing-cluster` (upgrade, then `smoke.py`).
4. `rehearse.py --existing-cluster` with one of `--deploy-check`
   ([deploy.py](../deploy/helm/deploy.py)), `--load-check`
   ([kube_run.py](../loadtest/kube_run.py)) or `--autoscale-check`
   ([autoscale.py](../deploy/helm/autoscale.py)); optionally
   [dashboard.py](../deploy/helm/dashboard.py).
5. `kind delete cluster`.

### Cloud day

1. `terraform apply`, then save `terraform output -json`
   ([Terraform README](../deploy/terraform/README.md#state-and-operator-only-deployment)).
2. [secrets.py](../deploy/aws/secrets.py), Helm installs of Prometheus, KEDA and `gf`,
   then [reset_db.py](../deploy/aws/reset_db.py) `--yes`.
3. [kube_run.py](../loadtest/kube_run.py) per condition (runs `messages.js` and
   `summarize.py`), with `reset_db.py --yes` between conditions; optionally
   [dashboard.py](../deploy/helm/dashboard.py).
4. [prom_export.py](../loadtest/prom_export.py) after each run, then
   [derive.py](../loadtest/derive.py).
5. [leftovers.py](../deploy/aws/leftovers.py) `inventory`, `terraform destroy`,
   `leftovers.py check`
   ([Daily teardown](../deploy/terraform/README.md#daily-teardown-bibi45c-only)).

### Single-service deploy and rollback

1. [images.yml](../.github/workflows/images.yml) publishes the digest
   ([image_metadata.py](../tools/repo/image_metadata.py)).
2. On EKS, [secrets.py](../deploy/aws/secrets.py) `--refresh-master-only`, before
   every `helm upgrade`.
3. [deploy.py](../deploy/helm/deploy.py) `--service` and `--digest` (runs
   [smoke.py](../test/e2e/smoke.py), writes a record).
4. `deploy.py --rollback-from <record>` (runs `smoke.py` again).
