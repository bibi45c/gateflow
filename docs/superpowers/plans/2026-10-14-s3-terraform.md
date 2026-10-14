# S3 Terraform Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task in this session. Steps use checkbox syntax for tracking. Subagent-driven execution is an alternative only if selected by the user.

**Goal:** Implement and locally validate issue #30's budget-matched infrastructure without accessing an AWS account or provisioning resources.

**Architecture:** A Terraform root composes local network, cluster and database modules. AWS and TLS providers define infrastructure; Kubernetes/Helm installation stays outside Terraform. The account holder owns backend setup and all real AWS planning and deployment.

**Tech Stack:** Terraform >=1.9,<2; HashiCorp AWS 6.x and TLS 4.x providers, exact tested versions locked at implementation; Python 3.13 repository checks; GitHub Actions.

**Spec:** [Approved design](../specs/2026-10-14-s3-terraform-design.md).

## Global constraints

- Issue #30 version 2026-10-14T05:13:31Z; owner KaenBin. Recheck live description/comments/assignment before resuming work or changing scope.
- Branch `feat/30-s3-terraform`; base/rules `1e1042ebd970bf2833138a5a160ec5737fd9341c`. Read any newer rule changes before rebasing/committing.
- Singapore, 32 resource-hours; three m6i.large service nodes, one c6i.xlarge generator, db.t4g.small Single-AZ RDS. No Redis, ECR, Helm or KEDA installation.
- Only bibi45c executes AWS plan/apply/destroy and creates the remote backend. No credentials, account lookup, remote Terraform backend initialization or live AWS tests in this session.
- Publish nothing and open no PR without fresh instruction for #30. Commit each completed module with `Refs #30`.
- `fmt`, backend-free `init`, `validate`, and provider-mocked Terraform tests are local checks. Mock-provider test plans have no AWS calls and do not substitute for bibi45c's real plan.
- Terraform resource syntax is configuration: validation and provider-mocked tests verify it; do not add tests that merely grep HCL text.

## Review focus

1. Unrestricted administrator CIDRs: reject empty allowlists and 0.0.0.0/0 or ::/0 (Task 2).
2. Load generator reaching the DB: database ingress references only the service node group (Task 3).
3. Generator placement competing with services: distinct node labels and NoSchedule taint (Task 2).
4. Unsafe deletion choices: snapshot-required mode requires a snapshot identifier, and disposable mode is explicit (Task 3).
5. CIDR overlap or wrong subnet counts: derive four distinct subnets from a validated IPv4 VPC CIDR; require exactly two distinct AZs (Task 1).

## Task 1: Network and root configuration

**Files:** `deploy/terraform/{versions,providers,main,variables,outputs}.tf`, `deploy/terraform/modules/network/{main,variables,outputs}.tf`, `deploy/terraform/tests/network.tftest.hcl`, `.gitignore`, `deploy/terraform/.terraform.lock.hcl`.

**Interfaces:** Network inputs `name:string`, `vpc_cidr:string`, `availability_zones:list(string)`, `tags:map(string)`. Outputs `vpc_id:string`, `public_subnet_ids:list(string)`, `private_subnet_ids:list(string)`.

- [ ] Add provider-mocked tests with literal expectations: two public/two private subnets; only one NAT; both private routes reference it; public subnet role tag is `1`; `map_public_ip_on_launch=false`; `10.40.0.0/16` yields four distinct /24 subnets. Invalid one-AZ/duplicate-AZ and invalid IPv4 inputs must fail validation.
- [ ] Install a workspace-local official Terraform binary if absent, selecting one stable release >=1.9,<2 and recording its version. Download provider packages via backend-free init only. Run the new tests and establish the missing-module failure before implementation.
- [ ] Implement the module with `cidrsubnet(vpc_cidr,8,index)` indices 0,1,2,3. Require IPv4 prefix between /16 and /20. Add VPC DNS settings, IGW, public route table, one NAT/EIP and private route table. Root default VPC CIDR is `10.40.0.0/16`; AZs are required operator input.
- [ ] Add partial S3 backend, root provider default tags, required account-ID input for provider `allowed_account_ids` and operator inputs. Exclude `.terraform/`, state, plans, local tfvars/backend config and crash logs; retain the lock file and non-secret examples.
- [ ] Run fmt/init/validate plus mocked network tests. Commit `feat(infra): define budgeted S3 networking`.

## Task 2: EKS, node groups and controller IAM

**Files:** `deploy/terraform/modules/cluster/{main,iam,security,variables,outputs}.tf`, `deploy/terraform/modules/cluster/policies/load-balancer-controller.json`, `deploy/terraform/tests/cluster.tftest.hcl`; root module wiring.

**Interfaces:** Consumes network outputs; inputs `name`, `kubernetes_version` (default `1.35`), `operator_principal_arn`, `operator_cidrs`, service/generator instance types and counts, disk size, tags. Outputs `cluster_name`, `cluster_endpoint`, `service_security_group_id`, `generator_security_group_id`, `controller_role_arn`.

