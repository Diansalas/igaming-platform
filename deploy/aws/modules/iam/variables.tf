variable "name_prefix" {
  type = string
}

variable "ecr_repository_arns" {
  description = "Exact ECR repository ARNs the execution role may pull from."
  type        = list(string)
}

variable "secret_arns" {
  description = "Exact Secrets Manager secret ARNs the execution role may read (used to inject container env vars at task start)."
  type        = list(string)
}

variable "log_group_arns" {
  description = "Exact CloudWatch log group ARNs (already suffixed with ':*') the execution role may write to."
  type        = list(string)
}

variable "tags" {
  type    = map(string)
  default = {}
}
