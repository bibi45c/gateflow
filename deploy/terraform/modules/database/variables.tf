variable "name" { type = string }
variable "vpc_id" { type = string }
variable "private_subnet_ids" { type = list(string) }
variable "service_security_group_id" { type = string }
variable "tags" { type = map(string) }
variable "instance_class" {
  type    = string
  default = "db.t4g.small"
}
variable "engine_version" {
  type    = string
  default = "17"
}
variable "allocated_storage" {
  type    = number
  default = 20
  validation {
    condition     = var.allocated_storage >= 20 && floor(var.allocated_storage) == var.allocated_storage
    error_message = "Database storage must be at least 20 GB."
  }
}
variable "deletion_protection" {
  type    = bool
  default = true
}
variable "skip_final_snapshot" {
  type    = bool
  default = false
}
variable "final_snapshot_identifier" {
  type    = string
  default = ""
  validation {
    condition     = var.skip_final_snapshot || (can(regex("^[a-zA-Z][a-zA-Z0-9-]{0,254}$", var.final_snapshot_identifier)) && !endswith(var.final_snapshot_identifier, "-") && !strcontains(var.final_snapshot_identifier, "--"))
    error_message = "Snapshot-enabled mode requires a valid, unique final snapshot identifier."
  }
}
