# AWS operator scripts

Scripts that **bibi45c only** runs against the S3 environment on EKS and RDS. They
use the Python standard library only, take every account, cluster and file name as
an argument, and never use the default kubeconfig. Run them from the repository root.
Each one prints `--help`. Their tests need neither AWS, kubectl nor helm:

```text
python -m unittest discover -s deploy/aws/tests -v
```

| Script | Use | Documented in |
|---|---|---|
| `secrets.py` | Creates the runtime and bootstrap Secrets, or refreshes the RDS master password | [Helm README](../helm/README.md#aws-handoff-operator-only) |
| `reset_db.py` | Empties the accounting tables and seeds large wallets again | [Helm README](../helm/README.md#aws-handoff-operator-only) |
| `leftovers.py` | Saves the inventory before teardown and finds what is left after it | [Terraform README](../terraform/README.md#daily-teardown-bibi45c-only) |
| `check_live.py`, `expect/` | Checks the live configuration before and after a run | [below](#check_livepy) |
| `conditions.py` | Ladders, upgrades, drain, settle and counted runs of a condition | [below](#conditionspy) |
| `collect_config.py` | Collects the cluster configuration and A3 evidence before teardown | [below](#collect_configpy) |
| `credserve.py` | Serves fresh AWS credentials to the Terraform container | [credential bridge](../terraform/credential-bridge.md) |
| `sweep.py` | Second, independent leftover check in every region | [below](#sweeppy) |

For tests T0–T5 against a real model on a GPU host, follow
[real-model.md](real-model.md), which extends the cloud day below.

`deploy/aws/secrets.py` shadows the standard library module of that name for every
script in this folder, so none of them imports `secrets`.

## Cloud day tooling

These tools gate a cloud day: before and after every counted run, before teardown
and after it. Their exit codes follow one rule: **0** the gate passed, **1** the
gate found something (drift, leftovers, still active, not settled), **2** a query
failed or an input is unusable. A failed query is never read as "nothing there" or
as zero, so a script that calls them can stop at the first non-zero exit.

### check_live.py

Saves and checks the live configuration of the release before a run, and again after
it with `--baseline`. It reads the Deployments, Pods and KEDA ScaledObjects of the
namespace and checks them against an expect file.

```text
python deploy/aws/check_live.py --kubeconfig <path> --kube-context <context> --out .local/cloud-day/live-<run>.json \
  --expect deploy/aws/expect/gateway-250m.json --image-tag <deployed commit> --replicas 1
python deploy/aws/check_live.py ... --out .local/cloud-day/live-<run>-after.json --baseline .local/cloud-day/live-<run>.json --replicas 1
python deploy/aws/check_live.py ... --autoscale 2 4 --target <T> --duration <D>
```

| Argument | Meaning |
|---|---|
| `--expect` | Expected requests, limits and replicas per service, the ScaledObject's server address and scale-down window |
| `--image-tag` | Tag every container image must carry |
| `--replicas N` | Fixed run: no ScaledObject, exactly N running gateway Pods |
| `--autoscale MIN MAX --target T --duration D` | Auto run: one ScaledObject on the gateway with MIN, MAX, both triggers at threshold T, the demand query multiplied by D, settled at MIN |
| `--baseline` | The file of the check before the run: no restart count may grow, and no Pod of a fixed run may be replaced |
| `--namespace`, `--release` | Default `gateflow` and `gf`; Deployments are `<release>-<service>` |

It writes the observed state and the problems to `--out`. Exit codes: 0 as expected,
1 a problem, 2 a query failed (nothing is checked) or the expect or baseline file is
unreadable.

The expect files in [expect/](expect) are keyed by the chart's service names:

| File | Configuration |
|---|---|
| `gateway-1vcpu.json` | `values-aws.yaml` alone: the chart defaults with a 1 vCPU, 2 GiB gateway |
| `gateway-250m.json` | a 0.25 vCPU, 1 GiB gateway with access 3, metering 5 and the mock upstream at requests cpu 1 / memory 1Gi, limit memory 2Gi |

An expect file describes the values files passed to `helm upgrade`. Copy and adjust
one whenever the values change; keep the values overlay itself under `.local/`.

### conditions.py

One subcommand per gate of a condition. Every subcommand that reaches the cluster
takes `--kubeconfig`, `--kube-context`, `--namespace` (default `gateflow`) and
`--release` (default `gf`).

| Subcommand | Use | Main arguments | Exit codes |
|---|---|---|---|
| `levels fixed` | Prints `LEVELS` = round(N × L × factor) | `--replicas N --level L [--factors 0.8,1.0,1.15,1.3]` | 0 |
| `levels auto` | Prints `LEVELS` = round((k − 0.5) × T / D) per replica step k, a repeated level lowered by 1 | `--target T --duration D [--steps 2,3,4,3,2]` | 0, 2 no positive level |
| `upgrade` | Optional master-password refresh (`--terraform-output`), then `helm upgrade --reuse-values`, then the rollout (fixed) or settle at the ScaledObject's minimum (auto) | `--values FILE` (repeatable, in order; default `values-aws.yaml`), `--fixed N` or `--auto --target T --duration D` | 0, 1 a command failed or not settled, 2 query failed |
| `drain` | Waits until no admitted request is active on the gateway | `--timeout 300`, `--targets N`, `--target-wait 60`, `--max-sample-age 60`, `--job gateflow-gateway` | 0 drained, 1 still active at the timeout, 2 query failed, 3 no measurable gateway target |
| `settle` | Waits for exactly N ready replicas and N gateway Pods; on timeout prints the Deployment, HPA, ScaledObject and latest events | `--replicas N --settle-timeout 600` | 0, 1 not settled, 2 query failed |
| `run` | `kube_run.py` with `LEVELS` (output in `--log`), `prom_export.py` of its window (without the HPA queries for `--mode fixed`), and the verdict of each hold | `--name <run> --mode fixed\|auto --levels <LEVELS> -- <more kube_run.py arguments>` | kube_run.py's code if not 0, otherwise prom_export.py's |

**drain** queries Prometheus inside the cluster (`kubectl exec` into
`deploy/prometheus-server`) for `up{job=J}`, `timestamp(up{job=J})` and
`gateflow_gateway_active_admitted_requests{job=J}`:

- A failed exec, an answer that is not JSON, a status other than `success`, a result
  that is not an instant vector or a value that is not a number is a **query
  failure**: exit 2 at once, never read as drained and never waited on.
- No gateway target (`count(up{job=J})` = 0) means no gateway Pod is scraped; with a
  target down, a scrape older than `--max-sample-age`, the gauge missing on a target,
  or a target count other than `--targets`, the drain **cannot be judged**. It waits
  up to `--target-wait` seconds for a complete reading, then exits 3 with the reason.
- Only a complete, fresh reading decides: 0 active is drained (exit 0); more is still
  active, polled every `--interval` seconds until `--timeout` (exit 1).

On day 2 the old shell drain printed `still active: -1` for five minutes while the
gateway had no Pod at all; that state now ends with exit 3 and
`no gateway target: count(up{job="gateflow-gateway"}) = 0`.

### collect_config.py

Before teardown, saves what the report's A3 evidence needs to `--out`, which git must
ignore: Helm values of each release with every credential-like value removed, the
Helm list, nodes, Pods, images and digests, init exit codes and restarts, events,
Secret names and key names (the values never reach the script), the Ingress, the
ALB's security group rules and the HTTP status of `/metrics`, `/healthz` and
`/readyz` through the ALB.

```text
python deploy/aws/collect_config.py --kubeconfig <path> --kube-context <context> --out .local/cloud-day/day<N>/day<N>-config \
  --terraform-output .local/cloud-day/tf-output.json --copy 'plan-day<N>*.txt' --copy 'live-d<N>-*.json' --copy 'reset-d<N>-*'
```

`--copy` globs are relative to `--from` (default `.local/cloud-day`) and copy files
and folders unchanged; `--helm-release NAME:NAMESPACE` replaces the default list
(`gf`, `prometheus`, `keda`, `aws-load-balancer-controller`); `--region` defaults to
ap-southeast-1. A failed step is recorded in `collect-errors.json` and the others
still run. Exit codes: 0 everything collected, 1 a step failed or a glob matched
nothing, 2 the output path is not ignored by git.

### credserve.py

Serves `aws configure export-credentials` to the Terraform container on
`127.0.0.1` only; see the [credential bridge](../terraform/credential-bridge.md) for
the setup and its acceptance test. Main arguments: `--token-file` (required, ignored
by git), `--port` (default 17651), `--log`, `--profile`. Exit codes: 0 after Ctrl-C,
2 the token file is not ignored or the server cannot start.

### sweep.py

After `leftovers.py check`, a second check that needs no inventory and no tags: in
every enabled region (or `--regions ...`) it lists every EKS cluster, live EC2
instance, NAT gateway, Elastic IP, ELBv2 load balancer, RDS instance, manual RDS
snapshot, EBS volume and non-default VPC, whatever it belongs to. S3 is not checked,
since the state bucket is kept. Exit codes: 0 nothing found in any region, 1 something
found, 2 a query failed.

### Data bundle

`loadtest/bundle.py` builds the Part C bundle of a day from a manifest; see
[Build the data bundle](../../loadtest/README.md#build-the-data-bundle).

### Flow example

A day in Git Bash, from the repository root. Values in angle brackets are
placeholders; L, T and D come from `loadtest/derive.py` of the day's calibration.
The flow resets the wallets **before** autoscaling is switched on: on day 2,
`reset_db.py` paused the KEDA-owned gateway at 0 and it never came back, so the
auto runs never started.

```bash
set -euo pipefail  # stop at the first failed gate; a pipe keeps the failing exit code
CD=.local/cloud-day DAY=<N> SHA=<deployed commit> L=<L> T=<T> D=<D>
K=(--kubeconfig "$CD/eks.kubeconfig" --kube-context <EKS context>)
VALUES=(--values deploy/helm/values-aws.yaml --values "$CD/<values overlay>.yaml")
EXPECT=deploy/aws/expect/gateway-250m.json
K6=(--k6-cpu 4 --k6-memory 16Gi --env MAX_VUS=12000 --env STREAM_S=45)
live() { local name=$1; shift; python deploy/aws/check_live.py "${K[@]}" --expect "$EXPECT" --image-tag "$SHA" --out "$CD/live-$name.json" "$@"; }

for n in 1 2 4; do
  levels=$(python deploy/aws/conditions.py levels fixed --replicas "$n" --level "$L")
  python deploy/aws/conditions.py upgrade "${K[@]}" "${VALUES[@]}" --terraform-output "$CD/tf-output.json" --fixed "$n"
  python deploy/aws/reset_db.py "${K[@]}" --evidence-dir "$CD/reset-d$DAY-rep$n" --yes
  for r in 1 2 3; do
    run=d$DAY-rep$n-r$r
    python deploy/aws/conditions.py drain "${K[@]}" --targets "$n"
    live "$run" --replicas "$n"
    python deploy/aws/conditions.py run "${K[@]}" --name "$run" --mode fixed --levels "$levels" -- "${K6[@]}" || echo "NOTE $run exited $?"
    live "$run-after" --replicas "$n" --baseline "$CD/live-$run.json" || echo "NOTE $run: the after-check found changes"
  done
done

python deploy/aws/reset_db.py "${K[@]}" --evidence-dir "$CD/reset-d$DAY-auto" --yes  # still fixed: the scale path
python deploy/aws/conditions.py upgrade "${K[@]}" "${VALUES[@]}" --terraform-output "$CD/tf-output.json" --auto --target "$T" --duration "$D"
levels=$(python deploy/aws/conditions.py levels auto --target "$T" --duration "$D")
for r in 1 2 3; do
  run=d$DAY-auto-r$r
  python deploy/aws/conditions.py drain "${K[@]}"
  python deploy/aws/conditions.py settle "${K[@]}" --replicas 2
  live "$run" --autoscale 2 4 --target "$T" --duration "$D"
  python deploy/aws/conditions.py run "${K[@]}" --name "$run" --mode auto --levels "$levels" -- "${K6[@]}" || echo "NOTE $run exited $?"
  python deploy/aws/conditions.py settle "${K[@]}" --replicas 2
  live "$run-after" --autoscale 2 4 --target "$T" --duration "$D" --baseline "$CD/live-$run.json" || echo "NOTE $run: the after-check found changes"
done
```

Teardown, whatever happened to the runs:

```bash
python deploy/aws/collect_config.py "${K[@]}" --out "$CD/day$DAY/day$DAY-config" --terraform-output "$CD/tf-output.json" \
  --copy "plan-day$DAY*.txt" --copy "smoke-day$DAY*.txt" --copy "live-d$DAY-*.json" --copy "reset-d$DAY-*" || echo 'see collect-errors.json'
python deploy/aws/leftovers.py inventory --terraform-output "$CD/tf-output.json"
# Remove the ScaledObjects and the Ingress, wait for the ALB to go, uninstall the Helm releases and
# destroy with a reviewed plan, as deploy/terraform/README.md (Daily teardown) describes.
python deploy/aws/leftovers.py check --inventory "$CD/teardown-inventory.json" --state-bucket <state bucket> --wait 1800
python deploy/aws/sweep.py
python loadtest/bundle.py --manifest "$CD/day$DAY-manifest.json"
```
