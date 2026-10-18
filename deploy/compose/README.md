# Local compose stack

`compose.yaml` runs every FlowGate service with Postgres and Redis. The start and
stop commands are at the top of that file; the checks are in
[docs/testing.md](../../docs/testing.md).

## Configuration

Every service reads its settings from these variables. Compose sets all of them
except the `SHUTDOWN_*` pair, `MODEL_ALIASES`, `UPSTREAM_MAX_IDLE_CONNS` and `DATABASE_MAX_CONNS`, whose
defaults suit it, so a service issue adds code that reads a variable, not a compose
change. The defaults
are local-only test values, not credentials: never reuse them outside this stack.
Override a value in `deploy/compose/.env`, which Git ignores.

| Variable | Read by | Local default | Meaning |
|---|---|---|---|
| `DATABASE_URL` | access, metering | The service's own role on database `gateflow` | Postgres connection; each service logs in as its own role |
| `DATABASE_MAX_CONNS` | access, metering; compose leaves it unset | pgx's default, the larger of 4 and the CPU count | Most database connections the service's pool opens, a whole number of at least 1; a separate variable because `psql` rejects `pool_max_conns` in `DATABASE_URL`. The seed commands keep the default |
| `ACCESS_URL` | gateway, metering | `http://access:8081` | Internal base URL of access |
| `METERING_URL` | gateway | `http://metering:8082` | Internal base URL of metering |
| `ACCESS_INTERNAL_TOKEN` | access (accepts); gateway, metering (send) | `access-token-local-only` | Bearer token for calls to access |
| `METERING_INTERNAL_TOKEN` | metering (accepts); gateway (sends) | `metering-token-local-only` | Bearer token for calls to metering |
| `MODEL_ALIASES` | gateway; compose leaves it unset | The mock scenarios (`mock-text` … `mock-load`), `qwen3:8b`, `qwen2.5-coder:7b` | Comma-separated model names, at most 16, that request log lines show as they are; any other model is logged as `other`. A model is never a metric label |
| `UPSTREAM_MAX_IDLE_CONNS` | gateway; compose leaves it unset | `256` | Idle upstream connections the gateway keeps, in all and to each upstream host, for 90 s, so a load test reuses connections instead of opening one per call |
| `ACCESS_KEY_PEPPER` | access | `key-pepper-local-only` | Secret mixed into the stored HMAC of each gateway key |
| `ACCESS_SEED_SECRET` | access seed | `seed-secret-local-only` | Derives the seeded tenants' gateway keys |
| `MOCK_UPSTREAM_BASE_URL` | access seed | `http://mock-upstream:8090` | Base URL of the mock upstream channel |
| `OLLAMA_BASE_URL` | access seed | `http://host.docker.internal:11434` | Base URL of the Ollama channel; the default reaches Ollama on the host |
| `OLLAMA_MODEL` | access seed | `qwen3:8b` | Model the Ollama channel serves |
| `CONSOLE_ADMIN_PASSWORD` | access seed | `admin-local-only` | Password of the console user `admin`; unset skips the user |
| `CONSOLE_DEMO_PASSWORD` | access seed | `demo-local-only` | Password of the console user `demo`, of tenant `tenant-demo`; unset skips the user |
| `OLLAMA_CONTEXT_LENGTH` | ollama, with the `ollama` profile only | `64000` | Context the Ollama container asks for; Ollama caps it at the model's maximum |
| `MOCK_FIRST_EVENT_DELAY_MS` | mock-upstream | `0` | Delay before the first stream event |
| `MOCK_EVENT_INTERVAL_MS` | mock-upstream | `0` | Delay between stream events |
| `MOCK_LOAD_EVENTS` | mock-upstream | `200` | Text deltas in a `mock-load` stream |
| `MOCK_LOAD_INTERVAL_MS` | mock-upstream | `100` | Delay between the events of a `mock-load` stream, in place of `MOCK_EVENT_INTERVAL_MS` |
| `MOCK_LOAD_EVENT_BYTES` | mock-upstream | `256` | Bytes of text in each `mock-load` delta |
| `POSTGRES_PASSWORD` | postgres | `gateflow-local-only` | Password of the `gateflow` superuser |
| `SHUTDOWN_DRAIN_DELAY` | every service and the mock upstream; compose leaves it unset | `0s` | Go duration a stopping process keeps serving, with `/readyz` answering 503, before it closes its listener |
| `SHUTDOWN_TIMEOUT` | every service and the mock upstream; compose leaves it unset | `10s` | Go duration a stopping process waits for in-flight requests before it closes them; `stop_grace_period` must exceed the drain delay plus this |
| `*_PORT` | compose | 8080, 8081, 8082, 8090, 5432, 6379; 11434 for `OLLAMA_PORT` | Host ports, bound to localhost only |

## Metrics

The gateway, access and metering each serve Prometheus metrics on `GET /metrics`,
beside `/healthz` on their own port and without a token;
[docs/observability.md](../../docs/observability.md#interim-logging-on-main) lists
what they hold. They are for scrapes inside the stack or the cluster: an ingress must
route only `/v1/messages` and `/v1/responses` to the gateway, and never `/metrics`.
Compose runs no Prometheus yet.

## Seed services

`seed-access` and `seed-metering` run once on every `up`, after `access` and
`metering` are healthy, and create the test tenants `tenant-00` to `tenant-10`, their
keys, the `mock` and `ollama` channels and the wallets (`tenant-00` has no balance),
and for the console the users `admin` and `demo` and the tenant `tenant-demo` with a
wallet but no key;
the gateway starts after them, and a repeated `up` changes nothing.
Each tenant's key, valid only for this local stack, is in the seed log:
`docker compose -p <name> -f deploy/compose/compose.yaml logs seed-access`.

## Ollama profile

The `ollama` service is optional and starts only with `--profile ollama`. It is for
people who do not install Ollama on the host. To make the stack use it, set
`OLLAMA_BASE_URL=http://ollama:11434`. The commands and the model to pull are in
[the OpenCode guide](../../docs/clients/opencode.md#ollama-in-compose).

## Database roles

[`postgres-init.sql`](postgres-init.sql) runs when the Postgres volume is created.
It creates the login roles `access` and `metering`, gives each the schema it owns
(`access` and `accounting`), and closes every schema to everyone else. Services
apply their own migrations at startup through `pkg/pgmigrate`. After changing the
init file, recreate the volume with `down --volumes`.
