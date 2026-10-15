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
  default = "m6i.xlarge"
}
variable "service_node_count" {
  type    = number
  default = 2
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

variable "database_instance_class" {
  type    = string
  default = "db.t4g.small"
}
variable "database_storage_gb" {
  type    = number
  default = 20
}
variable "database_engine_version" {
  type    = string
  default = "17"
}
variable "database_deletion_protection" {
  type    = bool
  default = true
}
variable "database_skip_final_snapshot" {
  type    = bool
  default = false
}
variable "database_final_snapshot_identifier" {
  type    = string
  default = ""
}
