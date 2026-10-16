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

variable "task_secret_arns" {
  description = "Exact Secrets Manager secret ARNs the TASK role (application code, not the ECS agent) may read at run time: the platform alert webhook HMAC secret. Empty (default) = the task role has no permissions."
  type        = list(string)
  default     = []
}

variable "aws_region" {
  description = "Region of the rds-db:connect ARN of the staging-runner task role."
  type        = string
}

variable "runner_db_resource_id" {
  description = "DbiResourceId (aws_db_instance.resource_id, e.g. db-ABCDEFGHIJKL) of the staging instance: the rds-db:connect ARN names it exactly, never a wildcard."
  type        = string

  validation {
    condition     = can(regex("^db-[A-Z0-9]{10,40}$", var.runner_db_resource_id))
    error_message = "runner_db_resource_id must be an RDS DbiResourceId (db-XXXXXXXX...), never a wildcard."
  }
}

variable "seed_world_secret_arn" {
  description = "ARN of the secret igaming-staging/seed-world-passwords: the only resource the seed-world runner task role may Get/PutSecretValue."
  type        = string

  validation {
    condition     = can(regex("^arn:aws:secretsmanager:[a-z0-9-]+:[0-9]{12}:secret:[A-Za-z0-9/_+=.@-]+$", var.seed_world_secret_arn)) && !strcontains(var.seed_world_secret_arn, "*")
    error_message = "seed_world_secret_arn must be one exact Secrets Manager secret ARN (no wildcard)."
  }
}

variable "runner_db_user" {
  description = "The only database user the staging-runner task role may mint an IAM token for."
  type        = string
  default     = "igaming_backup"

  validation {
    condition     = var.runner_db_user == "igaming_backup"
    error_message = "The runner task role is for the dedicated backup role only (ADR 0122)."
  }
}

variable "permissions_boundary_arn" {
  description = "Optional IAM permissions boundary attached to every role this module creates. The staging root passes the boundary created by deploy/aws/bootstrap (ADR 0086); null = no boundary."
  type        = string
  default     = null
}

variable "tags" {
  type    = map(string)
  default = {}
}
