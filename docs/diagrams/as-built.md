# As-built architecture views

These views show FlowGate as it is built on `main`, not as the proposal planned it.
They are drawn from the code, and every diagram names the files it was checked
against, so a reviewer can compare the two. Where the build differs from the
proposal, the difference is listed under [Deviations](#7-deviations-from-the-proposal).

- **Snapshot:** `main` at `de208ff` (15 October 2026). The first version was drawn at
  `31d5c4a` (14 October). The views are checked again at the freeze on 19 October,
  before they go into the slides (#17).
- **Scope:** S1 and S2 as built: `POST /v1/messages` and `POST /v1/responses` through
  the gateway, key verification by model and protocol, cost hold and settlement, and
  Prometheus metrics on every service. The S3 cloud environment is written as
  Terraform and the images are published, but nothing is deployed yet; the cloud
  view marks what is *planned*.
- **Legend:** solid lines and boxes are built and on `main`; dashed ones are planned
  or present but unused.
- The earlier diagrams in [index.html](index.html) were drawn from the plans before
  the code existed. Where the two disagree, this page follows the code.

Contents:

1. [Logical view](#1-logical-view)
2. [Context map](#2-context-map)
3. [Physical view: local stack](#3-physical-view-local-stack)
4. [Physical view: cloud](#4-physical-view-cloud)
5. [Persistence view](#5-persistence-view)
6. [Use case design: reserve and settle one call](#6-use-case-design-reserve-and-settle-one-call)
7. [Deviations from the proposal](#7-deviations-from-the-proposal)

## 1. Logical view

Three business services, each with its own process, configuration and data. Coding
tools call only the gateway. The gateway calls access and metering through their
internal HTTP APIs, and metering calls access for prices. No service reads another
service's tables.

```mermaid
flowchart TB
  subgraph clients["Coding tools"]
    OC["OpenCode<br/>Anthropic provider"]
    CX["Codex CLI<br/>Responses provider"]
  end

  subgraph gw["gateway :8080"]
    RL["relay<br/>Messages and Responses:<br/>request checks, SSE relay,<br/>usage and outcome"]
    AD["admission<br/>verify, channel check,<br/>reserve, settle"]
    RL --> AD
  end

  subgraph ac["access :8081"]
    AAPI["httpapi<br/>keys/verify, price-versions"]
  end

  subgraph me["metering :8082"]
    MAPI["api<br/>reservations, settle"]
    BL["billing<br/>max cost, charge"]
    PC["price cache<br/>(in process)"]
    MAPI --> BL
    MAPI --> PC
  end

  subgraph up["Model upstreams"]
    MU["mock-upstream :8090"]
    OL["Ollama :11434"]
  end

  subgraph stores["Data stores"]
    subgraph db["PostgreSQL 17, database gateflow"]
      SA[("schema access")]
      SC[("schema accounting")]
    end
    RD[("Redis<br/>runs in compose,<br/>no service uses it yet")]
  end

  OC -->|"POST /v1/messages<br/>gateway key"| RL
  CX -->|"POST /v1/responses<br/>gateway key"| RL
  AD -->|"POST /internal/v1/keys/verify<br/>key, model, protocol"| AAPI
  AD -->|"POST /internal/v1/reservations<br/>and .../settle"| MAPI
  PC -->|"GET /internal/v1/price-versions/{id}"| AAPI
  RL -->|"same endpoint as the client<br/>body unchanged"| MU
  RL -->|"same, for the ollama channel"| OL
  AAPI --> SA
  MAPI --> SC

  PR["Prometheus<br/>(planned, S3)"]
  PR -.->|"GET /metrics"| gw
  PR -.-> ac
  PR -.-> me

  classDef planned stroke-dasharray:5 5
  class RD,PR planned
```

| Call | Authentication | Timeout and retries |
|---|---|---|
| tool → gateway, `/v1/messages` or `/v1/responses` | gateway key in `x-api-key` or `Authorization: Bearer` | none on the reply; upstream connect 10 s |
| gateway → access, verify | `Bearer ACCESS_INTERNAL_TOKEN`; the body names the protocol for Responses, and a missing protocol means Messages | 2 s, no retry |
| gateway → metering, reserve | `Bearer METERING_INTERNAL_TOKEN` | 2 s, retried once on a timeout, 5xx or connection error |
| gateway → metering, settle | `Bearer METERING_INTERNAL_TOKEN` | 2 s per attempt, up to 2 retries, with a fresh context |
| metering → access, price version | `Bearer ACCESS_INTERNAL_TOKEN` | 1 s; each version is cached for the life of the process |
| gateway → upstream, Messages | none for the mock and Ollama; otherwise the channel's `credential_env` value as `x-api-key` | connect 10 s; forwards only `content-type`, `anthropic-version` and `anthropic-beta` |
| gateway → upstream, Responses | the same credential as `Authorization: Bearer` | connect 10 s; forwards only `content-type` |
| scraper → every service, `GET /metrics` | none; for scrapes inside the cluster only, never routed by the ingress | — |

Every internal call carries `X-Request-Id`, the gateway's `req_<uuid>`, and every
internal error uses the body in [api/errors.md](../../api/errors.md).

Checked against: `services/gateway/main.go`,
`services/gateway/internal/admission/admission.go`, `.../admission/settle.go`,
`.../internalapi/caller.go`, `.../relay/relay.go`, `.../relay/protocol.go`,
`.../relay/messages.go`, `.../relay/responses.go`, `.../relay/upstream.go`,
`.../accessclient/client.go`, `services/access/main.go`,
`services/metering/main.go`, `services/metering/internal/accessclient/accessclient.go`,
`services/access/internal/httpapi/httpapi.go`.

## 2. Context map

Five logical contexts map to three deployed services, as proposal section 5.3 planned.
Each arrow points from the upstream context (U), which publishes a contract, to the
downstream context (D), which uses it. The internal contracts are an *Open Host
Service* with a *Published Language*: [api/access.yaml](../../api/access.yaml),
[api/metering.yaml](../../api/metering.yaml) and [api/errors.md](../../api/errors.md).

```mermaid
flowchart LR
  subgraph prod["Producers"]
    OP["Platform operator<br/>registers channels and prices<br/>(S1: seed command)"]
  end

  subgraph accessSvc["access service"]
    TA["Tenant and access<br/>Tenant, ApiKey"]
    CC["Channel catalogue<br/>Channel by protocol,<br/>ChannelModel, PriceVersion"]
  end

  subgraph gatewaySvc["gateway service"]
    RR["Routing and relay<br/>live call, outcome"]
  end

  subgraph meteringSvc["metering service"]
    AC["Accounting<br/>Wallet, Reservation,<br/>LedgerEntry"]
    UA["Usage analytics<br/>(planned, UC7)"]
  end

  subgraph cons["Consumers"]
    DEV["Developers' coding tools<br/>OpenCode, Codex"]
  end

  EXT["Model providers<br/>Anthropic Messages,<br/>OpenAI Responses"]

  OP --> CC
  TA -->|"U: OHS keys/verify<br/>D: gateway"| RR
  CC -->|"U: OHS price-versions<br/>D: accounting"| AC
  AC -->|"U: OHS reservations, settle<br/>D: gateway"| RR
  EXT -->|"U: provider protocol<br/>D: conformist"| RR
  RR -->|"U: native model APIs<br/>D: tools"| DEV
  AC -.-> UA

  classDef planned stroke-dasharray:5 5
  class UA planned
```

| Context | Service | Owns | Relationship |
|---|---|---|---|
| Tenant and access | access | `tenants`, `api_keys`, `api_key_channels` | Upstream of the gateway through `keys/verify` |
| Channel catalogue | access | `channels`, `channel_models`, `price_versions` (immutable) | Upstream of accounting through `price-versions` |
| Routing and relay | gateway | no tables; the live call only | Downstream of access and accounting. Conformist to the provider protocols: the body goes upstream unchanged, and the wire types in `pkg/protocol/` are a shared kernel of technical types only, with no business rules |
| Accounting | metering | `wallets`, `reservations`, `ledger_entries` | Upstream of the gateway through reserve and settle |
| Usage analytics | metering (planned) | reads the ledger | Planned with UC7; no API yet |

The `test/archtest` package fails the build when one service imports another
service's packages, so the boundaries above hold in the code as well as at runtime.

Checked against: `services/*/internal/`, `pkg/protocol/`, `test/archtest/`,
`api/*.yaml`, `services/access/internal/seed/seed.go`.

## 3. Physical view: local stack

The local stack is `deploy/compose/compose.yaml`. Every port binds to `127.0.0.1`
only. An arrow means "starts after": the lower box waits until the upper one is
healthy, or, for the seed steps, has finished.

```mermaid
flowchart TB
  subgraph host["Developer machine: Docker, ports on 127.0.0.1"]
    PG[("postgres:17 :5432<br/>roles access, metering<br/>from postgres-init.sql")]
    RD[("redis:8 :6379<br/>not used by any service yet")]
    AC["access :8081"]
    ME["metering :8082"]
    SA["seed-access<br/>one-off: tenants, keys,<br/>channels, pv-seed-1"]
    SM["seed-metering<br/>one-off: wallets"]
    MU["mock-upstream :8090"]
    GW["gateway :8080"]
    OLC["ollama :11434<br/>optional, profile ollama, CPU"]
  end
  OLH["Ollama on the host :11434<br/>host.docker.internal"]

  PG --> AC
  PG --> ME
  AC --> ME
  AC --> SA
  ME --> SM
  SA --> SM
  RD --> GW
  AC --> GW
  ME --> GW
  MU --> GW
  SM --> GW
  GW -.->|"ollama channel<br/>OLLAMA_BASE_URL"| OLH
  GW -.-> OLC

  classDef optional stroke-dasharray:5 5
  class OLC,OLH,RD optional
```

- Each service image is built from one `deploy/docker/Dockerfile` and runs as a
  distroless, non-root binary. The binary checks its own `/healthz` for the health
  checks, because the image has no shell or curl.
- Containers stop with a 15 s grace period, longer than the services' 10 s drain.
- The seed steps run on every `up` and change nothing when the data exists. Keys are
  printed only in the `seed-access` log.

Checked against: `deploy/compose/compose.yaml`, `deploy/compose/postgres-init.sql`,
`deploy/docker/Dockerfile`, `pkg/server/server.go`.

## 4. Physical view: cloud

*Written, not deployed.* The S3 environment exists as Terraform on `main` (#90), and
CI publishes a versioned image of every service (#83), but nothing has been applied
to AWS yet. Only bibi45c runs `terraform apply`, after the spend is approved. The
Helm chart (#71), KEDA with Prometheus (#72) and the deployment script (#76) are
still to come, so the Pods, the ingress and the scaling are dashed.

```mermaid
flowchart LR
  T["Coding tools,<br/>k6 load"] -.-> ALB
  subgraph aws["AWS ap-southeast-1, one VPC across two AZs (Terraform, not applied)"]
    subgraph pub["Public subnets"]
      ALB["ALB<br/>load balancer controller IAM"]
      NAT["one NAT gateway"]
    end
    subgraph priv["Private subnets"]
      subgraph eks["EKS 1.35"]
        SVC["node group services<br/>2 × m6i.xlarge"]
        GEN["node group load-generator<br/>1 × c6i.xlarge, tainted"]
        PODS["gateway, access, metering,<br/>mock-upstream Pods<br/>(planned, Helm #71)"]
        KEDA["KEDA and Prometheus<br/>(planned, #72)"]
      end
      RDS[("RDS PostgreSQL 17<br/>db.t4g.small<br/>schemas access, accounting")]
    end
  end
  GHCR[("GHCR<br/>ghcr.io/bibi45c/gateflow-*<br/>tagged by commit, public")]
  GA["GitHub Actions<br/>images.yml on push to main"]

  ALB -.-> PODS
  PODS -.-> RDS
  SVC -.-|"runs on"| PODS
  KEDA -.->|"scales the gateway"| PODS
  GA --> GHCR
  GHCR -.->|"anonymous pull"| PODS

  classDef planned stroke-dasharray:5 5
  class ALB,NAT,SVC,GEN,RDS,PODS,KEDA,T planned
```

- **Built:** the image pipeline. Every push to `main` publishes four images tagged with
  the commit, with OCI source and revision labels, and records their digests in the
  job summary; pull requests build without pushing. The images pull without a login
  (being checked on team machines in #21).
- **Written and tested without AWS:** the Terraform modules `network`, `cluster` and
  `database`, checked in CI with `terraform validate` and `terraform test` against
  mocked providers. Redis is left out, so no ElastiCache is created.
- **Planned:** applying it, the workloads, autoscaling and the scripted deployment with
  rollback.

Checked against: `deploy/terraform/` (`main.tf`, `variables.tf`, `modules/*`, `README.md`),
`.github/workflows/images.yml`, `deploy/docker/README.md`, #21.

## 5. Persistence view

One PostgreSQL database, `gateflow`, holds two schemas. Each schema is owned by its
service's own login role, and every schema, `public` included, is closed to every
other role, so the access role cannot read accounting tables and the reverse. Each
service applies its own migrations at startup through `pkg/pgmigrate`. There are no
cross-schema foreign keys or joins: `tenant_id` matches between the schemas by value
only.

### Schema access (role access)

```mermaid
erDiagram
  tenants ||--o{ api_keys : "owns"
  price_versions ||--o{ api_keys : "prices"
  api_keys ||--o{ api_key_channels : "may use"
  channels ||--o{ api_key_channels : "granted to"
  channels ||--o{ channel_models : "serves"
  tenants {
    text id PK "tenant-00 to tenant-10"
    text name
    timestamptz created_at
  }
  api_keys {
    text id PK
    text tenant_id FK
    bytea key_hmac UK "HMAC-SHA256 with the pepper, 32 bytes"
    text price_version_id FK
    timestamptz revoked_at "null unless revoked"
    timestamptz created_at
  }
  price_versions {
    text id PK "pv-seed-1"
    bigint input_micro_per_token "0 to 1000000"
    bigint output_micro_per_token "0 to 1000000"
    timestamptz created_at "a trigger refuses update and delete"
  }
  api_key_channels {
    text key_id PK
    text channel_id PK
  }
  channels {
    text id PK "mock, mock-responses, ollama, ollama-responses"
    text protocol "anthropic-messages or openai-responses"
    text base_url "scheme, host, optional port"
    text credential_env "empty, or UPSTREAM_CREDENTIAL_*"
    timestamptz created_at
  }
  channel_models {
    text channel_id PK
    text model_pattern PK "exact name or prefix*"
  }
```

- The raw key is never stored; a lookup hashes the presented key and uses the unique
  index on `key_hmac`.
- A credential is only allowed over `https`: a check constraint refuses a
  `credential_env` on an `http` channel.
- Migration `0002` widened the protocol check to `anthropic-messages` and
  `openai-responses`, without changing a row. The seed gives each upstream a Messages
  channel and a Responses twin (`mock-responses`, `ollama-responses`) that serve the
  same models, and verify picks among the key's channels that speak the requested
  protocol.

### Schema accounting (role metering)

```mermaid
erDiagram
  wallets ||--o{ reservations : "holds"
  reservations ||--o| ledger_entries : "settles into"
  wallets {
    text tenant_id PK "same value as access tenants id"
    bigint balance "100000000 seeded, tenant-00 has 0"
    bigint held "open holds, balance minus held never below 0"
    timestamptz updated_at
  }
  reservations {
    text reservation_id PK
    text call_id UK "unique with attempt_id"
    integer attempt_id "1 in S1"
    text tenant_id FK
    text price_version_id
    bigint input_price "copied at reserve"
    bigint output_price "copied at reserve"
    bigint max_input_tokens "request body bytes"
    bigint max_output_tokens "max_tokens"
    bigint max_cost
    text status "reserved or settled"
    bytea reserve_digest "replay check"
    bigint input_tokens "null until settled, null if unknown"
    bigint output_tokens "null until settled, null if unknown"
    text outcome "null until settled"
    bigint charged "never above max_cost"
    bytea settle_digest "replay check"
    timestamptz settled_at
    timestamptz created_at
  }
  ledger_entries {
    text ledger_entry_id PK
    text reservation_id FK, UK "one entry per reservation"
    text call_id
    integer attempt_id
    text tenant_id
    bigint amount "the charge, may be 0"
    bigint input_tokens "as sent, nullable"
    bigint output_tokens "as sent, nullable"
    text outcome
    timestamptz created_at "a trigger refuses update and delete"
  }
```

- Check constraints keep a `reserved` row free of settlement values, and require a
  `settled` row to have an outcome, a charge and a time.
- `ledger_entries` is append-only: a correction is a new entry.

Checked against: `services/access/migrations/0001_init.sql`, `.../0002_channel_protocols.sql`,
`services/access/internal/seed/seed.go`, `services/access/internal/store/queries.go`,
`services/metering/migrations/0001_init.sql`, `deploy/compose/postgres-init.sql`,
`pkg/pgmigrate/pgmigrate.go`, `services/metering/main.go`.

## 6. Use case design: reserve and settle one call

The representative use case is **UC6, reserve and settle simulated usage**, as it
runs inside **UC4**, one `POST /v1/messages` call. The `/v1/responses` call follows
the same steps; its differences are listed below the sequence. It crosses all three services and
both schemas, and it carries most of the S1 rules: the order of checks, the
idempotent internal calls and the charge rule.

### Sequence

```mermaid
sequenceDiagram
  autonumber
  participant T as Coding tool
  participant G as gateway
  participant A as access
  participant M as metering
  participant DB as accounting schema
  participant U as Upstream

  T->>G: POST /v1/messages, gateway key
  Note over G: Checks in order: body at most 2 MiB (413),<br/>key present (401), JSON, model,<br/>max_tokens 1 to 32768 (400)
  G->>A: POST /internal/v1/keys/verify {key, model}
  A-->>G: 200 tenant_id, key_id, price_version_id, channel
  Note over G: Channel check: base_url form,<br/>credential variable set (else 500, no hold)
  G->>M: POST /internal/v1/reservations<br/>call_id = request id, attempt_id 1,<br/>max_input_tokens = body bytes, max_output_tokens
  M->>DB: find (call_id, attempt_id): a replay returns the record
  M->>A: GET /internal/v1/price-versions/{id} (cached)
  A-->>M: input and output price
  M->>DB: one transaction: insert reservation,<br/>held = held + max_cost if balance - held covers it
  M-->>G: 201 reservation_id, max_cost
  G->>U: POST /v1/messages, body unchanged
  U-->>G: SSE events
  G-->>T: each event relayed as it arrives
  Note over G: Usage from message_start and message_delta.<br/>Outcome by the first rule that matches.
  G->>M: POST /internal/v1/reservations/{id}/settle<br/>{input_tokens, output_tokens, outcome}, fresh context
  M->>DB: one transaction: lock the reservation,<br/>insert one ledger entry, mark it settled,<br/>held = held - max_cost, balance = balance - charged
  M-->>G: 200 charged, ledger_entry_id
```

Refusals end the call before the upstream is reached, and leave no ledger entry:

| Where | Internal answer | What the tool gets |
|---|---|---|
| verify | 401 `invalid_key` | 401 `authentication_error` |
| verify | 403 `model_not_allowed` | 403 `permission_error` |
| reserve | 409 `insufficient_balance`, 404 `wallet_not_found` | 402 `billing_error` |
| verify or reserve | timeout, unreachable, `dependency_unavailable` | 503 `api_error` |
| anything else | any other code | 500 `api_error`, logged |

### The same flow on `/v1/responses`

`POST /v1/responses` (S2, #65) runs the same admission, reserve, relay and settle
steps; only the wire format differs:

| Step | `/v1/messages` | `/v1/responses` |
|---|---|---|
| Verify | protocol left out, so `anthropic-messages` | protocol `openai-responses`, so a `*-responses` channel |
| Output bound held | `max_tokens`, required, 1 to 32,768 | `max_output_tokens`, 1 to 32,768; when absent, the gateway sets it to 32,768 in the body it forwards and holds that |
| Refused before admission | — | `previous_response_id`, with 400: the gateway keeps no response state |
| Usage source | `message_start` and `message_delta`, cache tokens added to input | `response.completed` usage; cached input tokens are part of `input_tokens` |
| Public errors | Anthropic error body | OpenAI error body |

### Reservation states

```mermaid
stateDiagram-v2
  [*] --> reserved: reserve, hold max_cost
  reserved --> settled: settle, charge and release the rest
  reserved --> reserved: reserve replay, same content
  settled --> settled: settle repeat, same content
  reserved --> [*]: never settled, hold stays until UC6 recovery
  note right of settled
    A replay with different content gets
    422 idempotency_conflict
  end note
```

### Charge rule

`max_cost = max_input_tokens × input price + max_output_tokens × output price`. At
settlement, an unknown count is replaced by its maximum, and
`charged = min(max_cost, input × input price + output × output price)`.
`upstream_error` and `not_dispatched` charge 0. With `pv-seed-1` (3 and 15
micro-units per token), a call with 6,328 input and 449 output tokens is charged
6,328 × 3 + 449 × 15 = 25,719.

| Outcome | When (first rule that matches) | Charge |
|---|---|---|
| `not_dispatched` | no upstream request was sent | 0 |
| `completed` | `message_stop` arrived, or a full 2xx body was read | counts as reported |
| `client_cancelled` | the client disconnected first | unknown counts at their maximum |
| `upstream_error` | the upstream answered non-2xx before any output | 0 |
| `interrupted` | anything else, such as a dropped stream | unknown counts at their maximum |

Checked against: `services/gateway/internal/relay/relay.go`, `.../relay/request.go`,
`.../relay/result.go`, `services/gateway/internal/admission/`,
`services/metering/internal/api/`, `services/metering/internal/billing/billing.go`,
`services/metering/internal/store/reservations.go`, `services/gateway/internal/relay/responses.go`,
[api/metering.yaml](../../api/metering.yaml).

## 7. Deviations from the proposal

Recorded in #18, the gate record (#20) and the [feasibility plan](../feasibility-plan.md).

| Proposal | As built | Why |
|---|---|---|
| Transactional outbox, SQS and idempotent consumers for usage views (sections 5.1, 5.4) | Reservation and settlement in PostgreSQL transactions only; no outbox, no queue | Keeps S1 small; planned with UC6 after the gate |
| A pending reservation waits for recovery when usage is unknown | An unknown count is charged at its maximum; a hold whose settle fails stays held until UC6 | No recovery process in S1 |
| Redis for limits and the key cache | Redis runs in compose, but no service uses it; no key cache | Not needed until rate limits or caching land |
| Keys and channels created through APIs (UC1, UC2) | Seed commands write each service's own schema | The key and channel APIs come later |
| Separate prices for cache and reasoning tokens | Cache tokens are charged at the full input price, on Messages and on Responses (where cached tokens are inside `input_tokens`); thinking tokens arrive in `output_tokens` | One price pair per version in S1 |
| Conversation state through the gateway | `/v1/responses` is stateless: `previous_response_id` is refused, and clients resend the conversation | The gateway keeps no response state |
| Per-caller internal identities | One shared token for access; metering trusts the gateway's `tenant_id` and `price_version_id` | mTLS and workload identity are out of S1 |
| Claude Code and Codex CLI as the acceptance clients (UC3) | OpenCode on Messages; Codex CLI on Responses (guide #103) | Claude Code needs a Claude service |
| Aurora, Argo CD, Cognito and CloudFront (section 5.5) | Plain RDS PostgreSQL in Terraform, images in GHCR instead of ECR, a scripted deployment (planned, #76) | Cost and time |
| Distributed tracing (UC15) | Prometheus metrics and JSON logs only (`docs/observability.md`) | Tracing is optional and its overhead unmeasured |
