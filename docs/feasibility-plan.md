# Feasibility plan

This plan schedules FlowGate's delivery from the proposal (v1.1, 6 October 2026),
starting with the proposal's early feasibility test (sections 4.4 and 7.1). It does not
reduce scope: every use case in proposal section 6.1, UC1 to UC15, remains planned.
The simplifications below are temporary and are replaced by the full proposal design
after the feasibility gate.

## Dates

The revised presentation schedule (guidelines V2.8) supersedes the dates in proposal
section 7.2.

| Date (2026) | Milestone |
|---|---|
| 12 October | S0 skeleton merged |
| 14 October | S1 done; feasibility gate |
| 16 October | S2 done |
| 18 October | S3 done |
| 19–20 October | Freeze, record fallback runs, rehearse |
| 21 October, 09:00–09:40 SGT | Team 1 presentation: 8-minute demo plus 2 minutes of questions, showing use cases, autoscaling, a scripted performance test and the DevOps pipeline |
| 16 November | Final report due |

## Feasibility test and gate

The feasibility test is S0 and S1 plus streaming through the ingress, within at most
four team person-days. It covers one real client workflow through the three services,
a durable reservation and a deployable skeleton. Streaming through the ingress is
checked on the EKS load balancer in S3. Until then, this is a recorded deviation.

At the gate on 14 October, the team checks the S1 outcome and confirms versions,
exclusions, test bounds and budget before the second protocol. If the test fails,
scope or estimates are reviewed with the lecturer, as proposal section 7.1 requires.

## Decisions (11 October 2026)

| Topic | Until the gate and during S2–S4 | Full proposal plan |
|---|---|---|
| Validation client | OpenCode (the open-source coding agent), through its Anthropic provider with a custom `baseURL`. Its runs exercise the gateway paths but are not UC3 acceptance. | UC3 acceptance: Claude Code CLI against a Claude service (section 5.2, [R8]), and Codex CLI against a native Responses upstream |
| Model upstreams | The mock upstream for every capacity, latency, isolation, failure and error test. A local Ollama model for functional tests with a real model. Optionally, hosted free models through a protocol adapter. | Same split; hosted providers where an acceptance test needs one |
| Environment | Local docker-compose for S0 to S2. From S3, EKS with Terraform, Helm and KEDA, using a plain RDS PostgreSQL database. | Aurora Serverless v2, Argo CD, Cognito and CloudFront (section 5.5) |
| Metering | Reservation and settlement in PostgreSQL transactions. Usage queries read the ledger directly. | Transactional outbox, SQS, idempotent consumers and recovery of pending reservations (UC6, sections 5.1 and 5.4) |
| Contracts | Public protocol wire types under `pkg/protocol/`. Internal API contracts as `api/<service>.yaml`. | Same |
| Reviews | PRs merge without a second approval; see CONTRIBUTING.md. | Reviewed merge requests |

## Model upstreams

- **Mock upstream.** Capacity, service-quality and error-handling results come only
  from the mock upstream. It is free and repeatable, and it can simulate long
  responses, slow streams, upstream errors and protocol error events.
- **Local Ollama.** Functional tests use a small model that supports tool calling,
  from about 1.5B to 7B parameters, for example a Qwen coder model. Very old models
  are not suitable, because coding clients rely on tool calls. Functional tests send
  few requests, so a slow laptop is acceptable; a teammate's faster machine or Ollama
  Cloud, which serves the same APIs, can stand in.
  - Ollama provides compatibility endpoints, not full parity. Use Ollama 0.13.3 or
    later, which added Responses.
  - Responses is stateless only.
  - The Anthropic endpoint returns HTTP errors instead of streaming error events,
    and has no `tool_choice`.
  - Set `OLLAMA_CONTEXT_LENGTH` to at least 64000, because the small default
    truncates coding-client prompts.
  - Record the Ollama and client versions with each result.
- **Hosted free models.** Free hosted models, such as the OpenRouter free models, may
  be used for real-model tests. Most speak OpenAI Chat Completions, so they need a
  channel adapter that converts requests and streamed events. Proposal section 4.4
  states that "conversion between arbitrary model protocols is outside the scope".
  The adapter is therefore optional test tooling with no use-case budget. Its results
  are not evidence for the native paths in UC3 and UC4. Free availability can change
  without notice.
- **Test tenants.** Tests need many FlowGate tenants. Seed scripts create tenants and
  gateway keys through the access API, and wallets through the metering API, for
  example ten tenants for the isolation test and one shared wallet for the hot-spot
  test.

## Stages

| Stage | Outcome | Proposal use cases | Proposed owners |
|---|---|---|---|
| S0 Skeleton | Three services and the mock upstream answer health checks. docker-compose runs the stack, and CI builds and tests on three operating systems. | — | M1 |
| S1 First link | OpenCode calls `POST /v1/messages` through the gateway. Access verifies a seeded gateway key, metering reserves and settles a simulated cost, and the gateway relays SSE output, including tool calls, first from the mock upstream and then from a local Ollama model. | Partial: UC1 key check, UC2 seeded channel, UC4 Messages path, UC5 single channel, UC6 reserve and settle | Gateway and mock: M1. Access: M3. Metering: M2. |
| S2 Second protocol | A client completes a coding task through `POST /v1/responses`, against the mock upstream and a local Ollama model. | Partial: UC4 Responses path | M1, M3 |
| S3 Cloud and scaling | The stack runs on EKS and is deployed by the CI/CD pipeline. k6 load against the mock upstream compares 1, 2 and 4 gateway replicas after single-Pod calibration, one KEDA autoscaling run is recorded, and streaming through the ingress is checked. Prerequisites: a cost estimate and authorised account (section 7.3), ECR image publishing, an ALB ingress, Redis, and a Prometheus active-request metric with one dashboard. | Partial: UC9 replica comparison and autoscaling, UC10 deployment | M4, with M2 and M3 for the experiments |
| S4 Reuse | Keys and usage can be managed through the API or a minimal console. A reference client, and the pi coding-agent CLI as a second client, reuse the gateway. | Partial: UC7, UC8 | M3, M2 |

Owners follow the proposal's division of work and are confirmed per issue. The S3
results record that the database is plain RDS rather than Aurora with fixed capacity
(section 5.6).

## Presentation preparation

These tasks run alongside S2 and S3:

- Logical, physical and persistence views, and the design of one representative use
  case. They show the architecture as built, including the deviations above.
- Slides.
- Recorded fallback runs of the demo.
- Rehearsal on 19 and 20 October.
- Per-member effort records, which also feed the fortnightly reports.

## After the feasibility test

The remaining proposal work follows the proposal's priorities: Must, then Should,
then Could.

- **Must:**
  - UC1: key creation and revocation, including the 30-second revocation bound.
  - UC2: channel, capability and price registration.
  - UC3: Claude Code CLI and Codex CLI acceptance.
  - UC5: routing across approved channels with explicit error classes.
  - UC6: the full metering design, with outbox, SQS, idempotent consumers and
    recovery of pending reservations.
  - UC7: the management console.
  - UC9: tenant isolation and the full capacity experiments.
  - UC10: rollback, and releasing one service while the other two keep their images.
  - Section 6.4: minimum security.
  - Section 5.5: the planned cloud services and delivery tooling.
  - Sections 5.6 and 6.2: the full experiments.
- **Should:** backup channel switching (UC11), and 8 replicas, 3,000 active requests
  and node start-up tests (UC12).
- **Could:** weighted routing (UC13), CSV export (UC14) and tracing (UC15).

**Excluded from the core plan:** WebSocket and advanced model features (proposal
sections 6.1 and 5.2).
