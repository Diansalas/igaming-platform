variable "name_prefix" {
  type = string
}

# --- Service execution role (platform-api, b2c, backoffice) ---

variable "service_ecr_repository_arns" {
  description = "Exact ECR repository ARNs the service execution role may pull from."
  type        = list(string)
}

variable "service_secret_arns" {
  description = "Exact Secrets Manager secret ARNs the service execution role may read. Must NOT include the RDS master credential."
  type        = list(string)
}

variable "service_log_group_arns" {
  description = "Exact CloudWatch log group ARNs (already suffixed with ':*') the service execution role may write to."
  type        = list(string)
}

# --- One-off execution role (migrate, role-init) ---

variable "one_off_ecr_repository_arns" {
  description = "Exact ECR repository ARNs the one-off execution role may pull from (the platform-api image only)."
  type        = list(string)
}

variable "one_off_secret_arns" {
  description = "Exact Secrets Manager secret ARNs the one-off execution role may read (RDS master credential + runtime credential)."
  type        = list(string)
}

variable "one_off_log_group_arns" {
  description = "Exact CloudWatch log group ARNs (already suffixed with ':*') the one-off execution role may write to."
  type        = list(string)
}

variable "tags" {
  type    = map(string)
  default = {}
}
