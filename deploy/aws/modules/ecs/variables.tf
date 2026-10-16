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
  description = "Execution role for the migrate/role-init/seed-admin/staging-runner one-off tasks."
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

# --- STAGING-PREP-3: staging-runner (one-off in-VPC operations image) ---

variable "staging_runner_image" {
  description = "Full repository_url:tag of the staging-runner image (deploy/docker/staging-runner.Dockerfile), immutable tag = the full git SHA."
  type        = string
}

variable "runner_task_role_arn" {
  description = "Task role of the staging-runner definition (modules/iam): rds-db:connect as igaming_backup on this instance, nothing else. The actor-proof definition deliberately has NO task role."
  type        = string
}

variable "runner_seed_world_task_role_arn" {
  description = "Task role of the staging-runner-seed-world definition (modules/iam): Get/PutSecretValue on igaming-staging/seed-world-passwords only. No rds-db:connect."
  type        = string
}

variable "runner_task_cpu" {
  description = "CPU units of the staging-runner task (a Fargate-valid pairing with runner_task_memory; the backup/restore drill dumps and restores the staging database in the task)."
  type        = string
  default     = "1024"
}

variable "runner_task_memory" {
  type    = string
  default = "2048"
}

variable "rds_ca_bundle_url" {
  description = "Where the runner downloads the RDS CA bundle for sslmode=verify-full (the runner refuses any host other than truststore.pki.rds.amazonaws.com and logs the bundle's SHA-256)."
  type        = string
  default     = "https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem"

  validation {
    condition     = startswith(var.rds_ca_bundle_url, "https://truststore.pki.rds.amazonaws.com/")
    error_message = "rds_ca_bundle_url must be on https://truststore.pki.rds.amazonaws.com/."
  }
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

# --- STAGING-PREP-1: key families and configuration the current binary needs ---
#     Names verified against internal/staffmfa/keys.go, internal/config/config.go,
#     internal/config/observability.go and cmd/platform-api (not against docs).

variable "staff_mfa_keys_secret_arn" {
  description = "Secret with JSON keys factor_keys and mac_keys (each kid:base64). Injected as STAFF_MFA_FACTOR_KEYS / STAFF_MFA_MAC_KEYS into platform-api AND the seed-admin task: the binary refuses to start without them in every environment (staffmfa.LoadKeys)."
  type        = string
}

variable "staff_mfa_active_kid" {
  description = "STAFF_MFA_FACTOR_ACTIVE_KID and STAFF_MFA_MAC_ACTIVE_KID (not secret; must name the kid used inside the secret)."
  type        = string

  validation {
    condition     = can(regex("^[A-Za-z0-9._-]{1,32}$", var.staff_mfa_active_kid))
    error_message = "staff_mfa_active_kid must be 1-32 characters of [A-Za-z0-9._-]."
  }
}

variable "payout_instrument_keys_secret_arn" {
  description = "Secret with JSON keys master_keys and fp_keys (each kid:base64). Injected as PAYOUT_INSTRUMENT_KEYS / PAYOUT_INSTRUMENT_FP_KEYS into platform-api (withdrawals answer 503 without them)."
  type        = string
}

variable "payout_instrument_active_kid" {
  description = "PAYOUT_INSTRUMENT_ACTIVE_KID and PAYOUT_INSTRUMENT_FP_ACTIVE_KID (not secret)."
  type        = string

  validation {
    condition     = can(regex("^[A-Za-z0-9._-]{1,32}$", var.payout_instrument_active_kid))
    error_message = "payout_instrument_active_kid must be 1-32 characters of [A-Za-z0-9._-]."
  }
}

variable "actor_proof_keys_secret_arn" {
  description = "Plain-string secret (kid:base64) injected as ACTOR_PROOF_KEYS. Governed writes also need the matching actor_proof_keys database row, which is an owner action (docs/runbooks/aws-staging-deploy-plan.md G3)."
  type        = string
}

variable "actor_proof_active_kid" {
  description = "ACTOR_PROOF_ACTIVE_KID (not secret)."
  type        = string

  validation {
    condition     = can(regex("^[A-Za-z0-9._-]{1,32}$", var.actor_proof_active_kid))
    error_message = "actor_proof_active_kid must be 1-32 characters of [A-Za-z0-9._-]."
  }
}

variable "mock_geolocation_country" {
  description = "MOCK_GEOLOCATION_COUNTRY (A8): the static country the MOCK locator reports. Honoured only when test_support_endpoints_enabled is true (cmd/platform-api mockProviderWiring); the config refuses it under APP_ENV=production. Empty (default) = the variable is NOT set and the operating-market gate refuses every request (fail closed). The value is an OWNER choice, never defaulted by an agent."
  type        = string
  default     = ""

  validation {
    condition     = var.mock_geolocation_country == "" || can(regex("^[A-Z]{2}$", var.mock_geolocation_country))
    error_message = "mock_geolocation_country must be empty or an uppercase ISO-3166-1 alpha-2 code."
  }
}

variable "metrics_addr" {
  description = "METRICS_ADDR: host:port of the dedicated Prometheus listener (A9). Empty = no listener. Must differ from the HTTP port. A loopback address (127.0.0.1) needs no extra acknowledgement; there is NO scrape path from outside the task (no collector or sidecar exists), so this only proves the listener binds at startup. A non-loopback bind would also need METRICS_ALLOW_NON_LOOPBACK, which this module deliberately does not set."
  type        = string
  default     = ""

  validation {
    condition     = var.metrics_addr == "" || can(regex("^127\\.0\\.0\\.1:[0-9]{2,5}$", var.metrics_addr))
    error_message = "metrics_addr must be empty or a loopback 127.0.0.1:<port> address."
  }
}

variable "alert_webhook_url" {
  description = "ALERT_WEBHOOK_URL: https URL of an owner-controlled alert receiver. Empty (default) = no webhook channel and none of the awssm settings below are set. Treated as NON-secret everywhere (tfvars, state, plan output, DescribeTaskDefinition), so it MUST NOT embed a credential: many receiver products put a bearer token in the URL path, and such a URL must not be used. Authentication is the HMAC signature (the generated alert-webhook-hmac secret), not the URL. It is deliberately not marked sensitive: a sensitive value would hide the whole container definition from plan review."
  type        = string
  default     = ""

  validation {
    condition     = var.alert_webhook_url == "" || startswith(var.alert_webhook_url, "https://")
    error_message = "alert_webhook_url must be empty or an https:// URL."
  }
}

variable "alert_webhook_secret_ref" {
  description = "ALERT_WEBHOOK_SECRET_REF: awssm:// reference (ARN plus pinned versionId) of the webhook HMAC secret. Required when alert_webhook_url is set; the reference is not secret."
  type        = string
  default     = ""

  validation {
    condition     = var.alert_webhook_secret_ref == "" || startswith(var.alert_webhook_secret_ref, "awssm://arn:aws:secretsmanager:")
    error_message = "alert_webhook_secret_ref must be empty or an awssm://arn:aws:secretsmanager:... reference."
  }
}

variable "alert_retention_days" {
  description = "ALERT_RETENTION_DAYS: resolved-alert retention (floor 7 days in the config). 0 (default) = not set, so no purge job runs."
  type        = number
  default     = 0

  validation {
    condition     = var.alert_retention_days == 0 || var.alert_retention_days >= 7
    error_message = "alert_retention_days must be 0 (not set) or at least 7 (internal/config floor)."
  }
}

variable "tags" {
  type    = map(string)
  default = {}
}
