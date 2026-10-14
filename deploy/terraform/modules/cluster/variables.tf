variable "name" { type = string }
variable "vpc_id" { type = string }
variable "private_subnet_ids" { type = list(string) }
variable "tags" { type = map(string) }
variable "kubernetes_version" {
  type    = string
  default = "1.35"
  validation {
    condition     = can(regex("^1\\.(3[5-9]|[4-9][0-9])$", var.kubernetes_version))
    error_message = "Use EKS 1.35 or later; operator must confirm standard support and KEDA compatibility."
  }
}
variable "operator_principal_arn" {
  type = string
  validation {
    condition     = can(regex("^arn:aws:iam::[0-9]{12}:(role|user)/.+$", var.operator_principal_arn))
    error_message = "Specify an operator IAM role/user ARN, not an STS session."
  }
}
variable "operator_cidrs" {
  type = list(string)
  validation {
    condition     = length(var.operator_cidrs) > 0 && alltrue([for c in var.operator_cidrs : can(cidrnetmask(c)) && try(tonumber(split("/", c)[1]) > 0, false)])
    error_message = "Specify restricted IPv4 operator CIDRs; unrestricted /0 is forbidden."
  }
}
variable "service_instance_type" {
  type    = string
  default = "m6i.large"
}
variable "service_node_count" {
  type    = number
  default = 3
  validation {
    condition     = var.service_node_count >= 1 && floor(var.service_node_count) == var.service_node_count
    error_message = "Node counts must be positive integers."
  }
}
variable "generator_instance_type" {
  type    = string
  default = "c6i.xlarge"
}
variable "generator_node_count" {
  type    = number
  default = 1
  validation {
    condition     = var.generator_node_count >= 1 && floor(var.generator_node_count) == var.generator_node_count
    error_message = "Node counts must be positive integers."
  }
}
variable "node_disk_gb" {
  type    = number
  default = 20
  validation {
    condition     = var.node_disk_gb >= 20 && floor(var.node_disk_gb) == var.node_disk_gb
    error_message = "Use at least 20 GB integer node disk size."
  }
}
