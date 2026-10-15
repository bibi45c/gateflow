mock_provider "aws" {
  mock_resource "aws_iam_policy" {
    defaults = { arn = "arn:aws:iam::123456789012:policy/test" }
  }
  mock_resource "aws_launch_template" {
    defaults = { id = "lt-0123456789abcdef0", latest_version = 1 }
  }
  mock_resource "aws_iam_role" {
    defaults = { arn = "arn:aws:iam::123456789012:role/test" }
  }
  mock_resource "aws_eks_cluster" {
    defaults = { identity = [{ oidc = [{ issuer = "https://oidc.eks.ap-southeast-1.amazonaws.com/id/TEST" }] }] }
  }
}
mock_provider "tls" {
  mock_data "tls_certificate" {
    defaults = { certificates = [{ sha1_fingerprint = "0123456789012345678901234567890123456789" }] }
  }
}
variables {
  name                   = "gateflow-test"
  vpc_id                 = "vpc-test"
  private_subnet_ids     = ["subnet-a", "subnet-b"]
  operator_principal_arn = "arn:aws:iam::123456789012:role/operator"
  operator_cidrs         = ["203.0.113.10/32"]
  tags                   = {}
}
run "isolated_nodes" {
  command = apply
  module { source = "./modules/cluster" }
  assert {
    condition     = aws_eks_cluster.main.version == "1.35" && aws_eks_cluster.main.vpc_config[0].endpoint_private_access
    error_message = "Use the approved Kubernetes version with private API access."
  }
  assert {
    condition     = aws_eks_node_group.workload["services"].scaling_config[0].desired_size == 2 && contains(aws_eks_node_group.workload["services"].instance_types, "m6i.xlarge") && contains(aws_eks_node_group.workload["load-generator"].instance_types, "c6i.xlarge")
    error_message = "Preserve the budgeted node groups."
  }
  assert {
    condition     = aws_eks_node_group.workload["load-generator"].labels["gateflow.io/workload"] == "load-generator" && length(aws_eks_node_group.workload["load-generator"].taint) == 1
    error_message = "Keep the generator isolated."
  }
  assert {
    condition     = aws_security_group.nodes["services"].id != aws_security_group.nodes["load-generator"].id
    error_message = "Generator and services require different security groups."
  }
  assert {
    condition     = jsondecode(aws_iam_role.controller.assume_role_policy).Statement[0].Condition.StringEquals["oidc.eks.ap-southeast-1.amazonaws.com/id/TEST:sub"] == "system:serviceaccount:kube-system:aws-load-balancer-controller"
    error_message = "IRSA trust must be limited to the intended controller service account."
  }
  assert {
    condition     = aws_eks_cluster.main.upgrade_policy[0].support_type == "STANDARD"
    error_message = "Prevent automatic extended-support charges."
  }
}
run "reject_public_admin" {
  command = plan
  module { source = "./modules/cluster" }
  variables { operator_cidrs = ["0.0.0.0/0"] }
  expect_failures = [var.operator_cidrs]
}
run "reject_empty_admin" {
  command = plan
  module { source = "./modules/cluster" }
  variables { operator_cidrs = [] }
  expect_failures = [var.operator_cidrs]
}

run "reject_old_kubernetes" {
  command = plan
  module { source = "./modules/cluster" }
  variables { kubernetes_version = "1.34" }
  expect_failures = [var.kubernetes_version]
}

run "reject_empty_principal" {
  command = plan
  module { source = "./modules/cluster" }
  variables { operator_principal_arn = "" }
  expect_failures = [var.operator_principal_arn]
}
