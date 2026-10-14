mock_provider "aws" { override_during = plan }
run "network_boundaries" {
  # Apply only to mocked in-memory state; no AWS provider operations occur.
  command = apply
  module { source = "./modules/network" }
  variables {
    name               = "gateflow-test"
    vpc_cidr           = "10.40.0.0/16"
    availability_zones = ["ap-southeast-1a", "ap-southeast-1b"]
    tags               = { Project = "gateflow" }
  }
  assert {
    condition     = length(aws_subnet.public) == 2 && length(aws_subnet.private) == 2
    error_message = "Require two public and two private subnets."
  }
  assert {
    condition     = aws_subnet.public[0].cidr_block == "10.40.0.0/24" && aws_subnet.private[1].cidr_block == "10.40.3.0/24"
    error_message = "Subnet ranges must not overlap."
  }
  assert {
    condition     = aws_subnet.public[0].tags["kubernetes.io/role/elb"] == "1" && !aws_subnet.private[0].map_public_ip_on_launch
    error_message = "ALB discovery and private node addressing must be configured."
  }
  assert {
    condition     = aws_route.private.nat_gateway_id == aws_nat_gateway.main.id
    error_message = "Private routes must use the sole NAT."
  }
}
run "invalid_azs" {
  command = plan
  module { source = "./modules/network" }
  variables {
    name               = "gateflow-test"
    availability_zones = ["ap-southeast-1a", "ap-southeast-1a"]
    tags               = {}
  }
  expect_failures = [var.availability_zones]
}
run "invalid_cidr" {
  command = plan
  module { source = "./modules/network" }
  variables {
    name               = "gateflow-test"
    availability_zones = ["ap-southeast-1a", "ap-southeast-1b"]
    vpc_cidr           = "not-a-network"
    tags               = {}
  }
  expect_failures = [var.vpc_cidr]
}
