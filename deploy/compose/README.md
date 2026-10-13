# Local compose stack

`compose.yaml` runs every FlowGate service with Postgres and Redis. The start and
stop commands are at the top of that file; the checks are in
[docs/testing.md](../../docs/testing.md).

## Configuration

Every service reads its settings from these variables. Compose sets all of them, so
a service issue adds code that reads a variable, not a compose change. The defaults
are local-only test values, not credentials: never reuse them outside this stack.
Override a value in `deploy/compose/.env`, which Git ignores.

| Variable | Read by | Local default | Meaning |
|---|---|---|---|
| `DATABASE_URL` | access, metering | The service's own role on database `gateflow` | Postgres connection; each service logs in as its own role |
| `ACCESS_URL` | gateway, metering | `http://access:8081` | Internal base URL of access |
| `METERING_URL` | gateway | `http://metering:8082` | Internal base URL of metering |
| `ACCESS_INTERNAL_TOKEN` | access (accepts); gateway, metering (send) | `access-token-local-only` | Bearer token for calls to access |
| `METERING_INTERNAL_TOKEN` | metering (accepts); gateway (sends) | `metering-token-local-only` | Bearer token for calls to metering |
| `ACCESS_KEY_PEPPER` | access | `key-pepper-local-only` | Secret mixed into the stored HMAC of each gateway key |
| `ACCESS_SEED_SECRET` | access seed | `seed-secret-local-only` | Derives the seeded tenants' gateway keys |
| `MOCK_UPSTREAM_BASE_URL` | access seed | `http://mock-upstream:8090` | Base URL of the mock upstream channel |
| `OLLAMA_BASE_URL` | access seed | `http://host.docker.internal:11434` | Base URL of the Ollama channel; the default reaches Ollama on the host |
| `OLLAMA_MODEL` | access seed | `qwen3:8b` | Model the Ollama channel serves |
| `OLLAMA_CONTEXT_LENGTH` | ollama, with the `ollama` profile only | `64000` | Context the Ollama container asks for; Ollama caps it at the model's maximum |
| `MOCK_FIRST_EVENT_DELAY_MS` | mock-upstream | `0` | Delay before the first stream event |
| `MOCK_EVENT_INTERVAL_MS` | mock-upstream | `0` | Delay between stream events |
| `POSTGRES_PASSWORD` | postgres | `gateflow-local-only` | Password of the `gateflow` superuser |
| `*_PORT` | compose | 8080, 8081, 8082, 8090, 5432, 6379; 11434 for `OLLAMA_PORT` | Host ports, bound to localhost only |

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
