# FlowGate Helm deployment

The application chart deploys four GHCR services. PostgreSQL, existing Kubernetes
Secrets and an ingress controller must exist before installation. No AWS resources,
Redis or KEDA are created. Kubernetes >=1.33 is required for native sleep preStop.

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
KEDA and k6 Jobs belong to their separate issues.

Defaults: preStop 5 seconds, drain 10 seconds, shutdown timeout 60 seconds and
margin 10 seconds, with total Pod grace 85 seconds. All four processes receive
SHUTDOWN_DRAIN_DELAY and SHUTDOWN_TIMEOUT; readiness is /readyz and liveness is
/healthz. Native sleep works with the distroless images. Values that cannot fit the
shutdown budget are rejected at render time.
