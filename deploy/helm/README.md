# FlowGate Helm deployment

The application chart deploys four GHCR services. PostgreSQL, existing Kubernetes
Secrets and an ingress controller must exist before installation. No AWS resources,
Redis or KEDA are created; with autoscaling on, KEDA and Prometheus are installed
first (see [Autoscaling](#autoscaling)). Kubernetes >=1.33 is required for native
sleep preStop.

## Local kind rehearsal

Install Docker, Helm 3, Python >=3.13, kind 0.33.0 and kubectl 1.35 on PATH.
The rehearsal pins kind's Kubernetes 1.35.8 node digest and ingress-nginx
controller-v1.15.1. ingress-nginx is used only in the disposable local rehearsal;
AWS uses the AWS Load Balancer Controller. The three-node cluster needs sufficient
Docker memory for its control plane, two workers and PostgreSQL.

Optionally install checksum-verified pinned binaries into an ignored local directory
with `python deploy/helm/toolchain.py --directory .local/bin`, then add that folder
to this terminal's PATH. CI uses this installer as well.

From the repository root:

```text
python -m unittest discover -s deploy/helm/tests -v
helm lint deploy/helm/gateflow
helm template gf deploy/helm/gateflow --kube-version 1.35.0
python deploy/helm/rehearse.py --cluster gateflow71 --kubeconfig .local/kubeconfig
```

The helper never uses the default kubeconfig. It creates a dedicated cluster,
labels service and load-generator workers and taints the generator, creates
random local-only database/token Secrets, installs disposable PostgreSQL 17 and
ingress, then runs Helm and the existing full smoke suite. Requests go through
port-forwarded ingress on 127.0.0.1:18071; ledger/reservation reads use kubectl exec
inside the local PostgreSQL Pod. /metrics must return 404 through ingress.
Credentials are persisted next to the supplied kubeconfig for repeat runs; put it
in an ignored folder such as .local. Do not publish the kubeconfig or credentials.
The fixture has no persistent volume; deleting its Pod loses its database.
Like the RDS master role, the bootstrap Job connects as gateflow_admin, a LOGIN
CREATEROLE non-superuser that owns the gateflow database; the container superuser
has its own password in gateflow-postgres. Missing keys are added to an older
credentials file, but a PostgreSQL Pod created by an older helper must be deleted
(or the cluster recreated) before the next run.

To repeat against this exact local cluster (also exercises Helm upgrade):

```text
python deploy/helm/rehearse.py --cluster gateflow71 --kubeconfig .local/kubeconfig --existing-cluster
kubectl --kubeconfig .local/kubeconfig --context kind-gateflow71 -n gateflow get pods -o wide
```

Only clean up the cluster created for this rehearsal:

```text
helm --kubeconfig .local/kubeconfig --kube-context kind-gateflow71 uninstall gf -n gateflow
kind delete cluster --name gateflow71 --kubeconfig .local/kubeconfig
```

## Chart inputs and Jobs

Set imageTag to a published full commit SHA or set services.<name>.digest to pin
an OCI digest. The kind fixture and chart use the same GHCR images; no login is
required. Per-service replicas and resources are configurable.

Create Secrets in the release namespace before Helm install:

| Secret (default name) | Required keys |
|---|---|
| gateflow-runtime | ACCESS_DATABASE_URL, METERING_DATABASE_URL, ACCESS_INTERNAL_TOKEN, METERING_INTERNAL_TOKEN, ACCESS_KEY_PEPPER, ACCESS_SEED_SECRET |
| gateflow-bootstrap | PGHOST, PGPORT, PGDATABASE, PGUSER, PGPASSWORD, PGSSLMODE, ACCESS_PASSWORD, METERING_PASSWORD |

URLs use the access and metering roles respectively; role passwords must match the
bootstrap keys. Names can be overridden with secretName/bootstrapSecretName. Secret
values are never chart values or rendered resources. Use the team's secret delivery
process for AWS, not the local fixture. Plain environment values and rendered YAML
contain no credentials.

Bootstrap runs as a pre-install/pre-upgrade hook and requires an existing database
and admin role able to create/manage the two service roles and schemas (RDS master
role, not a service login). A transaction advisory lock serializes bootstrap; it
creates missing roles/schemas, applies the supplied role passwords and revokes
public access. It does not create/drop the database. Applications migrate their
own schemas. With Helm --wait, post-install/post-upgrade access seed runs after ready
Deployments and finishes before metering seed. Deadlines/backoff bound failures.
Successful hook Jobs are removed; failed ones remain for diagnosis. Retry with Helm
upgrade after repairing the cause; before-hook-creation replaces prior hook Jobs.

Access seeding requires Ollama channel settings even when testing only mock models.
seedChannels.ollamaBaseURL and seedChannels.ollamaModel default to a valid placeholder
channel at http://ollama:11434 and qwen3:8b. No Ollama workload is installed; real
Ollama requests require an operator-supplied reachable endpoint/model.

The existing access seed command prints derived gateway API keys to its Job log.
Treat those logs as credentials: restrict access and never paste them into PRs or
issue evidence. The local helper uses the known disposable seed secret required
by the smoke suite. Use a private seed secret for a real deployment.

## AWS handoff (operator only)

Use values-aws.yaml and the existing RDS PostgreSQL 17 endpoint. Both service URLs
must explicitly contain sslmode=require; bootstrap PGSSLMODE must be require.
The cloud workload init checks verify URL policy and an actually encrypted session
before application startup. The kind-only fixture uses sslmode=disable against its
local database and is never installed in AWS.

The chart selects gateflow.io/workload=services for gateway/access/metering and
bootstrap/seeds. Mock upstream selects gateflow.io/workload=load-generator and
tolerates gateflow.io/workload=load-generator:NoSchedule. Planned service capacity
is 2 x m6i.xlarge; this chart does not resize nodes.

Install/configure the AWS Load Balancer Controller separately with
alb-controller-values.yaml (defaultTags Project=gateflow, Environment=s3), alongside
the cluster/IRSA settings supplied by #30. Ingress resource tags are also explicit.
The public ALB exposes exactly /v1/messages and /v1/responses, uses IP targets,
300-second idle timeout, 60-second deregistration delay and /readyz health checks.
Health/metrics/internal APIs are not Ingress routes; their Services are ClusterIP.
An operator must separately verify ALB behavior; kind does not prove AWS behavior.

Load generators inside the cluster should target http://gf-gateway:8080 (same
namespace), or http://gf-gateway.<namespace>.svc.cluster.local:8080. Public ALB is
only for the ingress streaming check; load should not traverse NAT and return.
k6 Jobs belong to their separate issue; KEDA is described under Autoscaling.

Defaults: preStop 5 seconds, drain 10 seconds, shutdown timeout 60 seconds and
margin 10 seconds, with total Pod grace 85 seconds. All four processes receive
SHUTDOWN_DRAIN_DELAY and SHUTDOWN_TIMEOUT; readiness is /readyz and liveness is
/healthz. Native sleep works with the distroless images. Values that cannot fit the
shutdown budget are rejected at render time.

## Autoscaling

With `autoscaling.enabled=true` the chart renders a KEDA ScaledObject for the gateway
(#72), following [KEDA demand and missing metrics](../../docs/observability.md#keda-demand-and-missing-metrics).
It is off by default, so the smoke rehearsal is unchanged; values-aws.yaml turns it on.
The Deployment then has no fixed `replicas`: KEDA's HPA, named after the gateway
Deployment, owns the count between `minReplicas` 2 and `maxReplicas` 4, with a
60-second scale-down stabilization window. Turning autoscaling on in an existing
release resets the gateway to one replica until the HPA raises it to two; switch it
in a quiet period.

Two Prometheus triggers, both `AverageValue` with the same threshold T:

- active admitted requests: `sum(gateflow_gateway_active_admitted_requests{job=...})`;
- eligible demand: `sum(rate(gateflow_gateway_eligible_arrivals_total{job=...}[1m]))`
  multiplied by D.

Neither divides by the replica count; the HPA does. Each query also requires, with
`and on()`, that every discovered gateway target is up (`min(up) == 1`), was
scraped within `maxSampleAgeSeconds`, and carries its own series (`absent(up unless
on(instance) ...)`, which also covers a new Pod whose rate has fewer than two
samples). If any check fails, or Prometheus does not answer, the query is empty or
fails; with `ignoreNullValues: "false"` KEDA reports an error, not zero, and the HPA
does not scale down on that metric. Only complete, fresh, idle targets give a real
zero. There is no `or vector(0)` and no KEDA fallback. A Pod that has not started
its process yet (phase Pending) is not a target; a draining Pod still is, because
its streams remain demand. While a new Pod's first scrapes arrive, the demand
trigger is briefly unavailable; the active trigger still scales up.

Where to change the settings:

| Value | Meaning | Default | values-kind.yaml | values-aws.yaml |
|---|---|---|---|---|
| `autoscaling.targetPerReplica` | T, 0.7 x the per-Pod admission cap C | 70 | 4 | 70, provisional until #73 |
| `autoscaling.requestDurationSeconds` | D, the mean request duration in seconds | 20 | 20 (mock-load) | 20, provisional until #73 |
| `autoscaling.minReplicas`, `maxReplicas` | Replica bounds | 2, 4 | | |
| `autoscaling.scaleDownStabilizationSeconds` | HPA scale-down window | 60 | | |
| `autoscaling.serverAddress`, `job`, `rateWindow`, `maxSampleAgeSeconds` | Prometheus address, scrape job, rate window, freshest sample age | prometheus-server.monitoring, gateflow-gateway, 1m, 30 | | |

`active` stays raised until metering settles a call, so a slow metering service
makes it exceed the streams actually open. That only errs towards scaling up (at
most to four replicas) and delays scale-down; it never hides demand. There is no
capacity gate yet, so eligible arrivals equal admitted ones.

### Installing Prometheus and KEDA

`monitoring/versions.json` pins both charts: prometheus-community `prometheus`
27.11.0 and kedacore `keda` 2.20.2. [KEDA 2.20](https://keda.sh/docs/2.20/operate/cluster/)
lists Kubernetes 1.33-1.35 as tested, which covers kind 1.35.8 and EKS 1.35 (#30).
Prometheus runs only its server, on an `emptyDir` (Terraform installs no EBS CSI
driver, so a PVC would stay Pending), keeps 6 hours of data and scrapes the gateway
Pods of namespace `gateflow` every 10 seconds; edit the namespace in
`monitoring/prometheus-values.yaml` if the release uses another. Both run on
`gateflow.io/workload=services` nodes.

Install in this order, Prometheus and KEDA before the FlowGate chart, because the
ScaledObject kind comes from KEDA's CRDs. On EKS, with the context from #30
(substitute the versions in versions.json):

```text
helm --kubeconfig <file> --kube-context <eks> upgrade --install prometheus prometheus --repo https://prometheus-community.github.io/helm-charts --version 27.11.0 -n monitoring --create-namespace -f deploy/helm/monitoring/prometheus-values.yaml --wait
helm --kubeconfig <file> --kube-context <eks> upgrade --install keda keda --repo https://kedacore.github.io/charts --version 2.20.2 -n keda --create-namespace -f deploy/helm/monitoring/keda-values.yaml --wait
helm --kubeconfig <file> --kube-context <eks> upgrade --install gf deploy/helm/gateflow -n gateflow -f deploy/helm/values-aws.yaml --wait --wait-for-jobs
```

### kind rehearsal

After the normal rehearsal has created the cluster:

```text
python deploy/helm/rehearse.py --cluster gateflow71 --kubeconfig .local/kubeconfig --existing-cluster --autoscale-check
```

It installs the pinned Prometheus and KEDA charts, upgrades the release with
values-kind.yaml and `autoscaling.enabled=true`, and port-forwards the ingress on
127.0.0.1:18071. Instead of the smoke suite it then:

1. waits for two ready gateway replicas;
2. keeps ceil(T x 4) + 2 concurrent `mock-load` streams open (18 with T = 4; each
   about 20 seconds), using the seeded tenants' keys, until the gateway reaches four
   replicas or 300 seconds pass, and requires more than two;
3. stops the load and requires a return to two replicas within the stabilization
   window plus 180 seconds;
4. applies the load again until the gateway is above two, scales Prometheus to zero,
   stops the load, and for the same 240 seconds requires the gateway never to fall
   below its replica count at the outage; the HPA Conditions are recorded;
5. restores Prometheus and records the return to two (not required to pass).

The record, with chart versions, T, D, a 5-second series of desired and ready
replicas, HPA desired replicas, external metric values and open client streams, and
each phase's result and HPA Conditions, is written to `.local/autoscale/`. The
command exits non-zero if any required phase fails. It takes about 20 minutes and
is not part of CI, which only lints and renders the ScaledObject. The release keeps
autoscaling on afterwards; a later run without `--autoscale-check` turns it off
again and restores the fixed replica counts.
