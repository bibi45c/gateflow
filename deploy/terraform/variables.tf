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

variable "gpu_model_enabled" {
  description = "Add the vLLM GPU model host for the real-model tests (issue #144)."
  type        = bool
  default     = false
}
variable "gpu_model_instance_type" {
  type    = string
  default = "g4dn.xlarge"
}
variable "gpu_model_subnet_index" {
  description = "Private subnet (0 or 1) whose AZ offers the instance type."
  type        = number
  default     = 0
  validation {
    condition     = contains([0, 1], var.gpu_model_subnet_index)
    error_message = "The VPC has two private subnets: use 0 or 1."
  }
}
variable "gpu_model_disk_gb" {
  type    = number
  default = 100
}
variable "gpu_model_ami_id" {
  description = "Exact AMI; empty resolves the latest Deep Learning Base GPU AMI (Ubuntu 24.04) from SSM."
  type        = string
  default     = ""
}
variable "gpu_model_vllm_image" {
  description = "vLLM image pinned by digest; the v0.31.0-cu129 tag is the fallback for a driver older than R580."
  type        = string
  default     = "vllm/vllm-openai:v0.31.0@sha256:c1c9f6fd5c109ba7f0546a59f5b2f15fb87f64c77782e90a27b648b42a8e67c3"
}
variable "gpu_model_served_name" {
  type    = string
  default = "qwen3-0.6b"
}
variable "gpu_model_max_num_seqs" {
  type    = number
  default = 256
}
variable "gpu_model_max_model_len" {
  type    = number
  default = 4096
}
variable "gpu_model_gpu_memory_utilization" {
  type    = number
  default = 0.85
}
variable "gpu_model_extra_args" {
  type    = list(string)
  default = []
}
variable "gpu_model_api_key_ssm_parameter" {
  description = "Optional SecureString with a vLLM key. Leave empty to reach vLLM through the gateway."
  type        = string
  default     = ""
}
variable "gpu_model_generator_access" {
  description = "Also admit the load-generator nodes, for direct baselines."
  type        = bool
  default     = true
}
