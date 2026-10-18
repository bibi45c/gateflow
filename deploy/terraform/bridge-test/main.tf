# Credential bridge acceptance (see ../credential-bridge.md): one terraform apply reads the
# caller identity, waits past the lifetime of the exported credentials, then reads it again.
# Creates no AWS resource; the state stays local, in the work folder of run.sh.
terraform {
  required_providers {
    aws  = { source = "hashicorp/aws" }
    time = { source = "hashicorp/time" }
  }
}

variable "region" {
  description = "Region of the provider; the caller identity is the same in every region"
  type        = string
  default     = "ap-southeast-1"
}

variable "wait" {
  description = "Time between the two reads; longer than the credential lifetime of aws login"
  type        = string
  default     = "17m"
}

provider "aws" {
  region = var.region
}

data "aws_caller_identity" "before" {}

resource "time_sleep" "wait" {
  create_duration = var.wait
  triggers        = { account = data.aws_caller_identity.before.account_id }
}

data "aws_caller_identity" "after" {
  depends_on = [time_sleep.wait]
}

output "same_account_after_expiry" {
  value = data.aws_caller_identity.after.account_id == data.aws_caller_identity.before.account_id
}
