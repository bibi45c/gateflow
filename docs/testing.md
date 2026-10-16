# Verification

## Checks

| Check | Command, from the repository root | Required for merge |
|---|---|---|
| Repository tests | `python -m unittest discover -s tools/repo/tests -v` | Yes: CI jobs `Tests (ubuntu-24.04)`, `Tests (macos-latest)` and `Tests (windows-latest)` |
| Service image builds | PR run of `.github/workflows/images.yml`, building all four targets without publishing | Yes for image workflow changes: CI jobs `Image build (gateway)`, `Image build (access)`, `Image build (metering)` and `Image build (mock-upstream)` |
| Terraform configuration | `terraform -chdir=deploy/terraform fmt -check -recursive`, `terraform -chdir=deploy/terraform init -backend=false -input=false -lockfile=readonly`, `terraform -chdir=deploy/terraform validate`, `terraform -chdir=deploy/terraform test` (mock providers only) | Yes for Terraform changes: CI job `Terraform validation`; no AWS credentials or real plan/apply |
| Helm deployment | `helm lint deploy/helm/gateflow`, `python -m unittest discover -s deploy/helm/tests -v`, then `python deploy/helm/rehearse.py --cluster gateflow71ci --kubeconfig .local/kubeconfig` and repeat with `--existing-cluster` | Yes for Helm/smoke changes: CI job `Helm kind smoke`; disposable local cluster, no AWS |
| Load test tools | `python -m unittest discover -s loadtest -v` | Yes: CI jobs `Tests (ubuntu-24.04)`, `Tests (macos-latest)` and `Tests (windows-latest)` |
| AWS operator scripts | `python -m unittest discover -s deploy/aws/tests -v` | Yes: CI jobs `Tests (ubuntu-24.04)`, `Tests (macos-latest)` and `Tests (windows-latest)` |
| Go checks | `go vet ./...`, then `go test ./...`, then `go build ./...` | Yes: CI jobs `Go (ubuntu-24.04)`, `Go (macos-latest)` and `Go (windows-latest)` |
| Go integration tests | With `GATEFLOW_TEST_POSTGRES_URL` set to a Postgres superuser URL, `go test -count=1 ./...` | Yes: CI job `Go integration (ubuntu-24.04)` |
| Compose smoke test | `docker compose -p <worktree-name> -f deploy/compose/compose.yaml up --build --detach`, then open `http://localhost:<port>/healthz` for ports 8080, 8081, 8082 and 8090, then `docker compose -p <worktree-name> -f deploy/compose/compose.yaml down --volumes` | Yes: CI job `Compose smoke test` |
| End-to-end test | With the compose stack up, `python -m unittest discover -s test/e2e -v`, then `python test/e2e/smoke.py --project <worktree-name>` | Yes: CI job `Compose smoke test` |

A required check must pass in CI on the head; a local run is supporting evidence.

- **Repository tests** need Python 3.13 or newer and only the standard library. They
  cover the issue format check and documentation links. On macOS use `python3` if
  `python` is not Python 3; on Windows `py -3.13` also works.
- **Load test tools** need Python 3.13 or newer and only the standard library. They
  cover `loadtest/summarize.py`, which builds the results table from k6's JSON output,
  and need neither k6 nor a stack. The k6 script itself has no automated check,
  because CI has no k6; a local run is its evidence (see `loadtest/README.md`).
- **Go checks** need the Go version in `go.mod`. They cover the services, `pkg/`,
  the mock upstream and the module boundaries (`archtest`).
- **Go integration tests** need a Postgres 17 server. They cover migrations and the
  isolation between service roles. Without `GATEFLOW_TEST_POSTGRES_URL` they skip, so
  the plain Go checks never need a database. `pkg/pgtest` gives each test its own
  database and drops it afterwards, and never reads `DATABASE_URL`. To run them
  locally, start the compose `postgres` service (`up --detach postgres`) and use
  `postgres://gateflow:gateflow-local-only@localhost:<POSTGRES_PORT>/gateflow?sslmode=disable`.
- **The compose smoke test** needs Docker. It builds every image, seeds the test
  tenants and wallets, and checks that each process answers its health endpoint. Use
  a project name per worktree (`-p`), so one worktree never reuses another's
  containers, and change host ports with the `*_PORT` variables when two stacks run
  at once.
- **The end-to-end test** needs the compose stack started as above, and Python 3.13
  or newer with only the standard library. It sends requests through the gateway on
  `GATEWAY_PORT` (default 8080) with the seeded keys, which it derives from
  `ACCESS_SEED_SECRET`, checks each status, stream and error body, and reads the
  metering ledger with `psql` in the `postgres` container. Pass the stack's project
  name as `--project`. Each check prints a PASS or FAIL line. Its unit tests in
  `test/e2e` need neither Docker nor the network.

Add a check here, with its exact command and the CI job that runs it, in the same PR
that adds its configuration. No configured check means no automated evidence; report
it as "not run", never as "passed".

For each change:

1. Run the checks closest to what you changed.
2. Run every required check.
3. For a rule or template change, also try one realistic case, such as filling in the
   changed issue form. Valid syntax alone proves nothing about behavior.
4. Give tests that write files or use ports or databases their own temporary
   resources, because other worktrees may run tests at the same time.

## Evidence

Report each check with:

- Command or scenario.
- Environment: OS and the version of each tool the check uses (`python --version`,
  `go version`, `docker version`).
- Head SHA, base branch and base tip SHA, and whether the head is up to date with the
  base.
- Uncommitted inputs, if any.
- Result: passed, failed, not run or blocked. Explain the last three.
- Short evidence, such as the summary line of the output.

Example, with illustrative SHAs: `python -m unittest discover -s tools/repo/tests -v`; Windows 11,
Python 3.14; head `1a2b3c4`, base `main` at `5d6e7f8`, up to date; passed, 30 tests.

Re-run affected checks when the head or base changes, or when you are unsure that the
tested content equals the delivered content. Two branches that pass separately are not
proven to work together.

## Gates

- Required checks are every check marked required under Checks, plus every check in
  the issue's verification plan.
- A merge needs every required check passed on the current head, with the head up to
  date with the current tip of its base branch. A failed or missing required check
  blocks the merge.
- Judge each check by its latest run on the head; an earlier run of the same check
  does not count either way.
- Any other failing check also blocks the merge unless the owner, or the integrator
  when the owner is the author, records on the PR why it does not apply.
- Post-merge acceptance, such as a test that needs a deployment, must be agreed in the
  issue. It never replaces a required security, contract or correctness check.
- An issue is Done only when all agreed acceptance is recorded on it.
- Local runs, CI, agent review, human approval, merge and deployment are separate
  evidence; one does not imply another.
- Repository protections are whatever GitHub settings enforce, and this document does
  not enable any. Read them back with
  `gh api repos/bibi45c/gateflow/branches/main/protection`.
