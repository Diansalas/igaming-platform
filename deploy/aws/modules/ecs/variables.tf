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

variable "test_support_endpoints_enabled" {
  description = <<-EOT
    TEST_SUPPORT_ENDPOINTS_ENABLED value (Stage 9.4). The second,
    independent gate the three non-production simulation routes
    (casino play simulation, mock payment settlement, account-activation
    test support) require IN ADDITION TO app_environment != "production"
    — see internal/config/config.go's Environment/TestSupportEndpointsEnabled
    doc comments for the full two-layer, fail-closed design. Defaults to
    false at THIS module level (not per-environment) so any future
    environment that reuses this module without explicitly overriding it
    stays fully closed by default — this module's own app_environment
    variable already has a hard validation block rejecting "production",
    so THIS module can never itself produce the contradictory pairing;
    the default-false-at-module-level choice is defense in depth for a
    module that is reused or copied into a context where that validation
    no longer applies, backstopped in every case by config.Load()'s own
    hard-fail contradiction check at container startup.
  EOT
  type        = bool
  default     = false
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
