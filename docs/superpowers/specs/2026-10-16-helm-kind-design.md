# Issue 71 Helm design

Base and rules: origin/main de208ffb766a70706c108c98fa10e00bce37d266.
Read: AGENTS.md, CONTRIBUTING.md, gateflow-task and gateflow-worktree skills, issue 71 description and every comment, compose manifest/bootstrap SQL, server shutdown implementation and smoke-test entry points.

## Outcome
One chart deploys gateway, access, metering and mock-upstream from public GHCR images. A fresh kind install reaches ready Pods and the existing smoke suite checks both API routes, streaming and database accounting through the ingress. No AWS actions or KEDA changes.

## Approach
Use deploy/helm/gateflow for application workloads; explicit kind and AWS values select environment settings. Avoid bundling cloud provisioning or a PostgreSQL operator into the application chart. A separate local fixture supplies PostgreSQL 17 and an ingress controller on kind. A bundled database would shorten installation but blur ownership of the external RDS lifecycle; separate application charts per environment would duplicate behavior and tests.

## Workloads and configuration
Four Deployments and ClusterIP Services, configurable replicas/resources and immutable GHCR image references. Gateway/access/metering select gateflow.io/workload=services. Mock selects load-generator and tolerates its NoSchedule taint. Kind uses matching worker labels/taint to verify placement. k6 is excluded; document gateway Service DNS for issue 72/load-test callers.

Existing Secret references supply database URLs, internal tokens, key pepper and seed secret; the application chart never renders Secret values. AWS URLs require sslmode=require. Local fixture URLs may use sslmode=disable only for disposable local PostgreSQL. Bootstrap runs with an externally supplied admin Secret and creates service roles/schemas with passwords from Secret keys, without embedding compose passwords. Access and metering seed Jobs run in dependency order after migrations/readiness, with bounded retries and observable failures. No secret contents are printed.

## Traffic and termination
Ingress exposes exactly /v1/messages and /v1/responses; health, metrics and internal APIs stay behind ClusterIP. Kind ingress disables proxy buffering and supports long streaming timeouts. AWS ingress sets idle timeout 300 seconds and deregistration delay; document controller installation values defaultTags Project=gateflow, Environment=s3. Load clients target internal gateway DNS.
Readiness /readyz, liveness /healthz, startup probes for migration startup. Explicit drain delay and timeout, gateway timeout at least 60 seconds, termination grace exceeding preStop duration plus drain plus timeout plus margin. Use Kubernetes native sleep lifecycle action for distroless images, subject to selected kind/EKS API support; avoid shell/curl hooks unavailable in the images.

## Verification and recovery
Add minimal Kubernetes execution options to smoke.py for ledger/hold SQL using kubectl exec and configurable ingress address, preserving compose defaults. Verify chart lint/render, malformed settings, secret references, ingress scope, scheduling and lifecycle relationships. Create isolated kind cluster/kubeconfig, supply disposable local Secrets, install fixture/controller/chart, wait for bootstrap/seeds and ready Pods, run complete smoke suite through ingress, and verify /metrics is inaccessible there. Record exact head/base, versions and outputs, then open PR after required repository checks. Commit each coherent step locally. Recovery is Helm uninstall and deletion of only this disposable kind cluster; never touch existing clusters or AWS.
