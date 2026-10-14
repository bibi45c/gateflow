# S3 infrastructure

Issue [#30](https://github.com/bibi45c/gateflow/issues/30) defines this environment.
Terraform creates networking, EKS, controller IAM and private RDS PostgreSQL.
Redis, Helm workloads, KEDA and the remote state backend are excluded.
Only **bibi45c** runs real AWS plan/apply/destroy or creates the state backend.
Local validation is not evidence of AWS deployability or a spending approval.

## Local checks (no AWS credentials)

Use Terraform 1.14.4 (tested), >=1.10,<2 (native S3 locking requires 1.10). Providers are locked in
[.terraform.lock.hcl](.terraform.lock.hcl). From the repository root:

```text
terraform -chdir=deploy/terraform fmt -check -recursive
terraform -chdir=deploy/terraform init -backend=false -input=false -lockfile=readonly
terraform -chdir=deploy/terraform validate
terraform -chdir=deploy/terraform test
```

Every test declares only mocked providers. Test plan/apply operations act on
in-memory fake resources, not AWS. No account or metadata lookup is performed.
Do not introduce real-provider tests into this CI workflow. Backend-free init
fetches signed public provider packages; it does not initialize the S3 backend.
CI job `Terraform validation` runs these same checks with read-only permissions.

## Inputs and budget

The recorded [#29 estimate](https://github.com/bibi45c/gateflow/issues/29#issuecomment-6146539645)
uses Singapore and 32 resource-hours across 17,18,19,21 October: about USD31,
about USD47 including a 50% margin. USD200 credits do not authorise extra resources.
The later #30 scope drops Redis; do not use the older table to add ElastiCache.

| Input | Default / required value |
|---|---|
| `region` | ap-southeast-1 |
| `aws_account_id` | Required; authorised 12-digit account ID, checked by provider |
| `operator_principal_arn` | Required; owner-approved IAM user/role ARN, not an STS session |
| `operator_cidrs` | Required; restricted operator IPv4 CIDRs, no empty list or /0 |
| `availability_zones` | Required; two distinct AZ names available to the account |
| `vpc_cidr` | 10.40.0.0/16; prefix /16-/20; four derived subnets |
| `environment` | s3; names use gateflow-s3 |
| `kubernetes_version` | 1.35 |
| `service_instance_type`, `service_node_count` | m6i.large, 3 |
| `generator_instance_type`, `generator_node_count` | c6i.xlarge, 1 |
| `node_disk_gb` | 20 encrypted gp3 per node |
| `database_instance_class`, `database_storage_gb` | db.t4g.small, 20 encrypted gp3 |
| `database_engine_version` | PostgreSQL 17; operator confirms current supported minor |
| `database_deletion_protection` | true |
| `database_skip_final_snapshot` | false |
| `database_final_snapshot_identifier` | Required unique identifier unless skipping snapshot |
| `tags` | Additional tags; Project/Environment applied consistently |

[terraform.tfvars.example](terraform.tfvars.example) uses fictional account/role
and documentation-only CIDR placeholders. bibi45c replaces them in an ignored
local tfvars file. The example explicitly selects disposable mode; use it only
after agreeing that daily database deletion is intended and evidence is exported.
Default protected mode requires an explicit final snapshot name. Terraform state,
plans and local tfvars/backend files must never be committed.

The VPC has two public subnets for the ALB and two private subnets for nodes/RDS.
Derived subnets are at least /27; the default /16 VPC uses /24 subnets. Smaller
subnets still need a Pod/IP-capacity review before use under load.
One NAT serves both private subnets: a cost trade-off with a single-AZ egress
failure point and possible cross-AZ charges. RDS is Single-AZ, not publicly
accessible, and accepts 5432 only from the service-node SG. The generator has its
own SG and a `gateflow.io/workload=load-generator:NoSchedule` taint. Its jobs must
select that node label and tolerate the taint; ordinary workloads select services.

## Compatibility and controller handoff

[EKS version support](https://docs.aws.amazon.com/eks/latest/userguide/kubernetes-versions.html)
currently lists 1.35 in standard support. The cluster opts into STANDARD support.
[KEDA 2.20](https://keda.sh/docs/2.20/operate/cluster/) lists 1.33-1.35 as tested.
#72 must pin its chart version and verify it; changing the EKS minor requires a
fresh compatibility check. Terraform does not install KEDA.

Controller policy: official [v3.6.0 IAM policy](https://raw.githubusercontent.com/kubernetes-sigs/aws-load-balancer-controller/v3.6.0/docs/install/iam_policy.json),
SHA256 `16f232c9d9f79366fe949c4550ad517a202380058a9e48d45a4e215044a20a6a`.
The AWS Helm chart 3.6.0 has appVersion v3.6.0. Before cloud installation, confirm
this release's support for the selected EKS minor and account permissions. #71's
operator installation uses that chart and the role ARN output, with service-account
name `aws-load-balancer-controller` in namespace `kube-system`, cluster name,
region and VPC ID explicitly supplied. Public subnet tags enable ALB discovery.
IRSA trust permits only that service-account subject and the STS audience.
No Kubernetes resources or ALBs are created by this Terraform configuration.

Root outputs expose network/subnet IDs, cluster name/endpoint, service and generator
SGs, controller role ARN, database address/port/name and master-secret ARN.
The RDS master password is generated and managed by AWS; no value is retrieved
or output by Terraform. bibi45c supplies #71's bootstrap Job with a Kubernetes
Secret, and separate access/metering passwords and internal tokens. Do not reuse
compose passwords, commit secrets or log master credentials. RDS master has
`rds_superuser` privileges, not PostgreSQL superuser; bootstrap must work with
those permissions and preserve access/accounting schema isolation.

## State and operator-only deployment

bibi45c separately creates an encrypted, versioned, private S3 bucket with native
state-lock permissions. Grant access only to authorised infrastructure operators;
state still contains sensitive operational information even without the password.
Copy [backend.hcl.example](backend.hcl.example) to ignored backend.hcl and fill in
bucket/key/region. Keep the backend outside experiment state so daily destroy
cannot delete it. No credentials belong in backend files.

After confirming the account, gate approval, budget and operator inputs, **bibi45c
only** runs from the repository root:

```text
terraform -chdir=deploy/terraform init -reconfigure -backend-config=backend.hcl
terraform -chdir=deploy/terraform plan -var-file=experiment.tfvars -out=experiment.tfplan
terraform -chdir=deploy/terraform apply experiment.tfplan
```

Verify the current AWS identity outside Terraform before these commands. The
account-ID guard must match it. Review every resource, AMI/engine availability,
quotas and deletion setting in the saved plan. Never apply an unreviewed/stale plan.
Changing sizes or workload duration requires budget review, not merely more credits.

## Capacity and cost gates

Three service nodes provide six raw vCPUs, less after system overhead. Four gateway
Pods plus services/mock/monitoring may not fit their allocatable CPU/memory. #71
checks scheduling and requests; cloud calibration measures actual sustained load.
If the workload does not fit, ask for revised sizing/workload before changing nodes.
Kind/mocked tests do not establish cloud capacity.

Price the actual plan including EBS/RDS storage, Secrets Manager, backend storage,
ALB LCUs, NAT/transfer, RDS CPU credits and any retained snapshots/logs. #29's ALB
assumption of one LCU and bundled telemetry costs are estimates, not limits. Check
real first-day usage before the next day. Provisioning/warm-up/cleanup are included
in the eight-hour windows. Account holder manages existing budget alerts/deny action;
it does not stop running resources and is not a hard spending ceiling.

## Daily teardown (bibi45c only)

1. Stop load and export redacted results, versions/configuration and necessary
   ledger evidence to separately retained storage. Confirm the deletion/snapshot
   mode and that no evidence relies solely on the database being deleted.
2. While the controller/cluster are alive, remove experiment Ingress/LoadBalancer
   objects and any dynamic volumes. Wait for AWS ALB/target-group/SG/volume cleanup;
   these resources are Kubernetes-owned and may not appear in Terraform state.
3. Review a destroy plan against the correct account/state. Default DB protection
   must be deliberately disabled through a reviewed configuration update before
   deletion. Protected-mode snapshots need a unique name and incur retention costs.
   For agreed disposable mode, skip final snapshot only after evidence export.
4. Operator-only commands:

```text
terraform -chdir=deploy/terraform plan -destroy -var-file=experiment.tfvars -out=destroy.tfplan
terraform -chdir=deploy/terraform apply destroy.tfplan
```

5. Verify EKS, node EC2 instances, RDS, ALB, NAT, EIPs, EBS volumes and unintended
   snapshots/secrets/log groups are gone. EKS service-generated SGs and NAT-related
   interfaces must also be checked. AWS-created resources may not inherit every tag;
   verify via state/output identifiers as well as Project/Environment tags.
6. Preserve the external state bucket and agreed evidence only, with explicit
   retention/cost. Reconcile billing after it updates. Escalate failed cleanup.

No teardown command is automated by CI. Code recovery is a revert; live-resource
recovery needs an operator-reviewed plan. Static/mocked checks cannot certify the
AWS plan, cost ceiling, permissions or teardown success.
