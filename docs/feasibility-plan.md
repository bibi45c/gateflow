# Feasibility plan

This plan covers the early feasibility test from the FlowGate proposal (v1.1, 6 October
2026; sections 4.4 and 7.1). The test sends one client flow through all three
services, with persistent reservation and streamed output through the entry point, on
a deployable skeleton. It validates the approach before the rest of the proposal is
built and does not reduce scope: every use case in proposal section 6.1, UC1 to UC15,
remains planned. The simplifications below are temporary and are replaced by the full
proposal design once the approach is validated.

Key dates:

- **21 October 2026, 09:00 to 09:40 SGT:** Team 1's presentation, with an 8-minute
  demo plus 2 minutes of questions. The demo should show significant use cases,
  autoscaling, a scripted performance test and the DevOps pipeline, so stage S3 must
  be finished by then.
- **16 November 2026:** the final report is due.

## Decisions (11 October 2026)

| Topic | During the feasibility test | Full proposal plan |
|---|---|---|
| Validation client | OpenCode, through its Anthropic provider with a custom `baseURL`. It can also exercise OpenAI Responses. | Claude Code CLI and Codex CLI acceptance (UC3) still follows, each run in its own shell |
| Model upstreams | The mock upstream for every capacity, latency, isolation and failure test. A local Ollama model for functional tests with a real model, because Ollama serves both Anthropic Messages and OpenAI Responses natively. Hosted free models may be added for real-model tests through a protocol adapter. | Same split; hosted providers where an acceptance test needs one |
| Environment | Local docker-compose for S0 to S2. From S3, EKS with Terraform, Helm and KEDA, using a plain RDS PostgreSQL database. | Aurora Serverless v2, Argo CD, Cognito and CloudFront (section 5.5) |
| Metering | Reservation and settlement in PostgreSQL transactions. Usage queries read the ledger directly. | Transactional outbox, SQS, idempotent consumers and recovery of pending reservations (UC6, sections 5.1 and 5.4) |
| Repository | One repository with one Go module. Each service keeps private packages under `internal/`. | Same |
| Reviews | PRs merge without a second approval; see CONTRIBUTING.md. | Reviewed merge requests |

## Model upstreams and test accounts

- **Mock upstream.** Capacity and service-quality results come only from the mock
  upstream. It is free and repeatable, and it can simulate long responses, slow
  streams and upstream errors. Results from real models would mix in the provider's
  rate limits and variance.
- **Local Ollama.** Functional tests use a small model that supports tool calling,
  from about 1.5B to 7B parameters, for example a Qwen coder model. Very old models
  are not suitable, because coding clients rely on tool calls. Functional tests send
  few requests, so a slow laptop is acceptable; a teammate's faster machine or the
  Ollama Cloud free tier, which serves the same APIs, can stand in. Ollama can run as
  an optional service in the compose stack; the first model download takes several
  GB.
- **Hosted free models.** Free hosted models, such as the OpenRouter free models or
  OpenCode Zen's Space Bunny Free, may be used for real-model tests. Most of them
  speak OpenAI Chat Completions, which is neither of the gateway's native protocols,
  so they need a channel adapter that converts requests and streamed events. The
  proposal excludes full conversion between arbitrary protocols, so the adapter
  covers only what the tests use. Free availability can change without notice.
- **Test tenants.** Tests need many FlowGate tenants and gateway keys. A seed script
  creates them in the access service, for example ten tenants for the isolation test
  and one shared wallet for the hot-spot test.

## Stages

| Stage | Outcome | Proposal use cases | Proposed owners |
|---|---|---|---|
| S0 Skeleton | Three services and the mock upstream answer health checks. docker-compose runs the stack, and CI builds and tests on three operating systems. | — | M1 to M4 |
| S1 First link | OpenCode calls `POST /v1/messages` through the gateway. Access verifies the gateway key, metering reserves and settles a simulated cost, and the gateway relays SSE output, including tool calls, first from the mock upstream and then from a local Ollama model. | UC1, UC2, UC3 (Messages), UC4, UC5, UC6 | Gateway and mock: M1. Access: M3. Metering: M2. |
| S2 Second protocol | A client completes a coding task through `POST /v1/responses`, against the mock upstream and a local Ollama model. | UC3 (Responses), UC4 | M1, M3 |
| S3 Cloud and scaling | The stack runs on EKS and is deployed by the CI/CD pipeline. k6 load against the mock upstream compares 1, 2 and 4 gateway replicas, and one KEDA autoscaling run is recorded. | UC9, UC10 | M4, with M2 and M3 for the experiments |
| S4 Reuse | Keys and usage can be managed through the API or a minimal console. A reference client, and pi as a second client, reuse the gateway. | UC7, UC8 | M3, M2 |

Owners follow the proposal's division of work and are confirmed per issue.

## After the feasibility test

The remaining proposal work follows the proposal's priorities: Must, then Should,
then Could.

- **Must:**
  - Claude Code CLI and Codex CLI acceptance (UC3), first against a local Ollama
    model.
  - The full metering design: outbox, SQS, idempotent consumers and recovery of
    pending reservations (UC6).
  - The planned cloud services and delivery tooling (section 5.5).
  - The full capacity, isolation and failure experiments (sections 5.6 and 6.2).
  - The management console (UC7).
- **Should:** backup channel switching (UC11), and 8 replicas, 3,000 active requests
  and node start-up tests (UC12).
- **Could:** weighted routing (UC13), CSV export (UC14) and tracing (UC15).

**Not in the proposal:** WebSocket and advanced model features. Section 6.1 lists them
as Won't Have this time.
