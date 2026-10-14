variable "name" { type = string }
variable "vpc_cidr" {
  type    = string
  default = "10.40.0.0/16"
  validation {
    condition     = can(cidrnetmask(var.vpc_cidr)) && try(tonumber(split("/", var.vpc_cidr)[1]) >= 16 && tonumber(split("/", var.vpc_cidr)[1]) <= 20, false)
    error_message = "Use an IPv4 VPC CIDR with prefix /16 through /20."
  }
}
variable "availability_zones" {
  type = list(string)
  validation {
    condition     = length(var.availability_zones) == 2 && length(distinct(var.availability_zones)) == 2
    error_message = "Specify exactly two distinct availability zones."
  }
}
variable "tags" { type = map(string) }

