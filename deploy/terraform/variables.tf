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

