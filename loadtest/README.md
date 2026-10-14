# Load tests

Scheduled load on the gateway, following
[proposal section 5.6](../docs/proposal/proposal-v1.1.md#5-6-capacity-planning-and-experiments).

| File | Contents |
|---|---|
| `messages.js` | k6 script: scheduled `POST /v1/messages` streams at fixed arrival rates, optionally paired with the same call straight to the mock |
| `summarize.py` | Builds the results table from the k6 JSON output |
| `test_summarize.py` | Unit tests of `summarize.py`; they need neither k6 nor a stack |

## Run

You need k6 2.3.0 and Python 3.13 or newer. Start the compose stack as
[docs/testing.md](../docs/testing.md#checks) describes, then from the repository root:

```text
k6 run --no-usage-report --out json=.local/loadtest/run.json -e GATEWAY_URL=http://127.0.0.1:8080 -e PAIRED=1 -e MOCK_URL=http://127.0.0.1:8090 loadtest/messages.js
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
otherwise seed the wallets with a larger `metering seed --balance`.

## Settings

Pass each with `-e NAME=value`.

| Name | Default | Meaning |
|---|---|---|
| `GATEWAY_URL` | `http://127.0.0.1:8080` | Gateway base URL |
| `PAIRED` | `0` | `1` also sends each request straight to the mock at the same moment, to measure the delay the gateway adds |
| `MOCK_URL` | `http://127.0.0.1:8090` | Mock upstream base URL, for `PAIRED=1` |
| `MODEL` | `mock-load` | Model name; the mock's scenario |
| `LEVELS` | `5,25,50` | Arrival rates in requests per second; add `150` for the extension |
| `WARMUP_S`, `RAMP_S`, `HOLD_S` | `60`, `60`, `180` | Warm-up at the first level, ramp to each later level, hold at each level |
| `MAX_TOKENS` | `256` | `max_tokens` of every request: at least the deltas of `mock-load`, so each call is charged its real usage; a missing value would hold 32,768 tokens |
| `INPUT_BYTES` | `1024` | Size of the user message |
| `TENANTS` | `10` | Seeded tenants `tenant-01` to `tenant-NN`, used in turn |
| `ACCESS_SEED_SECRET` | the compose default | Secret the seeded keys are derived from, as `access seed` derives them |
| `FIRST_OUTPUT_DEADLINE_MS` | `10000` | First-output deadline |
| `TOTAL_DEADLINE_S` | `60` | Total deadline of a request |
| `STREAM_S` | `25` | Seconds a request is expected to last; the highest level times this is the number of VUs k6 starts before the run |
| `MAX_VUS` | `4000` | Most requests in flight; k6 drops arrivals beyond it |

Requests are sent on schedule even when replies slow down (`ramping-arrival-rate`).
Each VU holds about 1 MB while its stream lasts, so 50 requests per second of 20-second
streams need about 1,000 VUs and 1 GB for k6.

## Results

Each request gets one result:

- **completed**: HTTP 200, first output within its deadline, and a stream that ends
  with `message_stop` and has no `error` event;
- **timed out**: no full reply within the total deadline, or first output after its
  deadline;
- **rejected**: HTTP 429, 503 or 529;
- **failed**: anything else.

Per phase, the table lists scheduled requests (sent plus dropped), admitted (HTTP
200), the four results, completion rate (completed over scheduled), throughput
(completed per second of the phase), the first-output delay at p50, p95 and p99, the
added first-output delay at p95 with the number of pairs, and the most k6 VUs active
at once. The JSON also has total durations, a count per HTTP status, failed pairs,
the added delay at p50, p95 and p99, and the first output of the direct calls. Under the table, the checks count responses with
an empty wallet (402), with a bad key (401 or 403), and arrivals k6 dropped because it
ran out of VUs; a run with any of them does not measure the gateway.

First output is k6's `http_req_waiting`, the time to the response headers: the
gateway writes them with the first event. With `PAIRED=1`, the added delay of a pair
is the gateway call's first output minus the direct call's, when both answered 200;
the calls straight to the mock stay out of the other columns. The proposal's 50 ms
target applies to the p95 of these differences. The mock serves twice the load, so
check that its own first output stays flat before reading the gateway's.

Not measured here:

- the 10-second idle gap between events, which needs an SSE-aware client such as
  xk6-sse;
- the gateway's own active requests, CPU, memory, network and database contention;
  they come from the services' `/metrics` and Prometheus.

## Tests

```text
python -m unittest discover -s loadtest -v
```
