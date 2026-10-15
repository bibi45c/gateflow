# Load tests

Scheduled load on the gateway, following
[proposal section 5.6](../docs/proposal/proposal-v1.1.md#5-6-capacity-planning-and-experiments).

| File | Contents |
|---|---|
| `messages.js` | k6 script: scheduled `POST /v1/messages` streams at fixed arrival rates, optionally paired with the same call straight to the mock |
| `summarize.py` | Builds the results table from the k6 JSON output |
| `test_summarize.py` | Unit tests of `summarize.py`; they need neither k6 nor a stack |

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
`summarize.py` prints one Markdown row per phase and writes the same results to
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

## Settings

Pass each with `-e NAME=value`.

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
| `TENANTS` | `10` | Seeded tenants `tenant-01` to `tenant-NN`, used in turn |
| `ACCESS_SEED_SECRET` | the compose default | Secret the seeded keys are derived from, as `access seed` derives them |
| `TTFB_DEADLINE_MS` | `10000` | Time-to-first-byte deadline; this does not measure the first model content delta |
| `TOTAL_DEADLINE_S` | `60` | Total deadline of a request |
| `STREAM_S` | `25` | Seconds a request is expected to last; the highest level times this is the number of VUs k6 starts before the run |
| `MAX_VUS` | `4000` | VU ceiling; k6 can drop arrivals when every VU is occupied |

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
