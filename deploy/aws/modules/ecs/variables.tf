variable "name_prefix" {
  type = string
}

variable "aws_region" {
  type = string
}

variable "task_subnet_ids" {
  description = "Subnets the service tasks (and, via deploy.sh, the one-off tasks) run in. Private subnets behind NAT in the production-shaped layout; the staging root passes its public subnets together with assign_public_ip = true (ADR 0086)."
  type        = list(string)
}

variable "assign_public_ip" {
  description = "Give each task a public IPv4 address so it can reach ECR/Secrets Manager/CloudWatch Logs without a NAT Gateway. Default false (production-shaped). Inbound traffic is still restricted to the ALB security group by modules/security. The staging root sets true (ADR 0086)."
  type        = bool
  default     = false
}

variable "container_insights_enabled" {
  description = "ECS Container Insights (billed as custom CloudWatch metrics). Default true; the minimal staging root sets false (ADR 0086)."
  type        = bool
  default     = true
}

variable "ecs_security_group_id" {
  type = string
}

variable "service_execution_role_arn" {
  description = "Execution role for platform-api/b2c/backoffice (cannot read the RDS master credential)."
  type        = string
}

variable "one_off_execution_role_arn" {
  description = "Execution role for the migrate/role-init one-off tasks."
  type        = string
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
  description = "Default 2, per docs/architecture/38-deployment-architecture.md's 'at least two running replicas' production requirement. The ephemeral staging root runs 1 by default and raises it to 2 only for the multi-replica acceptance test (ADR 0086)."
  type        = number
  default     = 2

  validation {
    condition     = var.platform_api_desired_count >= 1
    error_message = "platform_api_desired_count must be at least 1."
  }
}

variable "platform_api_capacity_provider_strategy" {
  description = "Capacity provider strategy for platform-api. Default: on-demand FARGATE only."
  type = list(object({
    capacity_provider = string
    base              = number
    weight            = number
  }))
  default = [{ capacity_provider = "FARGATE", base = 0, weight = 1 }]

  validation {
    condition     = length(var.platform_api_capacity_provider_strategy) > 0 && alltrue([for s in var.platform_api_capacity_provider_strategy : contains(["FARGATE", "FARGATE_SPOT"], s.capacity_provider)]) && anytrue([for s in var.platform_api_capacity_provider_strategy : s.weight > 0])
    error_message = "Use FARGATE and/or FARGATE_SPOT, with at least one entry of weight > 0."
  }
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

variable "b2c_capacity_provider_strategy" {
  description = "Capacity provider strategy for b2c. Default: on-demand FARGATE only."
  type = list(object({
    capacity_provider = string
    base              = number
    weight            = number
  }))
  default = [{ capacity_provider = "FARGATE", base = 0, weight = 1 }]

  validation {
    condition     = length(var.b2c_capacity_provider_strategy) > 0 && alltrue([for s in var.b2c_capacity_provider_strategy : contains(["FARGATE", "FARGATE_SPOT"], s.capacity_provider)]) && anytrue([for s in var.b2c_capacity_provider_strategy : s.weight > 0])
    error_message = "Use FARGATE and/or FARGATE_SPOT, with at least one entry of weight > 0."
  }
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

variable "backoffice_capacity_provider_strategy" {
  description = "Capacity provider strategy for backoffice. Default: on-demand FARGATE only."
  type = list(object({
    capacity_provider = string
    base              = number
    weight            = number
  }))
  default = [{ capacity_provider = "FARGATE", base = 0, weight = 1 }]

  validation {
    condition     = length(var.backoffice_capacity_provider_strategy) > 0 && alltrue([for s in var.backoffice_capacity_provider_strategy : contains(["FARGATE", "FARGATE_SPOT"], s.capacity_provider)]) && anytrue([for s in var.backoffice_capacity_provider_strategy : s.weight > 0])
    error_message = "Use FARGATE and/or FARGATE_SPOT, with at least one entry of weight > 0."
  }
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
  description = "Exact number of proxy hops in front of platform-api (TRUSTED_PROXY_COUNT). An ALB alone is 1 hop; CloudFront in front of the ALB (the staging root, ADR 0086) is 2."
  type        = number
  default     = 1
}

# --- Target groups (from modules/alb) ---

variable "target_group_arns" {
  type = map(string)
}

# --- Database connection (non-secret parts only) ---

variable "database_host" {
  type = string
}

variable "database_port" {
  type = number
}

variable "database_name" {
  type = string
}

variable "db_runtime_username" {
  description = "Runtime (non-owning) Postgres role platform-api connects as."
  type        = string
}

variable "db_master_username" {
  description = "RDS master (migration-owner) role migrate/role-init connect as."
  type        = string
}

# --- Secrets (ARNs only — never raw values) ---

variable "db_master_secret_arn" {
  description = "RDS-managed master credential secret (JSON {username,password}). Injected as PGPASSWORD into migrate/role-init only."
  type        = string
}

variable "db_runtime_secret_arn" {
  description = "Runtime credential secret (JSON {username,password}). The ':password::' JSON-key suffix is appended inside this module: PGPASSWORD for platform-api, IGAMING_RUNTIME_PASSWORD for role-init."
  type        = string
}

variable "jwt_signing_secret_arn" {
  type = string
}

variable "seed_admin_password_secret_arn" {
  description = "Plain-string secret injected as SEED_ADMIN_PASSWORD into the one-off seed-admin task only."
  type        = string
}

variable "tags" {
  type    = map(string)
  default = {}
}
