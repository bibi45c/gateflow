# S3 Terraform design - issue #30

## Goal and boundaries

Prepare reviewable Terraform for the S3 cloud experiments. The code must validate
without AWS credentials and reproduce the sizing recorded on issue #29. It must
not provision resources during development. Only bibi45c runs AWS plan/apply and
creates the remote state backend. This session writes code and commits locally;
pushing or opening a PR needs a separate instruction for this branch.

Issue #30 version: 2026-10-14T05:13:31Z; owner KaenBin. Base/rules commit:
`1e1042ebd970bf2833138a5a160ec5737fd9341c`. The user's instruction to continue
follows the proposed VPC/EKS/RDS design. This written specification is for review
before the implementation plan and resource definitions are written.

Excluded: Redis/ElastiCache, registry infrastructure, Helm installation, KEDA
installation, application secret creation, database bootstrap SQL execution,
remote backend creation, AWS plan/apply/destroy and deployment actions.

## Structure

Use a root configuration in `deploy/terraform/` and local modules `network`,
`cluster` and `database`. Use HashiCorp AWS/TLS providers rather than Kubernetes or
Helm providers, so validation never depends on a live cluster. Pin provider
versions and commit the dependency lock file. Keep provider/account configuration
at the root. CI runs only fmt, backend-free init and validate, without AWS secrets.

## Network

- Default Singapore region `ap-southeast-1`; explicit pair of AZ names provided by
  the operator, because AZ availability must be checked by bibi45c before planning.
- A new VPC with DNS resolution/hostnames, two public and two private subnets.
  Separate non-overlapping CIDRs are configurable and validated.
- Internet gateway and public routes; one NAT gateway in the first public subnet
  serves both private subnets. This follows the budget rather than claiming AZ
  resilience. Cross-AZ NAT traffic can add cost.
- Public subnets carry `kubernetes.io/role/elb=1`; private subnets carry
  `kubernetes.io/role/internal-elb=1`. Nodes have private addresses.
- RDS is not publicly accessible. Its security group accepts TCP 5432 only from
  the service node security group; the load-generator group is excluded. Application
  database roles and schema isolation remain separate controls owned by the services.

## EKS and IAM

- Default EKS 1.35; configure standard support to prevent automatic extended-support
  fees. KEDA 2.20 is documented as tested on Kubernetes 1.33-1.35. #72 pins its exact
  chart release and demonstrates compatibility on kind; #30 does not install it.
- Private API access is enabled. Public API access is limited to explicitly supplied
  operator CIDRs, with no default unrestricted range. Nodes reach the private API.
- Explicit operator IAM principal input and EKS access entry provide cluster
  administration without granting access to arbitrary identities.
- Two on-demand AL2023 amd64 managed node groups: services, fixed at three
  `m6i.large` nodes; load generator, fixed at one `c6i.xlarge`. Instance types,
  counts and disk sizes are variables. No cluster autoscaler is installed.
- Stable workload labels distinguish groups. A NoSchedule taint isolates the
  generator group; #73's generator requires a matching selector/toleration.
- Use explicit node/security groups and required EKS control-plane communication,
  including controller webhook traffic. The DB rule references the service group.
- Cluster/node IAM roles have required AWS-managed EKS policies. OIDC provider
  and a dedicated AWS Load Balancer Controller IRSA role restrict trust to
  `system:serviceaccount:kube-system:aws-load-balancer-controller` and the
  `sts.amazonaws.com` audience. Vendor a versioned official controller policy,
  record its source/version, and keep Helm installation outside Terraform.
- Outputs include cluster name, endpoint, VPC/subnets and controller role ARN for
  #71. No ALB is created directly: its Ingress and controller own its lifecycle.

## PostgreSQL and secret handling

- PostgreSQL 17 on Single-AZ `db.t4g.small`, 20 GB encrypted gp3 by default; bounded
  storage with no automatic expansion. Engine version and size remain configurable.
- Private DB subnet group spans the private subnets. Use an explicit non-superuser
  master username appropriate for RDS and database name `gateflow`.
- RDS manages its master password in Secrets Manager. Terraform outputs only the
  secret ARN, never its value. No plaintext password input, random-password resource
  or secret-value lookup is included. Secrets Manager adds a small charge not
  itemised in the earlier budget; bibi45c must include it in the final plan review.
- #71's bootstrap Job uses operator-created Kubernetes Secrets and RDS-compatible
  permissions; it must not assume the master user is a PostgreSQL superuser or use
  compose's local passwords in AWS.
- Deletion protection and final-snapshot behaviour are configurable. Example
  disposable experiment settings use no final snapshot only after the operator has
  exported evidence and explicitly selected that mode. Persistent environments use
  protection and an explicitly named final snapshot. Retained snapshots cost money.

## State, cost and operations

Declare a partial S3 backend; bibi45c supplies the existing encrypted/versioned
bucket, state key and lock configuration through an untracked backend file. No
backend bucket or credentials are created by this configuration. Backend-free init
is for validation only, not permission to use unprotected local state for deployment.
Ignore Terraform caches, state, saved plans and local variable/backend files in Git.

Apply project/environment tags via provider defaults and explicitly tag supported
resources such as managed node groups. Document indirect service-created resources
that cannot inherit every tag. Output enough identifiers for a teardown inventory.
Do not create duplicate budget actions; the account holder owns the controls in #29.

Before daily teardown, export evidence, delete Ingress/LoadBalancer resources while
controllers still run, and verify controller cleanup. bibi45c then reviews the destroy
plan for the correct state/account and destroys the experiment. Verify EC2/EKS/RDS,
NAT/EIPs, ALBs, volumes and retained snapshots/secrets for residual charges. The
external state bucket persists and is not part of experiment destroy. These are
documented future operator steps; none runs in this development session.

## Verification and recovery

Required local checks: `terraform fmt -check -recursive`,
`terraform init -backend=false -input=false`, and `terraform validate`. Add a CI
job for the same commands with pinned setup tooling and read-only permissions.
No plan/apply steps and no AWS credentials in CI. Run repository checks and record
any unavailable required Go/compose checks honestly. Validation proves schema and
configuration consistency, not AWS availability, quotas or deployability.

Self-review must trace subnet routes, node-group placement, IAM trust, DB ingress,
required operator inputs, budget sizes, backend separation and deletion options.
Before applying, bibi45c verifies account, budget, engine/AMI/add-on availability,
IAM policy compatibility and the resulting AWS plan. Code defects recover by
reverting the change; existing infrastructure requires an operator-reviewed plan,
not blind rollback or destroy. Changes to these boundaries need owner review.

## Sources

- [Issue #30](https://github.com/bibi45c/gateflow/issues/30)
- [Recorded budget](https://github.com/bibi45c/gateflow/issues/29#issuecomment-6146539645)
- [EKS versions](https://docs.aws.amazon.com/eks/latest/userguide/kubernetes-versions.html)
- [KEDA 2.20 compatibility](https://keda.sh/docs/2.20/operate/cluster/)
- [Controller IRSA installation](https://docs.aws.amazon.com/eks/latest/userguide/lbc-helm.html)
- [RDS managed password](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/rds-secrets-manager.html)
