provider "aws" {
  region              = var.region
  allowed_account_ids = [var.aws_account_id]
  default_tags { tags = local.tags }
}
locals {
  tags = merge(var.tags, { Project = "gateflow", Environment = var.environment })
}

