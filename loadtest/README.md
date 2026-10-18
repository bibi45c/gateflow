# Load tests

Scheduled load on the gateway, following
[proposal section 5.6](../docs/proposal/proposal-v1.1.md#5-6-capacity-planning-and-experiments).

| File | Contents |
|---|---|
| `messages.js` | k6 script: scheduled `POST /v1/messages` streams at fixed arrival rates, optionally paired with the same call straight to the mock |
| `summarize.py` | Builds the results tables, per phase and per tenant, from the k6 JSON output |
| `kube_run.py` | Runs `messages.js` in a Pod inside the cluster, copies the output back and runs `summarize.py` |
| `test_summarize.py` | Unit tests of `summarize.py`; they need neither k6 nor a stack |
| `test_kube_run.py` | Unit tests of `kube_run.py`; they need neither kubectl nor a cluster |
| `prom_export.py` | Exports the Prometheus series of a run window through a port-forward |
| `test_prom_export.py` | Unit tests of `prom_export.py`, with a fake port-forward and Prometheus |
| `derive.py` | Derives L, C, D and T from the single-Pod calibration repeats |
| `test_derive.py` | Unit tests of `derive.py`, on run folders built by the tests |
| `bundle.py` | Builds the Part C data bundle of a cloud day from a manifest |
| `test_bundle.py` | Unit tests of `bundle.py`, on a day built by the tests, with a fake aws CLI |

## Run

You need k6 2.3.0 and Python 3.13 or newer. Use a dedicated compose project and a
fresh database for every run. From the repository root, create `.local/loadtest/`
and save this local override as `.local/loadtest/large-wallets.yaml`:

```yaml
services:
  seed-metering:
    command: ["seed", "--balance", "1000000000000"]
```

Start it as below, using another project name and free host ports if needed (see
[compose configuration](../deploy/compose/README.md#configuration)). The override
must be present on the **first** seed: `metering seed --balance` never tops up an
existing wallet. The defaults are local test values only.

```text
docker compose -p lt73 -f deploy/compose/compose.yaml -f .local/loadtest/large-wallets.yaml up --build --detach --wait
```

Run the capacity scenario without direct-to-mock requests:

```text
k6 run --no-usage-report --out json=.local/loadtest/run.json -e GATEWAY_URL=http://127.0.0.1:8080 -e PAIRED=0 loadtest/messages.js
python loadtest/summarize.py .local/loadtest/run.json
```

`--no-usage-report` stops k6 from sending anonymous usage statistics to Grafana.
`summarize.py` prints one Markdown row per phase, then the
[per-tenant tables](#tenant-scenarios), and writes the same results to
`.local/loadtest/run.results.json`.

The default model, `mock-load`, streams for about 20 seconds: 200 deltas of 256 bytes,
100 ms apart, set by `MOCK_LOAD_EVENTS`, `MOCK_LOAD_INTERVAL_MS` and
`MOCK_LOAD_EVENT_BYTES` on the mock upstream (#68). Each delta is one output token, so
every completed call's ledger row has `output_tokens` 200.

Use a fresh database for every run, so that one run's ledger and wallets never carry
into the next: `docker compose -p <name> -f deploy/compose/compose.yaml down --volumes`,
then `up` again. A run that overloads the gateway leaves reservations held whose
settle failed; they stay held until recovery (UC6, see
[api/metering.yaml](../api/metering.yaml)). Check that `wallet_empty_responses` is 0;
otherwise repeat with a fresh database and the larger initial balance. Do not
reuse the volume and assume reseeding updated it.

## Tenant scenarios

Two variants of the run above compare the same arrival levels spread over ten
tenants and concentrated on one wallet. Run each against its own fresh database,
with the large-wallet override, and keep every other setting equal.

**Tenant isolation**: ten equally loaded tenants. This is the default
(`TENANTS=10`): request *n* uses `tenant-01` to `tenant-10` in turn, so each tenant
gets a tenth of the arrivals and its own `accounting.wallets` row.

```text
docker compose -p lt75a -f deploy/compose/compose.yaml -f .local/loadtest/large-wallets.yaml up --build --detach --wait
k6 run --no-usage-report --out json=.local/loadtest/tenants.json -e GATEWAY_URL=http://127.0.0.1:8080 -e PAIRED=0 -e TENANTS=10 loadtest/messages.js
python loadtest/summarize.py .local/loadtest/tenants.json
docker compose -p lt75a -f deploy/compose/compose.yaml down --volumes
```

**Shared wallet**: `TENANTS=1` sends every request with `tenant-01`'s key, so every
reservation and settlement updates the same wallet row.

```text
docker compose -p lt75b -f deploy/compose/compose.yaml -f .local/loadtest/large-wallets.yaml up --build --detach --wait
k6 run --no-usage-report --out json=.local/loadtest/shared.json -e GATEWAY_URL=http://127.0.0.1:8080 -e PAIRED=0 -e TENANTS=1 loadtest/messages.js
python loadtest/summarize.py .local/loadtest/shared.json
docker compose -p lt75b -f deploy/compose/compose.yaml down --volumes
```

The large-wallet override is required here. The one wallet pays for what ten
wallets share in the isolation run: with the defaults, each request holds about
7,200 micro-units until it settles (its body bytes at 3 plus `max_tokens` at 15, the
seeded price version), and the default plan sends about 17,850 requests, more than
the default seed balance of 100,000,000 can be relied on to cover once failed
settlements leave holds behind. The override seeds every wallet, `tenant-01`
included, with 1,000,000,000,000. As always, it must be present on the first seed,
and `wallet_empty_responses` must be 0.

For a quick local check, shorten the plan, for example
`-e LEVELS=5 -e WARMUP_S=10 -e RAMP_S=10 -e HOLD_S=30 -e STREAM_S=25`.

What to compare, at the same level:

- In the isolation run, the **spread** table: sent min / max shows the load was
  even; completion min / max and the TTFB p95 max/min ratio show whether any tenant
  was served worse than the others. A ratio near 1 and equal completion rates mean
  no tenant was favoured.
- Between the runs, the per-phase table (completion, throughput, TTFB) of the
  shared-wallet run against the isolation run. A drop that appears only with one
  wallet points at contention on that wallet's row; confirm it with metering's
  `/metrics` and the database, which these scripts do not measure.

## Separate latency diagnostic

Pairing doubles the load on the mock and the generator's HTTP connections. Keep it
out of capacity runs. For a separate low-rate comparison, use `PAIRED=1` and
explicitly declare `MOCK_FIRST_EVENT_DELAY_MS=0` on **both** the mock server and k6:

```text
k6 run --no-usage-report --out json=.local/loadtest/latency.json -e GATEWAY_URL=http://127.0.0.1:8080 -e MOCK_URL=http://127.0.0.1:8090 -e LEVELS=5 -e PAIRED=1 -e MOCK_FIRST_EVENT_DELAY_MS=0 loadtest/messages.js
python loadtest/summarize.py .local/loadtest/latency.json
```

k6 refuses pairing without that declaration. It cannot inspect the server's
environment; verify its compose settings separately. The mock flushes headers
before its first event, while the gateway flushes with the first event, so an
upstream first-event delay would otherwise be attributed to the gateway.

## Run inside the cluster

On EKS, run k6 inside the cluster (#77). From a laptop through the ALB, the
measurement includes the public internet, the ALB and the NAT, not the gateway
alone. `kube_run.py` puts `messages.js` into a ConfigMap and runs k6 in a Pod on the
load-generator node (it selects `gateflow.io/workload=load-generator` and tolerates
that taint), aimed at the gateway Service `http://<release>-gateway:8080` in the same
namespace. The Pod uses the pinned `grafana/k6:2.3.0` digest, runs as non-root
without a service-account token, and reads `ACCESS_SEED_SECRET` from the
`gateflow-runtime` Secret. When k6 ends, the script copies its JSON output back,
deletes the Pod and the ConfigMap, also after an error or Ctrl-C, and runs
`summarize.py` locally.

You need kubectl, Python 3.13 or newer, an installed release and the
`gateflow-runtime` Secret. The script never uses the default kubeconfig. From the
repository root:

```text
python loadtest/kube_run.py --kubeconfig <EKS kubeconfig> --kube-context <EKS context> --env PAIRED=0
```

`--namespace` and `--release` default to `gateflow` and `gf`. Pass the
[settings](#settings) with `--env NAME=value`; only those settings are accepted, and
three are not: `GATEWAY_URL` and `MOCK_URL` follow from `--release`
(`http://<release>-mock-upstream:8090` for the mock), and `ACCESS_SEED_SECRET` comes
only from the Secret. `PREALLOC_VUS` is set with `--prealloc-vus N`, which must be
a whole number from 1 to `MAX_VUS` (4000 unless set with `--env`); it is left unset
when not given. `--timeout` defaults to the plan's length plus 15 minutes.
k6 requests and is limited to 2 CPUs and 4 GiB (`--k6-cpu`, `--k6-memory`) of the
c6i.xlarge's 4 vCPUs and 8 GiB, which leaves the rest of the node to the mock
upstream. As locally, `wallet_empty_responses` must be 0.

**Single-Pod calibration** (#73): fix the gateway at one replica before the run,
and pause or delete any KEDA ScaledObject or HPA that targets it (the chart creates
none), so that nothing changes the replica count during the run:

```text
helm --kubeconfig <EKS kubeconfig> --kube-context <EKS context> upgrade gf deploy/helm/gateflow -n gateflow --reuse-values --set services.gateway.replicas=1 --wait
python loadtest/kube_run.py --kubeconfig <EKS kubeconfig> --kube-context <EKS context> --name calibration-1 --env PAIRED=0
```

The run record keeps the gateway Deployment's replicas and image before and after
the run (`gateway`, `gateway_after`); both must show one replica.

**Outputs** go to `.local/loadtest/<name>/` (`--out-dir`; `--name` defaults to the
UTC time):

| File | Contents |
|---|---|
| `run.json`, `exit` | k6's JSON output and exit code |
| `k6.log` | k6's own output, saved for successful and failed runs alike |
| `summary.md`, `run.results.json` | the tables printed by `summarize.py` and its JSON |
| `run-record.json` | time, context, namespace, release, k6 image digest, every setting, repository HEAD, the gateway's replicas and image, the k6 and `summarize.py` exit codes, the cleanup, and the run's UTC timeline (below) |

The record's timeline fields are UTC ISO 8601 times with milliseconds:

| Field | Meaning |
|---|---|
| `op_start`, `op_end` | when the script started, and when it had deleted the Pod and ConfigMap |
| `load_start` | k6's time zero: the `gateflow_start` point in `run.json`, written by the first iteration |
| `plan_end` | `load_start` plus the planned stages (warm-up, ramps, holds), without the graceful stop |
| `load_end` | the last point k6 wrote, after the graceful stop |
| `phases` | each phase of the plan, from its `gateflow_plan` point: `phase`, `level_rps`, offsets `start_s` and `end_s`, and UTC `start` and `end` |

They are `null`, with the reason in `timeline_problem`, when `run.json` has no
`gateflow_start` point or cannot be read; the run itself is still summarized.

The script exits with 1 if the run failed or k6 exited non-zero, 130 after Ctrl-C,
and otherwise with `summarize.py`'s exit code. To report a run on its issue, paste
`summary.md` and `run-record.json` into a comment. The record holds no secret value
and no local path; `ACCESS_SEED_SECRET` appears only as its Secret and key. Keep
`run.json` with you rather than posting it.

**Observer effect**: k6 and the mock upstream share the load-generator node, so a
saturated node slows the mock as well as k6. Watch the node's CPU during a run, and
read rising dropped iterations, VUs or direct-request durations as a generator limit,
not as gateway capacity. Pairing (`PAIRED=1`) doubles the mock's load; keep it out of
capacity runs here too.

Rehearse on kind, with an installed release, using a short plan:

```text
python deploy/helm/rehearse.py --cluster <name> --kubeconfig <kind kubeconfig> --existing-cluster --load-check
```

It writes to `loadtest/rehearsal/` next to the kubeconfig and fails unless k6 and
`summarize.py` exit with 0, the results have rows with requests sent, and the Pod and
ConfigMap were deleted.

## Export the Prometheus series

After each in-cluster run, and before the monitoring stack or the Ingress is
deleted, export the run's series with `prom_export.py`:

```text
python loadtest/prom_export.py --kubeconfig <EKS kubeconfig> --kube-context <EKS context> --record .local/loadtest/<name>/run-record.json
```

It opens `kubectl port-forward svc/prometheus-server <free local port>:80` in the
`monitoring` namespace (`--service`, `--monitoring-namespace`), runs the fixed
query list of the report template (Part 0.4) over `load_start` to `load_end` of the
record plus `--tail-seconds` (default 120), with `--step` (default `15s`), and stops
the port-forward on every exit. Instead of `--record`, give `--start` and `--end`
as RFC 3339 times with an offset, and `--out-dir`. If the end of the window is still
to come, the script prints how long it waits and waits, so the tail samples exist;
for `auto` runs, raise `--tail-seconds` to cover the scale-down.

Besides the template's queries it exports `access_results` and `metering_results`
(the `results` rate of those services, by route and result) and the raw counters
`gateway_duration_sum` and `gateway_duration_count` of gateway `/v1/messages`
durations, so that D can be recomputed over any hold window. The namespace and
release in the `cpu_by_pod`, `mem_by_pod`, `net_by_pod`, `replicas` and `hpa_*`
queries come from the record (`--namespace`, `--release` otherwise).

Each response is written unchanged to `prom/<name>.json` next to the record
(`--out-dir`), and `prom/index.json` lists each query, the window, the step and the
number of series and samples it returned. An earlier export is kept unless
`--overwrite` is given. The script exits with 2 when a query returned no series (the
others are still written, and the index marks it `empty`), and with 1 on an HTTP,
connection or port-forward error. Without autoscaling, leave out the HPA queries
with `--skip hpa_desired --skip hpa_conditions`.

## Derive L, C, D and T

From the single-Pod calibration repeats, each a folder with `run-record.json`,
`run.results.json` (or `results.json`, as in the data bundle) and the `prom/`
export, `derive.py` computes the values of the report template (Part 0.3):

```text
python loadtest/derive.py .local/loadtest/calib-1 .local/loadtest/calib-2 .local/loadtest/calib-3 --out .local/loadtest/calib-derived.json
```

- Runs that are not valid (an error in the record, a non-zero k6 or `summarize.py`
  exit code, a non-zero check, or `sent_source` other than `gateflow_sent`) are
  listed and left out.
- **L** is the highest hold level such that it and every lower hold level pass in
  every valid run, a level passing when `completed * 1000 >= 995 * scheduled`; the
  next level is the first violated one.
- Over the hold window of L, from the record's `phases` (or the results' offsets
  after `load_start`): **D** = increase of `gateway_duration_sum` over increase of
  `gateway_duration_count`, between their first and last samples inside the window,
  a counter reset counted as a restart from zero; **C** = floor of the mean of the
  `active` samples inside the window. Across repeats, C is the floor of the median
  of the per-run C, and D the median of the per-run D. **T** = floor(0.7 × C).
- It reports the cross-check |C − L × D| ≤ 10% of C, and the k6 `duration_ms.p50`
  of the same holds next to D. The cross-check is approximate: the gateway's
  duration includes the work before admission, while `active` counts only admitted
  requests.

`--level <req/s>` measures C and D over another hold level instead of L. The script
prints a Markdown summary, writes the JSON to `--out`, and exits with 2 when no run
is valid or no level passes, with 1 when an input is missing, and otherwise with 0,
also when the cross-check is off; then record why.

## Build the data bundle

After a cloud day, `bundle.py` turns its run folders into the Part C data bundle:
per run the results, the record, the raw k6 output gzipped, the Prometheus export,
the RDS values from CloudWatch and a Part B `summary.md` with charts; for the day a
runs CSV, `SHA256SUMS` and a zip. The operator (bibi45c) runs it, signed in to AWS,
because the RDS values come from `aws cloudwatch get-metric-statistics` (read-only):

```text
python loadtest/bundle.py --manifest .local/cloud-day/day2-manifest.json
```

The manifest describes the day. Every run ID starts with `d<day>-`; `condition`
`auto` or `burst` marks an autoscaling run, `calib` a calibration repeat:

```json
{
  "day": "2", "date": "2026-10-18", "commit": "<deployed commit>", "derived": "derived-day2.json",
  "generator_allocatable_cpu": 3.92, "gateway_resources": {"cpu": "1", "memory": "2Gi"},
  "configs": {"cfg-A": "values-aws.yaml, autoscaling off, chart defaults"},
  "database_reset": "`deploy/aws/reset_db.py --balance 1000000000000` before each condition",
  "extra_files": ["teardown-inventory.json", "leftovers-day2.txt"],
  "runs": {"d2-rep1-r1": {"condition": "rep1", "repeat": 1, "config": "cfg-A", "purpose": "1 replica, ladder 32-52", "notes": ""}}
}
```

- Inputs: the run folders in `--runs` (default `.local/loadtest/`), or a folder with
  only `ABORTED.txt` for a run the operator stopped; in `--evidence` (default: the
  manifest's folder) each run's `check_live.py` output (`live_pattern`, default
  `live-{run}.json`), the derived file and `extra_files`; and `<out>/day<N>-config/`
  from `deploy/aws/collect_config.py` with `images.json`, `nodes.json` and
  `tf-output.json`.
- The gateway's resources, and an auto run's min, max, T and D, come from the live
  check; a run's `gateway_resources` and `autoscaling`, then the manifest's
  `gateway_resources`, are the fallback. The generator criterion uses the
  load-generator node's allocatable CPU from `nodes.json`, then
  `generator_allocatable_cpu`.
- Before anything is deleted, it refuses mixed or renamed input (a run ID of another
  day, a record naming another run, a record time not on `date`, a phase outside the
  load window, another commit, a run without record or `ABORTED.txt`, a derived run
  that is not a calib run), AWS not reachable, a live check with problems, an auto
  run without min, max, T and D, a failed CloudWatch query, and an existing bundle
  without `--rebuild`. `day<N>-config/` is always kept.
- `--out` defaults to `<evidence>/day<N>/`, `--zip` to `<out>.zip`, `--region` to
  ap-southeast-1.

Charts need matplotlib, the only package outside the standard library. Without it
the bundle is still built; the output and every summary say the charts were
skipped. The script exits with 0 when the bundle is built and with 1 otherwise.

## Settings

Pass each with `-e NAME=value`, or with `--env NAME=value` to `kube_run.py`.

| Name | Default | Meaning |
|---|---|---|
| `GATEWAY_URL` | `http://127.0.0.1:8080` | Gateway base URL |
| `PAIRED` | `0` | Keep `0` for capacity; `1` enables a separate paired TTFB diagnostic |
| `MOCK_URL` | `http://127.0.0.1:8090` | Mock upstream base URL, for `PAIRED=1` |
| `MOCK_FIRST_EVENT_DELAY_MS` | unset | For pairing, must be explicitly `0`, matching the mock server |
| `MODEL` | `mock-load` | Model name; the mock's scenario |
| `LEVELS` | `5,25,50` | Arrival rates in requests per second; add `150` for the extension |
| `WARMUP_S`, `RAMP_S`, `HOLD_S` | `60`, `60`, `180` | Warm-up at the first level, ramp to each later level, hold at each level |
| `MAX_TOKENS` | `256` | Explicit `max_tokens` on every request; keep it at least the deltas of `mock-load` to cover reported usage. Messages rejects a missing value with 400; Responses defaults to a 32,768-token hold |
| `INPUT_BYTES` | `1024` | Size of the user message |
| `TENANTS` | `10` | Seeded tenants `tenant-01` to `tenant-NN`, used in turn; `1` is the shared-wallet scenario. The compose stack seeds 10 |
| `ACCESS_SEED_SECRET` | the compose default | Secret the seeded keys are derived from, as `access seed` derives them |
| `TTFB_DEADLINE_MS` | `10000` | Time-to-first-byte deadline; this does not measure the first model content delta |
| `TOTAL_DEADLINE_S` | `60` | Total deadline of a request |
| `STREAM_S` | `25` | Seconds a request is expected to last; the highest level times this is the number of VUs k6 starts before the run |
| `PREALLOC_VUS` | unset | VUs k6 starts before the run, replacing the highest level times `STREAM_S`; a whole number from 1 to `MAX_VUS`, otherwise k6 refuses to start. With `kube_run.py`, pass `--prealloc-vus N` instead of `--env` |
| `MAX_VUS` | `4000` | VU ceiling; k6 can drop arrivals when every VU is occupied |

Set `PREALLOC_VUS` when a failing level's requests last longer than `STREAM_S`:
when their duration times the level's rate exceeds the default preallocation
(`min(MAX_VUS, ceil(highest level × STREAM_S))`), k6 has to start VUs during the
run, cannot start them fast enough, and drops iterations, which makes the run
invalid. For example, 150 requests per second lasting 45 seconds keep 6,750 VUs
busy: raise `MAX_VUS` to at least that and pass `--prealloc-vus 6750`. Leave
`STREAM_S` at the expected stream length rather than raising it to get more VUs.

Requests are sent on schedule even when replies slow down (`ramping-arrival-rate`).
Each VU holds about 1 MB while its stream lasts, so 50 requests per second of 20-second
streams need about 1,000 VUs and 1 GB for k6.
`gracefulStop` is the total request deadline plus 5 seconds (65 seconds by default),
so requests started near the end can publish their terminal results. See k6's
[graceful-stop behavior](https://grafana.com/docs/k6/latest/using-k6/scenarios/concepts/graceful-stop/).
Use distinct arrival levels: repeated `(phase, level)` pairs are rejected by the
reporter rather than silently merged.

## Results

Each request gets one result:

- **completed**: HTTP 200, TTFB within its deadline, and a stream that ends
  with `message_stop` and has no `error` event;
- **timed out**: no full reply within the total deadline, or TTFB after its
  deadline;
- **rejected**: HTTP 429, 503 or 529, including dependency errors or upstream
  throttling; this does not establish that an admission cap rejected the call;
- **failed**: anything else.

Per phase, the table lists scheduled requests (sent plus dropped), HTTP 200, the
four results, unclassified requests, completion rate (completed over scheduled),
throughput (completed per second of the phase), TTFB percentiles, added TTFB with
the number of pairs, failed pairs, direct-request duration p95 and maximum VUs.
VUs include preallocated workers; this is not the gateway's active-stream count.
The JSON also has total-duration and direct-TTFB percentiles and HTTP status counts.

When the requests carry a `tenant` tag, as every request of `messages.js` does, two
more tables follow the checks:

- **Per tenant**: per phase and tenant, requests sent, completed, the other three
  results, completion rate and TTFB p50 / p95. Dropped arrivals never reached a
  tenant, so a tenant's completion rate is over the requests sent for it; dropped
  arrivals already make the run invalid through the checks.
- **Spread across tenants**: per phase, the number of tenants, the lowest and
  highest requests sent, completion rate and TTFB p95, and the highest TTFB p95
  over the lowest.

The JSON has them as `tenants` and `tenant_spread`. They do not change the exit
code.

`gateflow_sent` is counted **before** HTTP starts. `unclassified` is sent minus
the sum of the four results, exposing an interrupted iteration even if it emitted
no HTTP metrics. A negative value means excess terminal results. The checks also
count empty-wallet (402), bad-key (401/403) responses and dropped arrivals.
`summarize.py` writes the table and JSON but returns exit code **2** if any check is
nonzero; such a run is not valid capacity evidence. Results finishing during the
grace period still belong to the phase in which the request started.

Older recordings without `gateflow_sent` use `http_reqs` as a fallback and are
marked `sent_source: "http_reqs (legacy)"`; they cannot detect interruptions that
emitted no HTTP metrics. Old `gateflow_added_first_output_ms` points are accepted
but reported under the corrected TTFB names.

TTFB is k6's [`http_req_waiting`](https://grafana.com/docs/k6/latest/using-k6/metrics/reference/),
the wait for the first response byte. It is **not** the first model content delta
defined in the proposal and [observability design](../docs/observability.md).
With pairing, `gateflow_added_ttfb_ms` subtracts the direct call's TTFB from the
gateway call's when both complete successfully. Direct calls stay out of gateway
request counts. Inspect both direct TTFB and duration for mock saturation, and
never use this proxy alone to claim the proposal's 50 ms first-output target or
derive an accepted KEDA target T. Old paired calibration values remain provisional.

Not measured here:

- true first-content timing and the 10-second idle gap between events, which need
  an SSE-aware client such as xk6-sse;
- the gateway's own active requests, CPU, memory, network and database contention;
  they come from the services' `/metrics` and Prometheus.

## Tests

```text
python -m unittest discover -s loadtest -v
```
