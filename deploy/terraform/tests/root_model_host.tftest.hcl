# The whole root configuration, with mocked providers: the model host is off unless
# gpu_model_enabled is set. Apply acts on in-memory fake resources, not AWS.
mock_provider "aws" {
  mock_data "aws_ssm_parameter" {
    defaults = { insecure_value = "ami-0123456789abcdef0" }
  }
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
  mock_resource "aws_instance" {
    defaults = { private_ip = "10.40.2.15" }
  }
}
mock_provider "tls" {
  mock_data "tls_certificate" {
    defaults = { certificates = [{ sha1_fingerprint = "0123456789012345678901234567890123456789" }] }
  }
}
variables {
  aws_account_id               = "123456789012"
  operator_principal_arn       = "arn:aws:iam::123456789012:role/operator"
  operator_cidrs               = ["203.0.113.10/32"]
  availability_zones           = ["ap-southeast-1a", "ap-southeast-1b"]
  database_deletion_protection = false
  database_skip_final_snapshot = true
}
run "off_by_default" {
  command = apply
  assert {
    condition     = length(module.model_host) == 0 && output.model_host_base_url == null && output.model_host_instance_id == null
    error_message = "Without gpu_model_enabled the configuration adds no model host."
  }
}
run "on_with_the_flag" {
  command = apply
  variables { gpu_model_enabled = true }
  assert {
    condition     = length(module.model_host) == 1 && output.model_host_base_url == "http://10.40.2.15:8000" && output.model_host_served_model == "qwen3-0.6b"
    error_message = "The flag adds one model host and exposes its address."
  }
  assert {
    condition     = output.model_host_instance_id != null && output.model_host_security_group_id != null
    error_message = "Expose the identifiers that leftovers.py inventory records."
  }
}
run "subnet_index_is_bounded" {
  command = plan
  variables {
    gpu_model_enabled      = true
    gpu_model_subnet_index = 2
  }
  expect_failures = [var.gpu_model_subnet_index]
}
