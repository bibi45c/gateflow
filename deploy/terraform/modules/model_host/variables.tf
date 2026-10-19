variable "name" { type = string }
variable "region" { type = string }
variable "account_id" { type = string }
variable "vpc_id" { type = string }
variable "subnet_id" { type = string }
variable "tags" { type = map(string) }
variable "client_security_group_ids" {
  description = "Security groups allowed to reach the vLLM port, by name."
  type        = map(string)
  validation {
    condition     = length(var.client_security_group_ids) > 0
    error_message = "At least one client security group must reach the model host."
  }
}

variable "instance_type" {
  type    = string
  default = "g4dn.xlarge"
  validation {
    condition     = can(regex("^g4dn\\.[0-9]*x?large$", var.instance_type))
    error_message = "The model host is sized for one T4: use a g4dn instance type."
  }
}
variable "disk_gb" {
  type    = number
  default = 100
  validation {
    condition     = var.disk_gb >= 75 && floor(var.disk_gb) == var.disk_gb
    error_message = "The Deep Learning AMI and the vLLM image need at least 75 GB."
  }
}
variable "ami_ssm_parameter" {
  type    = string
  default = "/aws/service/deeplearning/ami/x86_64/base-oss-nvidia-driver-gpu-ubuntu-24.04/latest/ami-id"
}
variable "ami_id" {
  description = "Exact AMI to use instead of the SSM parameter; empty resolves the parameter."
  type        = string
  default     = ""
}

variable "vllm_image" {
  type    = string
  default = "vllm/vllm-openai:v0.31.0@sha256:c1c9f6fd5c109ba7f0546a59f5b2f15fb87f64c77782e90a27b648b42a8e67c3"
  validation {
    condition     = can(regex("@sha256:[0-9a-f]{64}$", var.vllm_image))
    error_message = "Pin the vLLM image by digest."
  }
}
variable "model" {
  type    = string
  default = "Qwen/Qwen3-0.6B"
}
variable "model_revision" {
  type    = string
  default = "c1899de289a04d12100db370d81485cdf75e47ca"
}
variable "served_model_name" {
  type    = string
  default = "qwen3-0.6b"
  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9._-]*$", var.served_model_name))
    error_message = "Use a lowercase model name of letters, digits, dots, dashes and underscores."
  }
}
variable "port" {
  type    = number
  default = 8000
}
variable "max_num_seqs" {
  type    = number
  default = 256
}
variable "max_model_len" {
  type    = number
  default = 4096
}
variable "gpu_memory_utilization" {
  type    = number
  default = 0.85
  validation {
    condition     = var.gpu_memory_utilization > 0 && var.gpu_memory_utilization < 1
    error_message = "gpu_memory_utilization is a fraction between 0 and 1."
  }
}
variable "extra_args" {
  description = "Further vLLM arguments, one list item each, such as --enforce-eager."
  type        = list(string)
  default     = []
  validation {
    condition     = alltrue([for a in var.extra_args : can(regex("^[A-Za-z0-9_.=:/,-]+$", a))])
    error_message = "Extra arguments may hold only letters, digits and _ . = : / , -"
  }
}
variable "api_key_ssm_parameter" {
  description = "SecureString parameter holding a vLLM API key; empty runs vLLM without a key, as the gateway's ollama channel needs."
  type        = string
  default     = ""
  validation {
    condition     = var.api_key_ssm_parameter == "" || can(regex("^/[A-Za-z0-9_./-]+$", var.api_key_ssm_parameter))
    error_message = "Give the parameter's full name, starting with /."
  }
}
