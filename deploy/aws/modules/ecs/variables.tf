variable "name_prefix" {
  type = string
}

variable "aws_region" {
  type = string
}

variable "private_subnet_ids" {
  type = list(string)
}

variable "ecs_security_group_id" {
  type = string
}

variable "execution_role_arn" {
  type = string
}

variable "task_role_arn" {
  type = string
}

variable "log_retention_days" {
  type    = number
  default = 14
}

variable "container_port" {
  type    = number
  default = 8080
}

# --- Images (full "repository_url:tag") ---

variable "platform_api_image" {
  type = string
}

variable "b2c_image" {
  type = string
}

variable "backoffice_image" {
  type = string
}

# --- Sizing (staging defaults; override for production) ---

variable "platform_api_cpu" {
  type    = string
  default = "256"
}
variable "platform_api_memory" {
  type    = string
  default = "512"
}
variable "platform_api_desired_count" {
  description = "At least 2, per docs/architecture/38-deployment-architecture.md's 'at least two running replicas' requirement."
  type        = number
  default     = 2
}

variable "b2c_cpu" {
  type    = string
  default = "256"
}
variable "b2c_memory" {
  type    = string
  default = "512"
}
variable "b2c_desired_count" {
  type    = number
  default = 1
}

variable "backoffice_cpu" {
  type    = string
  default = "256"
}
variable "backoffice_memory" {
  type    = string
  default = "512"
}
variable "backoffice_desired_count" {
  type    = number
  default = 1
}

variable "one_off_task_cpu" {
  description = "CPU/memory for the migrate and role-init one-off tasks."
  type        = string
  default     = "256"
}
variable "one_off_task_memory" {
  type    = string
  default = "512"
}

# --- Application configuration (non-secret) ---

variable "app_environment" {
  description = "APP_ENV value. Must be 'staging' here, never 'production' — see docs/architecture/38-deployment-architecture.md §2 point 6."
  type        = string
  default     = "staging"

  validation {
    condition     = var.app_environment != "production"
    error_message = "This module is for the staging environment; APP_ENV must never be 'production' here."
  }
}

variable "cors_allowed_origins" {
  type = string
}

variable "trusted_proxy_count" {
  description = "Exact number of proxy hops in front of platform-api. An ALB in front of ECS Fargate is 1 hop."
  type        = number
  default     = 1
}

# --- Target groups (from modules/alb) ---

variable "target_group_arns" {
  type = map(string)
}

# --- Secrets (ARNs only — never raw values) ---

variable "database_url_runtime_secret_arn" {
  type = string
}

variable "database_url_migration_secret_arn" {
  type = string
}

variable "jwt_signing_secret_arn" {
  type = string
}

variable "db_runtime_secret_arn" {
  description = "Base secret ARN (JSON {username,password}) — the ':password::' JSON-key suffix is appended inside this module for the role-init task's IGAMING_RUNTIME_PASSWORD."
  type        = string
}

variable "tags" {
  type    = map(string)
  default = {}
}
