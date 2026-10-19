mock_provider "aws" {
  mock_data "aws_ssm_parameter" {
    defaults = { insecure_value = "ami-0123456789abcdef0" }
  }
  mock_resource "aws_iam_role" {
    defaults = { arn = "arn:aws:iam::123456789012:role/test" }
  }
  mock_resource "aws_instance" {
    defaults = { private_ip = "10.40.2.15" }
  }
}
variables {
  name       = "gateflow-test"
  region     = "ap-southeast-1"
  account_id = "123456789012"
  vpc_id     = "vpc-test"
  subnet_id  = "subnet-private-a"
  tags       = { Project = "gateflow", Environment = "test" }
  client_security_group_ids = {
    services         = "sg-services"
    "load-generator" = "sg-generator"
  }
}
run "private_t4_host" {
  command = apply
  module { source = "./modules/model_host" }
  assert {
    condition     = aws_instance.model.instance_type == "g4dn.xlarge" && aws_instance.model.subnet_id == "subnet-private-a" && !aws_instance.model.associate_public_ip_address
    error_message = "One g4dn.xlarge in the private subnet, without a public IP."
  }
  assert {
    condition     = aws_instance.model.ami == "ami-0123456789abcdef0" && aws_instance.model.metadata_options[0].http_tokens == "required"
    error_message = "Use the AMI from SSM and require IMDSv2."
  }
  assert {
    condition     = aws_instance.model.root_block_device[0].encrypted && aws_instance.model.root_block_device[0].volume_size == 100 && aws_instance.model.root_block_device[0].delete_on_termination
    error_message = "An encrypted 100 GB root volume that goes with the instance."
  }
  assert {
    condition     = aws_instance.model.tags["Project"] == "gateflow" && aws_instance.model.tags["Environment"] == "test" && aws_instance.model.root_block_device[0].tags["Environment"] == "test"
    error_message = "Tag the instance and its volume so the leftover checks find them."
  }
  assert {
    condition     = output.base_url == "http://10.40.2.15:8000" && output.served_model_name == "qwen3-0.6b"
    error_message = "Expose the vLLM address and model name for the seed channel."
  }
}
run "only_cluster_nodes_reach_vllm" {
  command = apply
  module { source = "./modules/model_host" }
  assert {
    condition     = toset([for r in aws_vpc_security_group_ingress_rule.vllm : r.referenced_security_group_id]) == toset(["sg-services", "sg-generator"])
    error_message = "Admit the services and load-generator node groups, and nothing else."
  }
  assert {
    condition     = alltrue([for r in aws_vpc_security_group_ingress_rule.vllm : r.cidr_ipv4 == null && r.cidr_ipv6 == null && r.from_port == 8000 && r.to_port == 8000])
    error_message = "No CIDR ingress, and only the vLLM port."
  }
  assert {
    condition     = length(aws_iam_role_policy.api_key) == 0 && aws_iam_role_policy_attachment.ssm.policy_arn == "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
    error_message = "Session Manager only, and no parameter access without an API key."
  }
}
run "vllm_command" {
  command = plan
  module { source = "./modules/model_host" }
  assert {
    condition = alltrue([for part in [
      "vllm/vllm-openai:v0.31.0@sha256:c1c9f6fd5c109ba7f0546a59f5b2f15fb87f64c77782e90a27b648b42a8e67c3",
      "--model 'Qwen/Qwen3-0.6B' --revision 'c1899de289a04d12100db370d81485cdf75e47ca' --served-model-name 'qwen3-0.6b'",
      "--dtype half", "--max-num-seqs '256' --max-model-len '4096'", "--no-enable-prefix-caching",
      "--default-chat-template-kwargs '{\"enable_thinking\": false}'", "--publish '8000:8000'",
    ] : strcontains(aws_instance.model.user_data, part)])
    error_message = "Start the pinned vLLM image with FP16, thinking off and the agreed limits."
  }
  assert {
    condition     = !strcontains(aws_instance.model.user_data, "get-parameter") && !strcontains(aws_instance.model.user_data, "%%{")
    error_message = "Without an API key parameter the script reads no key, and every directive is rendered."
  }
}
run "optional_api_key_and_flags" {
  command = plan
  module { source = "./modules/model_host" }
  variables {
    api_key_ssm_parameter     = "/gateflow/vllm-key"
    extra_args                = ["--enforce-eager"]
    instance_type             = "g4dn.2xlarge"
    client_security_group_ids = { services = "sg-services" }
  }
  assert {
    condition     = strcontains(aws_instance.model.user_data, "--name '/gateflow/vllm-key'") && strcontains(aws_instance.model.user_data, "'--enforce-eager'")
    error_message = "Read the key from its parameter and pass the extra flags."
  }
  assert {
    condition     = jsondecode(aws_iam_role_policy.api_key[0].policy).Statement[0].Resource == "arn:aws:ssm:ap-southeast-1:123456789012:parameter/gateflow/vllm-key"
    error_message = "Allow reading that one parameter only."
  }
  assert {
    condition     = length(aws_vpc_security_group_ingress_rule.vllm) == 1 && aws_instance.model.instance_type == "g4dn.2xlarge"
    error_message = "Generator access is optional, and 2xlarge is allowed."
  }
}
run "reject_other_instance_types" {
  command = plan
  module { source = "./modules/model_host" }
  variables { instance_type = "m6i.large" }
  expect_failures = [var.instance_type]
}
run "reject_unpinned_image" {
  command = plan
  module { source = "./modules/model_host" }
  variables { vllm_image = "vllm/vllm-openai:latest" }
  expect_failures = [var.vllm_image]
}
run "reject_shell_in_extra_args" {
  command = plan
  module { source = "./modules/model_host" }
  variables { extra_args = ["--x'; rm -rf /"] }
  expect_failures = [var.extra_args]
}
run "no_clients_no_host" {
  command = plan
  module { source = "./modules/model_host" }
  variables { client_security_group_ids = {} }
  expect_failures = [var.client_security_group_ids]
}
