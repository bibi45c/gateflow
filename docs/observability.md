# Logging and experiment metrics

Design and skeleton prototype for [#13](https://github.com/bibi45c/gateflow/issues/13).
The comparison recommends Prometheus metrics plus structured JSON logs. The note
and prototype do not record the owner's decision or complete service integration.
Scope follows [Proposal sections 5.6 and 6.2](proposal/proposal-v1.1.md).

## Options and recommendation

| Option | Benefit | Tradeoff |
|---|---|---|
| JSON logs only | Reuses the existing slog logger; request timings can be analysed offline | A separate real-time aggregation pipeline is needed for the Prometheus KEDA signals; log I/O and storage still cost resources |
| Prometheus metrics plus JSON logs | Direct counters, gauges and histograms for experiments and KEDA; fits the proposal | A shared helper, bounded labels, collectors and a dashboard are needed; request and scrape overhead must be measured |
| OpenTelemetry metrics and traces, exported to Prometheus/Grafana | Optional cross-service timing diagnosis | SDK/exporter/Collector and trace-storage work; tracing is UC15 and disabled in the baseline capacity test |

Recommend the second option for the core deliverable. Keep the existing JSON
process logs. Reuse one technical metrics helper and one dashboard. Use optional
traces only if core acceptance is complete and their separate overhead is measured.
The costs above are relative implementation and operation costs, not price quotes.

See the official [Prometheus instrumentation guidance](https://prometheus.io/docs/practices/instrumentation/)
and [OpenTelemetry overview](https://opentelemetry.io/docs/what-is-opentelemetry/).
The prototype pins the Prometheus Go client to v1.25.0; its Go requirement is
compatible with this repository's Go 1.27. Later integrations should keep the
dependency pinned and verify changes. Adding the client also raises two indirect
dependencies that pgx already brought in: `golang.org/x/sync` from v0.17.0 to v0.23.0
and `golang.org/x/text` from v0.29.0 to v0.42.0. The database tests pass with them.

## Implemented prototype and integration boundaries

The [helper](../pkg/observability/observability.go) has explicit per-process
registries, bounded route/result dimensions, safe diagnostic fields and request
lifecycle handles. Nil registry means logs only; nil observer means no observation.
It is demonstrated on [the probe](../test/observability/main.go), using the existing
[pkg/server](../pkg/server/server.go) on a random loopback port. Service mains,
contracts, business rules, Compose and CI are unchanged.

The helper has explicit hooks and never wraps ResponseWriter, consumes bodies,
parses SSE, verifies identities or reads business state. The caller supplies byte
counts and the result, and calls:

- `Begin` at receipt, with a configured route template. This starts timing before
  authentication and internal calls.
- `SetVerifiedTenantSlot` after the caller verifies identity, without restarting
  timing. Caller-provided tenant headers are not proof of identity.
- `MarkEligible` once before a capacity decision, including a later capacity
  rejection. Invalid credentials and traffic outside tenant limits do not call it.
- `MarkAdmitted` once when the request is admitted; waiting for first output is
  part of active demand.
- `MarkFirstOutput` after protocol-recognised model output is written and flushed.
  Headers, comments, keep-alives and metadata do not call it.
- `Finish` once with actual result, status and byte counts. Deferred interrupted
  cleanup releases admission on every exit. Competing calls and late hooks have no
  further effect; HTTP 200 does not override a reported SSE error.

Real protocol recognition and request limits belong to gateway internal packages;
transaction outcomes belong to metering; key/config decisions belong to access.
All cross-service business calls remain HTTP. The probe's scripted eligibility and
protocol-neutral events are not authentication, Anthropic/Responses compatibility,
reservation or capacity evidence.

## Interim logging on main

The S1 services merged before this decision, so each one logs requests in its own
way. This is the state of `main` on 13 October 2026, at `b951ff0`:

| Service | What it logs today | Missing against this note |
|---|---|---|
| gateway | One `request` line per call, marked in the code as interim until #13: method, route, status, duration_ms, model, request_bytes, response_bytes, request_id. A recorder around the ResponseWriter counts the status and the bytes | A result, a tenant and the time to first output; `model` is the requested name, not a configured alias; no metrics |
| access | One `request` line per call: request_id, method, route, status, duration_ms, and key_id and tenant_id once a key is verified | A result; no metrics |
| metering | Failures only: `metering request failed` with operation, code, request_id and err | A line for a call that succeeds; no metrics |

The helper was written before these lines existed. It now follows them, so that a
service which adopts it gains fields and metrics without renaming anything:

- **Line and field names.** The helper writes one `request` line, like the services,
  with the same names for method, route, status, duration_ms, model, request_bytes,
  response_bytes and request_id. It used to write `request_finished` and
  `http_status`.
- **Result names.** For an admitted call the gateway reports the five outcomes of
  issue #18, and the helper's results for an admitted call are those five under the
  same names; see [Results](#results). It used to say `success` for `completed` and
  had no `not_dispatched`. A test reads the outcome list of
  [the metering contract](../api/metering.yaml) and fails when the two differ.
- **Byte counts.** The gateway counts response bytes in its recorder; the helper wraps
  nothing and takes counts from its caller. The two fit as they are: the gateway
  passes its recorder's counts to `Finish`.
- **Model.** The gateway logs the model name the client asked for. The helper logs a
  configured alias, or `other`, so that client input never becomes a log value it
  did not expect. This difference stays.

## Results

Every request ends with exactly one result. It is the `result` field of the request
line and the `result` label of `gateflow_http_requests_total`, and it does not depend
on the HTTP status: a stream that fails after a 200 is not `completed`.

| Result | When |
|---|---|
| `completed`, `not_dispatched`, `upstream_error`, `client_cancelled`, `interrupted` | An admitted call, by the first-match rule of issue #18. A call that times out after admission is `interrupted`, as that rule has it; the stage of a timeout belongs to the planned `gateflow_gateway_timeouts_total` |
| `invalid_request` | The request is malformed or too large |
| `authentication_denied` | The key or the internal token is missing or unknown |
| `permission_denied` | The key is valid but may not do this, such as use the requested model |
| `insufficient_balance` | The cost could not be held |
| `tenant_limited` | The tenant is over its own limit; not counted as eligible demand |
| `capacity_rejected` | The call was eligible, but the service had no capacity |
| `dependency_error` | Another service or a store did not answer |
| `internal_error` | A fault of the service itself |

The set is fixed in the helper, and every route has one counter per result from the
start, so a result that never happened reads as a real zero. An unknown result is
recorded as `interrupted`.

## Safe logs and bounded labels

One `request` JSON entry contains service, method, configured route,
status (the actual HTTP status, 0 if not established), result, duration_ms, request_bytes,
response_bytes, tenant_slot, configured model alias, validated request_id and
first_output_ms only if model output was observed. Duration ends before log output;
client timing includes transport, and the benchmark separately measures Finish.
Body byte counts do not include TLS/header bytes. No per-frame logs.

Never record bodies, prompts, tool arguments/results, keys, Authorization,
credential values, raw query strings, upstream error bodies, or an arbitrary error's
text. The helper accepts metadata rather than request/response objects or errors.
It cannot sanitise a logger that a caller already configured with sensitive fields.
The fixture tests both the helper and actual probe logs with canaries.

Request IDs must have the `req_<uuid>` shape; invalid values are omitted. Model
aliases come from configuration, and unknown values become `other`. Tenant slots
are limited to tenant-00 through tenant-10, other and unverified. This mapping is
for controlled experiment diagnostics, not an identity or permission rule.

Metrics have only fixed service, configured route and fixed result dimensions.
Request IDs, reservation/key IDs, raw paths, arbitrary tenants/models and URLs are
never labels. The prototype does not yet emit tenant metrics. Later experimental
tenant counters and first-output histograms use only the fixed 13-slot set, not
every metric crossed with every tenant. Per-request logs link ID/tenant/instance;
shared CPU, pool or memory measurements do not prove per-tenant resource usage.

Use seconds, bytes and `_total` counters. Histogram bucket definitions are frozen
with experiment configuration. Sum buckets across replicas; do not average Pod p95s.
See [metric naming](https://prometheus.io/docs/practices/naming/) and
[histogram aggregation](https://prometheus.io/docs/practices/histograms/).

## Metric dictionary

"Prototype" means actual helper collectors. Every other row is a planned
integration requirement; an absent feature is not represented as working telemetry.

| Metric/family | Type and bounded dimensions | Observation/use | Status |
|---|---|---|---|
| `gateflow_http_requests_total` | Counter: service, route, result | One terminal result, independent of HTTP success | Prototype |
| `gateflow_http_request_duration_seconds` | Histogram: service, route | Begin to Finish, including any work before Finish | Prototype |
| `gateflow_http_request_bytes_total`, `gateflow_http_response_bytes_total` | Counters: service, route | Caller-reported application bytes; no body capture | Prototype |
| `gateflow_gateway_active_admitted_requests` | Gauge: fixed service | Waiting-first-output and relaying requests; released on every exit | Prototype hook, real admission later |
| `gateflow_gateway_eligible_arrivals_total` | Counter: fixed service | Valid demand including capacity rejects, excluding tenant limits | Prototype hook, real eligibility later |
| `gateflow_gateway_first_output_seconds` | Histogram: service, route | Explicit first model output; no zero samples for no-output failures | Prototype hook, native recognition later |
| `gateflow_gateway_requests_received_total`, `gateflow_gateway_admitted_requests_total` | Counters | Cumulative received/admitted counts, not just current active demand | Planned |
| `gateflow_gateway_relay_duration_seconds` | Histogram: route, result | Admission to relay terminal; settlement measured separately | Planned |
| `gateflow_gateway_rejections_total`, `gateflow_gateway_timeouts_total` | Counters: reason or stage | Key/model/quota/tenant/capacity/dependency; first/idle/total/write/internal timeouts | Planned |
| `gateflow_gateway_draining_requests`, `gateflow_gateway_admission_ready` | Gauges | Draining subset and acceptance of new requests, distinct from Kubernetes readiness | Planned |
| `gateflow_gateway_buffered_bytes`, `gateflow_gateway_write_duration_seconds` | Gauge and histogram | Application buffers and downstream write/flush delays | Planned |
| `gateflow_internal_requests_total`, `gateflow_internal_request_duration_seconds` | Counter/histogram: service, peer, operation, result as needed | Internal HTTP completion/errors and timing | Planned; the S1 services merged without it |
| `gateflow_metering_reservations_total`, `gateflow_metering_reservation_duration_seconds` | Counter/histogram: result | Reserve includes price lookup, pool wait and short transaction; success only after commit | Planned; the S1 services merged without it |
| `gateflow_metering_settlements_total`, `gateflow_metering_settlement_duration_seconds` | Counter/histogram: result | Commit/replay/conflict/error; replay is not a new charge | Planned; the S1 services merged without it |
| `gateflow_db_transaction_duration_seconds`, `gateflow_db_pool_acquire_duration_seconds` | Histograms: service, fixed operation/result where needed | Transaction and pool wait separately, no model-stream lifetime lock | Planned; the S1 services merged without it |
| `gateflow_db_pool_connections`, `gateflow_db_lock_waiting_sessions` | Gauges: service, fixed state | Acquired/idle/total/max; PostgreSQL measured lock waiting, not renamed SQL latency | Planned |
| `gateflow_redis_operations_total`, `gateflow_redis_operation_duration_seconds` | Counter/histogram: fixed operation/result | Limit/lease/cache timing and dependency failure, no Redis keys | Planned |
| `gateflow_cache_requests_total` | Counter: service, fixed cache, hit/miss/error | Immutable-price/config/key cache hit rates; no extension of revocation limits | Planned |
| `gateflow_tenant_requests_total`, `gateflow_tenant_first_output_seconds` | Counter/histogram: fixed tenant slot, result or route | Controlled isolation experiments; no unbounded tenant population | Planned |
| `gateflow_accounting_pending_reservations`, `gateflow_accounting_oldest_pending_age_seconds` | Gauges | Durable uncertain-state visibility; S1 unsettled holds are not full pending recovery | Later UC6 |
| `gateflow_accounting_outbox_pending_events`, `gateflow_accounting_oldest_outbox_age_seconds`, `gateflow_accounting_events_total` | Gauges/counter: fixed result | Durable backlog/age; applied/duplicate/failed event outcomes | Later outbox/SQS |
| Container/process CPU, working set/RSS, network, goroutines; deployment desired/ready and DB/Redis capacity/connections/lock waits; SQS backlog/age | Existing collector/provider metrics | Resolve actual exporter names/permissions at deployment; do not duplicate authority | Planned infrastructure |

The immutable ledger is authoritative for money. Prometheus counters can miss
updates on crashes and cannot establish exactly-once settlement or rebuild balances.
S1 #27 excludes outbox/SQS and recovery of never-settled reservations.

## KEDA demand and missing metrics

Let C be the calibrated per-Pod admission cap, T initially 0.7*C, and D the measured
mean duration of the same frozen workload. The proposal's 20 seconds is provisional.
Use two independent Prometheus triggers, both `metricType: AverageValue`, with the
same threshold T: total active admitted, and eligible arrival rate multiplied by D.
Do not divide the deployment totals by replica count before querying KEDA.

With complete fresh data the query shapes are:

```promql
sum(gateflow_gateway_active_admitted_requests{job="gateflow-gateway"})
```

```text
sum(rate(gateflow_gateway_eligible_arrivals_total{job="gateflow-gateway"}[1m])) multiplied by calibrated D
```

The job label and window are illustrative. The desired-demand approximation is
ceil(max(active, arrivals*D)/T), limited to 2..4; actual HPA control also has sampling,
readiness, tolerance and stabilization. The provisional 60-second downscale window
belongs to HPA behavior, not scale-to-zero cooldown.

[KEDA 2.18](https://keda.sh/docs/2.18/reference/scaledobject-spec/) documents
AverageValue and HPA behavior. [Kubernetes HPA](https://kubernetes.io/docs/concepts/workloads/autoscaling/horizontal-pod-autoscale/)
describes taking the largest recommendation and skipping downscale on metric errors.

The query shapes above are not deployed guards: sum can undercount when only some
targets disappear. Later recording rules/configuration must check recent successful
scrapes and expected admission-ready/draining target coverage; insufficient rate
samples, stale/partial targets or query failure must become unavailable, not zero.
Only healthy complete idle targets produce a real zero. Set `ignoreNullValues: false`
and never mask missing data with `or vector(0)`; that setting alone does not detect
partial missing targets. See [the Prometheus scaler](https://keda.sh/docs/2.18/scalers/prometheus/).

Test A missing/B valid, B missing/A valid, both missing, genuine idle zeros, capacity
saturation with rising arrivals, and draining requests. Record external metrics,
HPA Conditions and desired/ready/admission-ready replicas; verify no unsafe downscale.
No KEDA object, guard or cloud scaling result is implemented in this prototype.

## One dashboard and acceptance evidence

Use one dashboard, filtering run/profile/config version and aligning six panel
groups: offered/outcomes, demand/replicas, latency/internal calls, dependencies/hotspot,
resources/interruption, and tenant/accounting. The proposal fixes supporting capacity
while changing gateway 1/2/4 replicas, then changes supporting API replicas separately.
Keep ten-wallet and shared-wallet, normal/fault/slow-reader, 20s/60s and fixed/KEDA
conditions separate. Report measured boundaries and failed/unreached load levels.

| Proposal scenario | Necessary evidence beyond the prototype |
|---|---|
| Fixed/automatic scaling and hotspot | Scheduled/admitted/completed/rejected/timeouts, active/demand, replicas, resource/pool/lock/cache/backlog; generator/simulator capacity and frozen supporting resources |
| First-output latency and 99.5% completion | Client dispatch/first model output/terminal/deadline/outcome and matched gateway/direct inputs, output schedules and network location |
| Service separation | Independent image/rollout, three business Deployments, role isolation, no upstream dispatch on unconfirmed reservation |
| Client compatibility/reuse | CLI/model versions, read/edit/test/tool-result workflow, redacted IDs, reference app/additional channel contracts, changed files and actual hours |
| Tenant isolation | A at ten times tenant rate; B completion and paired latency within calibrated capacity; rate/concurrency enforcement and resource observations |
| Provider/dependency failure | Attempt and reservation/ledger state, explicit outcomes and no automatic model replay; safe admission closure/resumption |
| Planned/abrupt interruption | Ingress stream completion/interruption, admission/readiness/drain timeline and first new successful request after crash |
| Slow readers | 10% at 1KiB/s, at least 20s read pause, buffer/memory/write timeout/upstream cancellation and unaffected tenants |
| Accounting/recovery | Database/ledger assertions: nonnegative available balance, one terminal charge, idempotent repeated events and visible uncertain records |
| Key/object access | Revocation-to-denial within configured 30s bound, cross-tenant object denial, cache/config evidence; metrics alone do not prove security |

Normal-load completion counts terminal success before the deadline and includes all
scheduled valid, sufficiently funded requests; capacity rejection and timeout are
failures. Invalid-key, tenant-limit, cancellation and injected faults are separate.
Formal added latency is the p95 of individual paired gateway-minus-direct first-model
output differences, not a subtraction of two independent p95s. Keep negative
differences, raw distributions, usable/failed pair counts and completion evidence.
Keep-alives/metadata do not count; latency cannot pass a run that fails completion.

## Run the prototype

From the repository root, after the tool versions in [verification](testing.md) are
available. Every command stands alone and uses native paths through Go/Python.

```text
go test ./pkg/observability ./test/observability -v
go test ./pkg/observability -run ^$ -bench BenchmarkRequestHooks -benchmem -benchtime=1s -count=3
go run ./test/observability -duration 10s -warmup 1s -rps 100 -repeats 3 -scrape-interval 1s -output .local/observability/results.json
```

The microbenchmark reports wall ns/op, bytes/op and allocations/op for no observation,
JSON-only and metrics+JSON. The loopback probe compares these modes plus scraping,
with alternating order, ordinary and 1ms test streams, 1KiB request bodies, scheduled
arrivals, raw request samples, completion, dispatch lag, scrape series/bytes and
active returning to zero. Scrapes themselves are not model requests. Raw output
contains no bodies/credentials. The counting/discard log sink measures serialization
and synchronization, not disk/stdout shipping. HeapAlloc is Go heap in the shared
probe process, not RSS or server-only allocation.

The short probe is not the proposal's 60s warmup/60s ramp/180s hold, 20/60s streams,
native protocols, ingress, three-service authentication/reservation, KEDA or formal
paired-latency experiment. CPU/request, RSS, real log I/O, deployment costs and
full-service overhead require later isolated experiments. A benchmark ns/op is not
a direct CPU measurement. Preserve raw values and variance; no negligible-overhead
or 50ms-capacity conclusion follows from this probe.

The helper and probe tests cover bounded metadata/labels, real zero demand,
capacity rejection visibility, one finish under competing exits, registry isolation,
SSE error classification, metadata exclusion, immediate first frame, unchanged
fixture bytes, cancellation, safe logging and deferred cleanup. Races should also be
tested where a CGO/race toolchain is available; report unavailable checks explicitly.

## Recovery and remaining work

The prototype is not enabled in any service main and stores no business data.
Keep helper/probe changes reviewable separately. Follow-up issues must name owners
and scope for each service, collectors, dashboard and KEDA guards. Do not copy private
business code into pkg or change shared contracts while instrumenting.

When later reverting live telemetry, agree safe fixed replicas before removing
signals; never feed KEDA a false zero or turn off financial/admission protection.
Issue comparison/decision and actual measurement evidence still need recording on
#13. Local tests, current-head CI, human decision, merge and issue Done remain
separate under [the repository rules](../.github/CONTRIBUTING.md).
