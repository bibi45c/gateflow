variable "region" {
  type    = string
  default = "ap-southeast-1"
}
variable "aws_account_id" {
  type = string
  validation {
    condition     = can(regex("^[0-9]{12}$", var.aws_account_id))
    error_message = "Specify the authorised 12-digit account ID."
  }
}
variable "environment" {
  type    = string
  default = "s3"
}
variable "tags" {
  type    = map(string)
  default = {}
}
variable "availability_zones" { type = list(string) }
variable "vpc_cidr" {
  type    = string
  default = "10.40.0.0/16"
}

variable "operator_principal_arn" { type = string }
variable "operator_cidrs" { type = list(string) }
variable "kubernetes_version" {
  type    = string
  default = "1.35"
}
variable "service_instance_type" {
  type    = string
  default = "m6i.large"
}
variable "service_node_count" {
  type    = number
  default = 3
}
variable "generator_instance_type" {
  type    = string
  default = "c6i.xlarge"
}
variable "generator_node_count" {
  type    = number
  default = 1
}
variable "node_disk_gb" {
  type    = number
  default = 20
}
