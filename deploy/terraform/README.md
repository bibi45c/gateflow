# S3 infrastructure

Issue [#30](https://github.com/bibi45c/gateflow/issues/30) defines this environment.
Terraform creates networking, EKS, controller IAM and private RDS PostgreSQL.
Redis, Helm workloads, KEDA and the remote state backend are excluded.
It uses only the AWS and TLS providers, not Kubernetes or Helm providers, so
validation never depends on a live cluster.
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
The estimate assumed three m6i.large service nodes. The owner's #90 review moved
to two m6i.xlarge nodes and accepted the change as within budget. Price the
revised nodes at Singapore On-Demand rates when reviewing the plan.

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
| `service_instance_type`, `service_node_count` | m6i.xlarge, 2 |
| `generator_instance_type`, `generator_node_count` | c6i.xlarge, 1 |
| `node_disk_gb` | 20 encrypted gp3 per node |
| `database_instance_class`, `database_storage_gb` | db.t4g.small, 20 encrypted gp3 |
| `database_engine_version` | PostgreSQL 17; operator confirms current supported minor |
| `database_deletion_protection` | true |
| `database_skip_final_snapshot` | false |
| `database_final_snapshot_identifier` | Required unique identifier unless skipping snapshot |
| `tags` | Additional tags; Project/Environment applied consistently |
| `gpu_model_enabled` | false; true adds the [GPU model host](#optional-gpu-model-host) |
| `gpu_model_instance_type`, `gpu_model_subnet_index` | g4dn.xlarge (only `g4dn.*`), private subnet 0 |
| `gpu_model_*` (other) | Disk, AMI, model name and vLLM limits; see [below](#optional-gpu-model-host) |

[terraform.tfvars.example](terraform.tfvars.example) uses fictional account/role
and documentation-only CIDR placeholders. bibi45c replaces them in an ignored
local tfvars file. The example explicitly selects disposable mode; use it only
after agreeing that daily database deletion is intended and evidence is exported.
Default protected mode requires an explicit final snapshot name. The deletion mode
takes effect at `apply`; see [Daily teardown](#daily-teardown-bibi45c-only). Terraform state,
plans and local tfvars/backend files must never be committed.

The VPC has two public subnets for the ALB and two private subnets for nodes/RDS.
Derived subnets are at least /27; the default /16 VPC uses /24 subnets. Smaller
subnets still need a Pod/IP-capacity review before use under load.
One NAT serves both private subnets: a cost trade-off with a single-AZ egress
failure point and possible cross-AZ charges. RDS is Single-AZ, not publicly
accessible, and accepts 5432 only from the service-node SG. The generator has its
own SG and a `gateflow.io/workload=load-generator:NoSchedule` taint.

Workload placement keeps generator and simulator compute off the relay nodes.
k6 and the mock upstream both select `gateflow.io/workload=load-generator` and
tolerate the taint, so they share the generator node. Service nodes run only the
gateway, access, metering and cluster components. #71 implements this in Helm.

## Optional GPU model host

Issue [#144](https://github.com/bibi45c/gateflow/issues/144). With
`gpu_model_enabled = true` the configuration adds one GPU instance that serves a real
model with vLLM, for the real-model tests. The default `false` adds nothing, so the
S3 plan is unchanged.

| Part | Value |
|---|---|
| Instance | `gpu_model_instance_type`, default g4dn.xlarge (1 × T4, 16 GB, 4 vCPU; g4dn.2xlarge also allowed); private subnet `gpu_model_subnet_index`, no public IP, IMDSv2 required |
| AMI | AWS Deep Learning Base OSS Nvidia Driver GPU AMI (Ubuntu 24.04), NVIDIA driver, Docker and the NVIDIA container toolkit preinstalled. Resolved from the public SSM parameter `/aws/service/deeplearning/ami/x86_64/base-oss-nvidia-driver-gpu-ubuntu-24.04/latest/ami-id` unless `gpu_model_ami_id` is set. A newer AMI later does not replace a running host (`ignore_changes`) |
| Disk | `gpu_model_disk_gb`, default 100 GB encrypted gp3, deleted with the instance |
| Server | `vllm/vllm-openai:v0.31.0` pinned by digest, started by user-data with `--restart unless-stopped`; `Qwen/Qwen3-0.6B` at a pinned revision, served as `gpu_model_served_name` (`qwen3-0.6b`) |
| vLLM flags | `--dtype half` (T4 has no BF16), thinking off (`--default-chat-template-kwargs '{"enable_thinking": false}'`), `--max-num-seqs` 256, `--max-model-len` 4096, `--gpu-memory-utilization` 0.85, `--no-enable-prefix-caching`, plus `gpu_model_extra_args` such as `--enforce-eager` |
| Network | Its own security group: TCP 8000 from the services node group, and from the load-generator node group while `gpu_model_generator_access` is true (the default; the direct baselines run there). No CIDR ingress. Egress through the NAT for the image (about 9 GB) and the model (about 1.5 GB) |
| Access | An instance role with `AmazonSSMManagedInstanceCore` only, for Session Manager. No SSH key, no port 22 |
| API key | None by default. vLLM checks only `Authorization: Bearer`, while the seeded `ollama` channel has no credential and the gateway refuses one on an `http://` base URL. A keyed vLLM therefore cannot be reached through the gateway. `gpu_model_api_key_ssm_parameter` names a SecureString for direct-only use; the role may then read that one parameter |
| Outputs | `model_host_base_url` (`http://<private ip>:8000`), `model_host_served_model`, `model_host_private_ip`, `model_host_instance_id`, `model_host_security_group_id`, `model_host_role_arn`, `model_host_ami_id`; all null when off |

Point the gateway at it with the Helm values of the
[seed channel](../helm/README.md#real-model-channel). `leftovers.py inventory` records
the instance, its security group and its role from the outputs, and the daily teardown
removes it with everything else.

Before an apply with the flag on, confirm the On-Demand G and VT vCPU quota (4 vCPU
for xlarge, 8 for 2xlarge) and that the subnet's AZ offers the type:
`aws ec2 describe-instance-type-offerings --region ap-southeast-1 --location-type availability-zone --filters Name=instance-type,Values=g4dn.xlarge`.
On 2026-10-19 the price list gave USD 0.736 per hour for g4dn.xlarge and 1.052 for
g4dn.2xlarge in Singapore, On-Demand Linux. The download through the NAT costs about
USD 0.6 once per host.

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

To run Terraform in a container with short-lived sign-in credentials and no static
keys, use the [credential bridge](credential-bridge.md); it also describes its
acceptance test.

Verify the current AWS identity outside Terraform before these commands. The
account-ID guard must match it. Review every resource, AMI/engine availability,
quotas and deletion setting in the saved plan. Never apply an unreviewed/stale plan.
Changing sizes or workload duration requires budget review, not merely more credits.

## Capacity and cost gates

The nodes total 12 On-Demand vCPUs. Two m6i.xlarge service nodes provide eight
raw vCPUs, less after system overhead: about three gateway Pods per node, plus
access, metering, monitoring and cluster components. One c6i.xlarge generator
node provides four vCPUs for k6 and the mock upstream. These may still not fit
their allocatable CPU/memory. #71 checks scheduling and requests; cloud
calibration measures actual sustained load.
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
   mode and that no evidence relies solely on the database being deleted. While
   everything still exists, save the resource inventory that step 5 checks
   against: write the output of
   `terraform -chdir=deploy/terraform output -json` to the ignored
   `.local/cloud-day/terraform-output.json`, then run

```text
python deploy/aws/leftovers.py inventory --terraform-output .local/cloud-day/terraform-output.json
```

   It records the VPC, subnets, security groups, EKS cluster, instances, volumes,
   network interfaces, NAT gateways and Elastic IPs, load balancers, target groups,
   `/aws/eks/<cluster>/` log groups, the RDS instance and the exact ARN of its
   managed master secret in `.local/cloud-day/teardown-inventory.json`. Exit code 2
   means a query failed; the file then lists it under `unchecked`.
2. While the controller/cluster are alive, remove experiment Ingress/LoadBalancer
   objects and any dynamic volumes. Wait for AWS ALB/target-group/SG/volume cleanup;
   these resources are Kubernetes-owned and may not appear in Terraform state.
3. Review a destroy plan against the correct account/state. RDS reads
   `deletion_protection`, `skip_final_snapshot` and `final_snapshot_identifier`
   from state, so the deletion mode is chosen at the morning `apply`. Changing
   them in `experiment.tfvars` just before `plan -destroy` has no effect.
   For daily teardown, use the example's disposable mode, and skip the final
   snapshot only after evidence export. Protected mode needs a date-suffixed
   snapshot name such as `gateflow-s3-final-20261017`; snapshots incur retention
   costs. Whenever these settings change, run a reviewed `plan` and `apply` with
   the new values first, then plan the destroy.
4. Operator-only commands:

```text
terraform -chdir=deploy/terraform plan -destroy -var-file=experiment.tfvars -out=destroy.tfplan
terraform -chdir=deploy/terraform apply destroy.tfplan
```

5. Check for leftovers against the saved inventory, polling while load balancers
   and network interfaces are released:

```text
python deploy/aws/leftovers.py check --inventory .local/cloud-day/teardown-inventory.json --state-bucket <state bucket> --wait 1800
```

   Exit code 0 prints `no billable leftovers found`; 1 lists each leftover with
   its type, ID, state and the reason it matched; 2 means at least one query
   failed, shown as `unchecked`, which is never the same as absent. The state
   bucket appears only as retained. The script covers what this step must confirm
   gone: EKS, node EC2 instances, RDS instances and their manual and automated
   snapshots, ALB/NLB and target groups, NAT gateways, Elastic IPs, EBS volumes
   and snapshots, unintended secrets and log groups, EKS service-generated SGs and
   NAT-related or other network interfaces. AWS-created resources may not inherit
   every tag, so it matches on the inventory's identifiers and on VPC and cluster
   association as well as on Project/Environment tags. Until it exits 0, keep the
   kubeconfig and investigate; do not report the teardown as complete.
6. Preserve the external state bucket and agreed evidence only, with explicit
   retention/cost. Reconcile billing after it updates. Escalate failed cleanup.

No teardown command is automated by CI. Code recovery is a revert; live-resource
recovery needs an operator-reviewed plan. Static/mocked checks cannot certify the
AWS plan, cost ceiling, permissions or teardown success.