- [ ] Add mocked tests for private API enabled, restricted public CIDRs, version/support configuration, three service nodes and one generator node, distinct security groups, generator label `gateflow.io/workload=load-generator` and taint `gateflow.io/workload=load-generator:NO_SCHEDULE`. Test required operator inputs and the review-focus rejection cases.
- [ ] Run tests to show failures before EKS definitions exist.
- [ ] Implement EKS cluster and explicit administrator access entry/association. Use standard-support policy, AL2023 amd64 groups and fixed min/desired/max counts. Service node label is `gateflow.io/workload=services`; generator scheduling must require its matching selector/toleration.
- [ ] Use node-group launch templates with explicit security groups. Allow required node/control-plane traffic, DNS and controller/metrics webhooks; do not open the DB via VPC-wide ingress. Ensure outbound HTTPS can reach GHCR through NAT.
- [ ] Add cluster/node IAM roles, OIDC provider and dedicated controller role. Trust only the specified service-account subject and STS audience. Vendor policy from a pinned official controller release, record its source/hash and select a compatible Helm release in the README for #71's operator installation.
- [ ] Run mocked tests, fmt/validate, inspect trust JSON and node routing. Commit `feat(infra): define EKS and controller identity`.

## Task 3: Private database, outputs and state examples

**Files:** `deploy/terraform/modules/database/{main,variables,outputs}.tf`, `deploy/terraform/tests/database.tftest.hcl`, `deploy/terraform/terraform.tfvars.example`, `deploy/terraform/backend.hcl.example`; root wiring/outputs.

**Interfaces:** Consumes `vpc_id`, `private_subnet_ids`, `service_security_group_id`. Database outputs `database_address`, `database_port`, `database_name`, `master_secret_arn`; no password output. Root exposes these plus network/cluster handoff outputs.

- [ ] Add mocked tests for Single-AZ/private PostgreSQL 17, db.t4g.small, encrypted 20-GB gp3, managed master secret, only service-SG TCP5432 ingress, and the deletion/snapshot review-focus cases.
- [ ] Run tests and observe missing-resource failures before adding DB definitions.
- [ ] Implement subnet group and database SG, RDS with `manage_master_user_password=true`, username `gateflow_admin`, DB name `gateflow`, no automatic storage expansion. Default deletion protection on and snapshot required; the disposable example explicitly disables protection and skips final snapshot for daily teardown after evidence export.
- [ ] Require a non-empty `final_snapshot_identifier` whenever final snapshot is enabled. No Terraform secret-value lookup. Snapshot and secret retention implications belong in documentation.
- [ ] Create non-secret examples with placeholder account/principal/CIDRs/AZs. Partial encrypted S3 backend uses operator-supplied bucket/key/region and native state locking; no bucket resource is introduced.
- [ ] Run mocked tests, fmt/validate and inspect outputs for secret exposure. Commit `feat(infra): add private experiment PostgreSQL`.

## Task 4: Operator documentation, CI and complete verification

**Files:** `deploy/terraform/README.md`, `.github/workflows/terraform.yml`, `docs/testing.md`.

- [ ] Document module inputs/outputs, local validation commands, recorded defaults versus #29, Redis deviation and KEDA/EKS compatibility sources. Clearly mark AWS operations as bibi45c-only, and example values as placeholders.
- [ ] Document existing remote backend preparation, operator bootstrap-secret handoff to #71, RDS's non-superuser master limitations, and controller installation ownership. No application credentials in examples.
- [ ] Document first-day capacity/calibration and full cost review: six raw service vCPUs do not establish allocatable capacity; include disks, secrets, ALB LCUs, transfer and state storage. Budget alerts/deny actions do not stop existing resources.
- [ ] Document export, Kubernetes-owned ALB/volume cleanup, then operator-reviewed Terraform destroy, residual resource check and preservation of state/evidence. No automatic resource deletion script.
- [ ] Add PR/main CI with pinned setup tooling, contents-read permission, no AWS secrets, fmt/init/validate and mocked tests only. Ensure no CLI plan/apply or remote backend initialization. Add exact check commands and job names to docs/testing.md.
- [ ] Run `terraform fmt -check -recursive`, `terraform init -backend=false -input=false`, `terraform validate`, and `terraform test` with only mock providers. Expected: exit 0; each module's assertions pass. Also check lock-file checksums for the CI/local platforms.
- [ ] Run repository Python tests, workflow actionlint and required available checks. Record absent Go/Docker checks as blocked, never passed. Review all changed paths against the spec. Commit `ci(infra): validate Terraform without AWS credentials`.
- [ ] Prepare local evidence keyed to exact head/base; report drift and outstanding AWS-plan/capacity/cost acceptance. No push/PR unless authorised.

## Handoff

Plan execution needs user approval. Recommended: implement in this session using executing-plans; the module outputs make the tasks sequential. After local review, bibi45c reviews the real AWS plan, budget and operator setup. #71/#72 consume the outputs but are separate tasks. No mock test result guarantees cloud availability, quotas, fit or cost.
